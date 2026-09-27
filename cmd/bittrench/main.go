// Command bittrench is a user-space WireGuard BitTorrent engine.
//
// It brings up a user-space WireGuard tunnel and - once the remaining
// milestones land - drives a torrent engine entirely inside it, exposing a
// Transmission-compatible JSON-RPC API and a web UI on localhost.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/moshen/bittrench/internal/config"
	"github.com/moshen/bittrench/internal/engine"
	"github.com/moshen/bittrench/internal/logging"
	"github.com/moshen/bittrench/internal/rpc"
	"github.com/moshen/bittrench/internal/server"
	"github.com/moshen/bittrench/internal/service"
	"github.com/moshen/bittrench/internal/store"
	"github.com/moshen/bittrench/internal/tunnel"
)

// shutdownTimeout bounds the whole shutdown sequence.
const shutdownTimeout = 10 * time.Second

const usage = `bittrench - user-space WireGuard BitTorrent engine

usage:
  bittrench [-config PATH]                 run in the foreground
  bittrench [-config PATH] get [-dir DIR] [-timeout DUR] TORRENT
                                               download one torrent - a magnet
                                               URI, an http(s) URL or a path to
                                               a .torrent file - then exit.
                                               Opens no host listener.
  bittrench [-config PATH] dial-through [URL]
                                               fetch URL through the tunnel and
                                               print the response (default:
                                               https://api.ipify.org, which
                                               prints the exit IP)
  bittrench [-config PATH] install [-manual] [-start-now]
                                               register the Windows service, or
                                               re-register it if it is already
                                               installed, pointing it at the
                                               running binary
  bittrench uninstall [-name N] [-no-stop] remove the Windows service
  bittrench [-config PATH] serve           run under the Windows SCM
                                               (invoked by the SCM itself)

configuration:
  -config wins when given. Without it, the first of these that exists is used:
    ./config.toml
    $XDG_CONFIG_HOME/bittrench/config.toml  (~/.config/bittrench/config.toml)
    %ProgramData%\bittrench\config.toml     (Windows only)
  The state database and the log directory default to sitting beside whichever
  file is chosen, so the location decides where the daemon keeps its state.

flags:
`

func main() {
	configPath := flag.String("config", "",
		"path to the configuration TOML file (default: the first standard location that has one)")
	flag.Usage = func() {
		fmt.Fprint(flag.CommandLine.Output(), usage)
		flag.PrintDefaults()
	}
	flag.Parse()

	absConfig, configErr := resolveConfigPath(*configPath)
	// Not finding a config is only fatal here for the commands that read one and
	// have no flags of their own. uninstall takes a service name rather than a
	// configuration, and install and get parse their own flags first so that
	// `install -h` still works on a machine with no config at all.
	switch flag.Arg(0) {
	case "uninstall", "install", "get":
	default:
		if configErr != nil {
			fatal(configErr)
		}
	}

	var err error

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd := flag.Arg(0); cmd {
	case "":
		// The SCM launches the service with the `serve` subcommand, but a
		// mis-registered service that omits it would otherwise run in the
		// foreground and be killed for never reporting Running.
		if service.IsWindowsService() {
			err = serveUnderSCM(absConfig)
			break
		}
		err = run(ctx, absConfig)
	case "serve":
		err = serveUnderSCM(absConfig)
	case "install":
		err = installService(absConfig, configErr, flag.Args()[1:])
	case "uninstall":
		err = uninstallService(flag.Args()[1:])
	case "get":
		err = get(ctx, absConfig, configErr, flag.Args()[1:])
	case "dial-through":
		url := flag.Arg(1)
		if url == "" {
			url = "https://api.ipify.org"
		}
		err = dialThrough(ctx, absConfig, url)
	default:
		flag.Usage()
		fatal(fmt.Errorf("unknown command %q", cmd))
	}
	if err != nil {
		// A subcommand's -h has already printed its own flags, and asking for
		// help is not a failure to report or to exit non-zero on.
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fatal(err)
	}
}

// resolveConfigPath settles on the configuration file to use.
//
// An explicit -config always wins, and a missing file behind it is that
// command's error to report rather than a reason to fall back on some other
// config: quietly loading a different file than the one asked for is worse than
// failing. With no flag, the standard locations are searched.
//
// The result is absolute either way. The Windows SCM launches services with the
// working directory set to C:\Windows\System32, so a relative config path, and
// with it the state database and log directory resolved beside it, would land
// somewhere wrong. filepath.Abs rather than EvalSymlinks: the latter's Windows
// output carries a \\?\ prefix that works but logs badly.
func resolveConfigPath(flagValue string) (string, error) {
	path := flagValue
	if path == "" {
		discovered, err := config.Discover()
		if err != nil {
			return "", err
		}
		path = discovered
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("couldn't resolve the config path %q: %w", path, err)
	}
	return abs, nil
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}

