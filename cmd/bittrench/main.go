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
  bittrench [-config PATH] dial-through [URL]
                                               fetch URL through the tunnel and
                                               print the response (default:
                                               https://api.ipify.org, which
                                               prints the exit IP)
  bittrench [-config PATH] install [-name N] [-manual] [-start-now]
                                               register a Windows service
  bittrench uninstall [-name N] [-no-stop] remove the Windows service
  bittrench [-config PATH] serve           run under the Windows SCM
                                               (invoked by the SCM itself)

flags:
`

func main() {
	configPath := flag.String("config", "config.toml", "path to the configuration TOML file")
	flag.Usage = func() {
		fmt.Fprint(flag.CommandLine.Output(), usage)
		flag.PrintDefaults()
	}
	flag.Parse()

	// The Windows SCM launches services with the working directory set to
	// C:\Windows\System32, so a relative config path - and with it the state
	// database and log directory that are resolved beside it - would land
	// somewhere wrong. Absolute from here on. filepath.Abs rather than
	// EvalSymlinks: the latter's Windows output carries a \\?\ prefix that
	// works but logs badly.
	absConfig, err := filepath.Abs(*configPath)
	if err != nil {
		fatal(fmt.Errorf("couldn't resolve the config path %q: %w", *configPath, err))
	}

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
		err = installService(absConfig, flag.Args()[1:])
	case "uninstall":
		err = uninstallService(flag.Args()[1:])
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
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
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

	dbPath := cfg.StateDBFile(configPath)
	db, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	slog.Info("state database opened", "path", dbPath)

	// fast_resume = false keeps completion in memory only, so every restart
	// rehashes - which is exactly what turning it off means.
	var completion *store.Completion
	if cfg.Torrent.FastResume {
		if completion, err = db.NewCompletion(ctx); err != nil {
			return err
		}
	} else {
		completion = store.NewMemoryCompletion()
		slog.Warn("fast_resume is off: every restart will rehash every torrent")
	}

	tun, err := startTunnel(ctx, &cfg)
	if err != nil {
		completion.Close()
		return err
	}

	eng, err := engine.New(engine.Options{Config: &cfg, Net: tun, Store: db, Completion: completion})
	if err != nil {
		completion.Close()
		tun.Close()
		return err
	}
	if err := eng.Restore(ctx); err != nil {
		// A torrent that will not come back is worth reporting, but it is not
		// a reason to refuse to start with the others.
		slog.Error("some torrents could not be restored", "error", err)
	}

	if !cfg.Torrent.DisableUPnPPortForward {
		slog.Warn("disable_upnp_port_forward = false has no effect: " +
			"UPnP would have to reach the provider's gateway through the tunnel, which is not supported")
	}

	srv, err := server.New(&cfg, eng)
	if err != nil {
		eng.Close()
		completion.Close()
		tun.Close()
		return err
	}
	go srv.Serve()
	slog.Info("API and web UI listening", "addr", srv.Addr(),
		"rpc", "http://"+srv.Addr().String()+rpc.Path)

	slog.Info("running; press ctrl-c to stop")
	<-ctx.Done()

	// The shutdown order is documented in AGENTS.md and is correctness-critical,
	// not tidiness: Client.Close() emits a final round of piece completions on
	// the way down, so flushing before it loses exactly the pieces verified
	// last - which looks like a fast-resume bug rather than a shutdown-ordering
	// one.
	slog.Info("shutting down")
	shutdown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// 1. stop accepting HTTP, so nothing is reading from the client when
		// it closes underneath.
		if err := srv.Shutdown(shutdown); err != nil {
			slog.Error("failed to stop the API server", "error", err)
		}
		eng.Close()                                // 2. peers and storage, after HTTP
		if err := completion.Close(); err != nil { // 3. flush, after Close
			slog.Error("failed to flush piece completion", "error", err)
		}
		if err := db.Close(); err != nil { // 4. WAL checkpoint
			slog.Error("failed to close the state database", "error", err)
		}
		tun.Close() // 5. the device last
	}()
	select {
	case <-done:
	case <-shutdown.Done():
		// The Windows SCM kills a service whose StopPending runs long, so the
		// sequence gets a hard deadline rather than hanging on a stuck peer.
		slog.Error("shutdown timed out", "timeout", shutdownTimeout)
	}
	return nil
}

// serveUnderSCM runs the daemon under the Windows Service Control Manager,
// which translates its Stop control into the same cancelled context Ctrl-C
// produces in the foreground.
func serveUnderSCM(configPath string) error {
	return service.Serve(service.DefaultName, func(ctx context.Context) error {
		return run(ctx, configPath)
	})
}

func installService(configPath string, args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	name := fs.String("name", service.DefaultName, "service name to register under")
	display := fs.String("display-name", service.DefaultDisplayName, "name shown in services.msc")
	description := fs.String("description", service.DefaultDescription, "description shown in services.msc")
	manual := fs.Bool("manual", false, "start on demand rather than at boot")
	startNow := fs.Bool("start-now", false, "start the service immediately after registering it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := service.Install(service.InstallOptions{
		Name:        *name,
		DisplayName: *display,
		Description: *description,
		AutoStart:   !*manual,
		StartNow:    *startNow,
		ConfigPath:  configPath,
	}); err != nil {
		return err
	}
	fmt.Printf("installed the service %q with -config %s\n", *name, configPath)
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