// session is everything a run brings up that must later be taken down, in the
// order it must be taken down in. srv is nil when the API is disabled.
type session struct {
	db         *store.Store
	completion *store.Completion
	tun        *tunnel.Tunnel
	eng        *engine.Engine
	srv        *server.Server
}

// openSession brings up the state database, the tunnel, the engine and - when
// [api] enabled is set - the one host listener. A caller that gets an error
// owns nothing: everything opened before the failure is closed again here.
func openSession(ctx context.Context, cfg *config.AppConfig, configPath string) (*session, error) {
	dbPath := cfg.StateDBFile(configPath)
	db, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}
	slog.Info("state database opened", "path", dbPath)

	// fast_resume = false keeps completion in memory only, so every restart
	// rehashes - which is exactly what turning it off means.
	var completion *store.Completion
	if cfg.Torrent.FastResume {
		if completion, err = db.NewCompletion(ctx); err != nil {
			db.Close()
			return nil, err
		}
	} else {
		completion = store.NewMemoryCompletion()
		slog.Warn("fast_resume is off: every restart will rehash every torrent")
	}

	tun, err := startTunnel(ctx, cfg)
	if err != nil {
		completion.Close()
		db.Close()
		return nil, err
	}

	eng, err := engine.New(engine.Options{Config: cfg, Net: tun, Store: db, Completion: completion})
	if err != nil {
		completion.Close()
		tun.Close()
		db.Close()
		return nil, err
	}

	s := &session{db: db, completion: completion, tun: tun, eng: eng}
	if cfg.API.Enabled {
		if s.srv, err = server.New(cfg, eng); err != nil {
			s.close()
			return nil, err
		}
	}
	return s, nil
}

// close takes the session down in the documented order, which is correctness
// and not tidiness: Client.Close() emits a final round of piece completions on
// the way down, so flushing before it loses exactly the pieces verified last -
// which looks like a fast-resume bug rather than a shutdown-ordering one. See
// AGENTS.md.
//
// The whole sequence gets a hard deadline because the Windows SCM kills a
// service whose StopPending runs long.
func (s *session) close() {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if s.srv != nil {
			// 1. stop accepting HTTP, so nothing is reading from the client
			// when it closes underneath.
			if err := s.srv.Shutdown(ctx); err != nil {
				slog.Error("failed to stop the API server", "error", err)
			}
		}
		s.eng.Close()                                // 2. peers and storage, after HTTP
		if err := s.completion.Close(); err != nil { // 3. flush, after Close
			slog.Error("failed to flush piece completion", "error", err)
		}
		if err := s.db.Close(); err != nil { // 4. WAL checkpoint
			slog.Error("failed to close the state database", "error", err)
		}
		s.tun.Close() // 5. the device last
	}()
	select {
	case <-done:
	case <-ctx.Done():
		slog.Error("shutdown timed out", "timeout", shutdownTimeout)
	}
}

// run brings the daemon up and serves until ctx is cancelled by Ctrl-C or
// SIGTERM.
func run(ctx context.Context, configPath string) error {
	cfg, err := config.Load(ctx, configPath)
	if err != nil {
		return err
	}
	writer := logging.Setup(cfg.LogDir(configPath), "bittrench", cfg.Logging.MaxAgeDays)
	if writer != nil {
		defer writer.Close()
	}
	slog.Info("configuration loaded", "config", configPath)

	s, err := openSession(ctx, &cfg, configPath)
	if err != nil {
		return err
	}
	defer s.close()

	if err := s.eng.Restore(ctx); err != nil {
		// A torrent that will not come back is worth reporting, but it is not
		// a reason to refuse to start with the others.
		slog.Error("some torrents could not be restored", "error", err)
	}

	if s.srv != nil {
		go s.srv.Serve()
		slog.Info("API and web UI listening", "addr", s.srv.Addr(),
			"rpc", "http://"+s.srv.Addr().String()+rpc.Path)
	} else {
		slog.Warn("[api] enabled = false: no Transmission RPC endpoint and no web UI; " +
			"this run can only be steered by restarting it")
	}

	slog.Info("running; press ctrl-c to stop")
	<-ctx.Done()
	slog.Info("shutting down")
	return nil
}

// getPollInterval is how often a one-shot download re-reads its progress, and
// getProgressInterval how often it says so.
const (
	getPollInterval     = time.Second
	getProgressInterval = 5 * time.Second
)

// get downloads exactly one torrent and exits.
//
// It deliberately does not restore the database's other torrents: a one-shot
// run has no business starting to seed everything the daemon knows about. The
// torrent itself is still recorded, so an interrupted `get` resumes rather than
// starting over, and the daemon proper picks it up on its next start.
func get(ctx context.Context, configPath string, configErr error, args []string) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	dir := fs.String("dir", "", "download directory (default: the [torrent] save_path)")
	timeout := fs.Duration("timeout", 0, "give up after this long, e.g. 30m (default: wait indefinitely)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("get takes exactly one torrent: a magnet URI, an http(s) URL, " +
			"or a path to a .torrent file")
	}
	// After parsing, so -h prints the flags rather than a missing config.
	if configErr != nil {
		return configErr
	}
	req, err := addRequest(fs.Arg(0), *dir)
	if err != nil {
		return err
	}

	cfg, err := config.Load(ctx, configPath)
	if err != nil {
		return err
	}
	// Nothing outlives the download, so the RPC endpoint and the web UI have
	// nobody to serve - and binding their port would fail outright whenever the
	// daemon proper already holds it. Forced rather than merely defaulted off:
	// a one-shot run must work against an unmodified config.
	cfg.API.Enabled = false

	writer := logging.Setup(cfg.LogDir(configPath), "bittrench", cfg.Logging.MaxAgeDays)
	if writer != nil {
		defer writer.Close()
	}
	slog.Info("configuration loaded", "config", configPath)

	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}

	s, err := openSession(ctx, &cfg, configPath)
	if err != nil {
		return err
	}
	defer s.close()

	id, duplicate, err := s.eng.Add(ctx, req)
	if err != nil {
		return err
	}
	if duplicate {
		// Not an error: the state database already had it, so this run picks up
		// where the last one left off.
		slog.Info("already known, resuming it", "id", id)
	}
	if err := waitForDownload(ctx, s.eng, id); err != nil {
		return err
	}
	slog.Info("download complete", "id", id)
	return nil
}

// addRequest turns the one command-line argument into an AddRequest. A magnet
// URI or an http(s) URL is handed over as a Source for the engine to fetch
// through the tunnel; anything else is read as a local .torrent file. Exactly
// one of the two fields is set, which is what the engine's spec() expects.
func addRequest(source, dir string) (engine.AddRequest, error) {
	req := engine.AddRequest{DownloadDir: dir}
	switch {
	case strings.HasPrefix(source, "magnet:"),
		strings.HasPrefix(source, "http://"),
		strings.HasPrefix(source, "https://"):
		req.Source = source
	default:
		blob, err := os.ReadFile(source)
		if err != nil {
			return req, fmt.Errorf("failed to read the torrent file %s: %w", source, err)
		}
		req.Metainfo = blob
	}
	return req, nil
}

// waitForDownload polls until every selected file is complete, the torrent
// reports an error, or ctx is cancelled.
//
// Polling rather than an event: anacrolix signals per-piece completion, which
// says nothing about the file selection that decides "done" here. Status.Complete
// is the engine's answer to that, and counts only the wanted files - an
// extension allow-list would otherwise make this wait forever.
func waitForDownload(ctx context.Context, eng *engine.Engine, id int64) error {
	ticker := time.NewTicker(getPollInterval)
	defer ticker.Stop()

	var lastLog time.Time
	for {
		st, ok := eng.Status(id)
		if !ok {
			return fmt.Errorf("torrent %d is no longer managed", id)
		}
		if st.Error != "" {
			return fmt.Errorf("torrent %d (%s) failed: %s", id, st.Name, st.Error)
		}
		if st.Complete() {
			return nil
		}
		if now := time.Now(); now.Sub(lastLog) >= getProgressInterval {
			lastLog = now
			done := st.SizeWhenDone - st.LeftUntilDone
			slog.Info("downloading", "name", st.Name, "state", st.State,
				"percent", percent(done, st.SizeWhenDone), "completed", done, "total", st.SizeWhenDone,
				"down_bytes_per_sec", int64(st.DownloadRate), "peers", st.Peers, "seeders", st.Seeders)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("gave up waiting for %s: %w", st.Name, ctx.Err())
		case <-ticker.C:
		}
	}
}

// percent reports progress, and 0 before metadata has set a total - a zero
// denominator is the normal state for a magnet's first few seconds, not an
// error.
func percent(completed, total int64) int {
	if total <= 0 {
		return 0
	}
	return int(completed * 100 / total)
}

// serveUnderSCM runs the daemon under the Windows Service Control Manager,
// which translates its Stop control into the same cancelled context Ctrl-C
// produces in the foreground.
func serveUnderSCM(configPath string) error {
	return service.Serve(service.DefaultName, func(ctx context.Context) error {
		return run(ctx, configPath)
	})
}

// installService registers the service, or re-registers it if it is already
// there.
//
// There is deliberately no -name: one machine, one daemon. Two services sharing
// this binary would share its configuration, and with it one state database and
// one API port. uninstall still takes a name, so a stray from an older version
// can be removed.
func installService(configPath string, configErr error, args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	display := fs.String("display-name", service.DefaultDisplayName, "name shown in services.msc")
	description := fs.String("description", service.DefaultDescription, "description shown in services.msc")
	manual := fs.Bool("manual", false, "start on demand rather than at boot")
	startNow := fs.Bool("start-now", false, "start the service immediately after registering it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// After parsing, so -h prints the flags rather than a missing config.
	if configErr != nil {
		return configErr
	}
	replaced, err := service.Install(service.InstallOptions{
		Name:        service.DefaultName,
		DisplayName: *display,
		Description: *description,
		AutoStart:   !*manual,
		StartNow:    *startNow,
		ConfigPath:  configPath,
	})
	if err != nil {
		return err
	}
	if replaced {
		fmt.Fprintf(os.Stderr, "warning: replaced the existing %q registration. "+
			"An update removes and recreates the service, so anything customised by "+
			"hand in services.msc (recovery actions, the log-on account) is back to "+
			"its default. A service that was running has been started again.\n",
			service.DefaultName)
	}
	fmt.Printf("installed the service %q\n", service.DefaultName)
	if exe, err := os.Executable(); err == nil {
		fmt.Printf("  exe:    %s\n", exe)
	}
	fmt.Printf("  config: %s\n", configPath)
	return nil
}

func uninstallService(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	name := fs.String("name", service.DefaultName, "service name to remove")
	noStop := fs.Bool("no-stop", false, "do not stop the service before removing it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := service.Uninstall(service.UninstallOptions{Name: *name, Stop: !*noStop}); err != nil {
		return err
	}
	fmt.Printf("removed the service %q\n", *name)
	return nil
}

// dialThrough is the tunnel smoke test: it fetches a URL entirely through the
// tunnel - resolution included - and prints the response. Pointed at an
// echo-my-IP service it prints the VPN's exit IP, which is the one check that
// proves the tunnel actually carries traffic.
func dialThrough(ctx context.Context, configPath, url string) error {
	cfg, err := config.Load(ctx, configPath)
	if err != nil {
		return err
	}
	logging.Setup(cfg.LogDir(configPath), "bittrench", cfg.Logging.MaxAgeDays)

	tun, err := startTunnel(ctx, &cfg)
	if err != nil {
		return err
	}
	defer tun.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", url, err)
	}
	resp, err := tun.HTTPClient(30 * time.Second).Do(req)
	if err != nil {
		return fmt.Errorf("fetching %s through the tunnel: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("reading the response from %s: %w", url, err)
	}
	fmt.Printf("%s %s\n%s\n", url, resp.Status, body)
	return nil
}

// startTunnel is the shared bring-up: start the device, then prove the peer
// answered before anything downstream assumes it can reach the network.
func startTunnel(ctx context.Context, cfg *config.AppConfig) (*tunnel.Tunnel, error) {
	tun, err := tunnel.Start(ctx, &cfg.WireGuard)
	if err != nil {
		return nil, err
	}
	slog.Info("WireGuard device configured",
		"endpoint", tun.Endpoint(), "client_ip", tun.ClientIP(), "mtu", tunnel.MTU)

	handshakeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := tun.WaitHandshake(handshakeCtx); err != nil {
		tun.Close()
		return nil, errors.Join(err, errors.New(
			"check wireguard.private_key, wireguard.peer_public_key and wireguard.endpoint"))
	}
	slog.Info("WireGuard handshake completed")

	servers, _ := cfg.WireGuard.DNSServers()
	slog.Info("DNS routed through the tunnel", "servers", servers)
	return tun, nil
}
