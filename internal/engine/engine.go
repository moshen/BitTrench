// Package engine drives a BitTorrent client whose every socket lives inside
// the WireGuard tunnel.
//
// The whole point of this package is what it does *not* do: it opens no host
// sockets at all. anacrolix/torrent would open a TCP listener, a uTP/UDP
// socket and a DHT server on the host stack by default; all three are
// suppressed and replaced with tunnel-backed equivalents. Read AGENTS.md
// before changing anything here.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/dialer"
	"github.com/anacrolix/torrent/storage"
	"golang.org/x/time/rate"

	"github.com/moshen/bittrench/internal/config"
	"github.com/moshen/bittrench/internal/store"
)

const (
	// dhtBootstrapTimeout bounds resolving the bootstrap host list in-tunnel.
	dhtBootstrapTimeout = 30 * time.Second
	// trackerHTTPTimeout applies to HTTP(S) tracker announces and webseeds.
	trackerHTTPTimeout = 60 * time.Second
)

// Net is the tunnel surface the engine is allowed to use.
//
// It is an interface for one reason beyond testing: it is the complete list of
// ways this package can reach a network. Anything the engine needs that is not
// here would have to come from the host stack, so adding a method is a
// decision to be made deliberately, against the leak audit.
type Net interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
	LookupHost(ctx context.Context, host string) ([]string, error)
	ListenUDP(port uint16) (net.PacketConn, error)
	ListenTCP(port uint16) (net.Listener, error)
	HTTPClient(timeout time.Duration) *http.Client
	ClientIP() netip.Addr
}

// Engine owns the torrent client, the tunnel-backed sockets it runs on, and
// every piece of per-torrent state anacrolix/torrent does not model.
type Engine struct {
	client   *torrent.Client
	dht      *dht.Server
	listener net.Listener
	trackers *trackerResolver
	net      Net

	cfg               *config.AppConfig
	store             *store.Store
	completion        *store.Completion
	allowedExtensions map[string]struct{}
	maxPeers          int

	mu      sync.RWMutex
	records map[int64]*record

	sampler *sampler
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// Options are the collaborators the engine needs.
type Options struct {
	Config     *config.AppConfig
	Net        Net
	Store      *store.Store
	Completion *store.Completion
}

// New builds the client and starts the sampler and the seed monitor. Restore
// is a separate call so the caller decides when persisted torrents come back.
func New(opts Options) (*Engine, error) {
	cfg, tnet := opts.Config, opts.Net
	trackers := newTrackerResolver(tnet.LookupHost)

	cc, err := clientConfig(cfg, tnet)
	if err != nil {
		return nil, err
	}
	// The daemon's own completion store, not one of the upstream backends:
	// the SQLite one is cgo-only and the bbolt fallback is a second database
	// keyed on a directory, which per-torrent download-dirs would fragment.
	cc.DefaultStorage = storage.NewFileWithCompletion(cfg.Torrent.SavePath, opts.Completion)

	client, err := torrent.NewClient(cc)
	if err != nil {
		return nil, fmt.Errorf("failed to create the torrent client: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{
		client: client, trackers: trackers, net: tnet,
		cfg: cfg, store: opts.Store, completion: opts.Completion,
		allowedExtensions: normaliseExtensions(cfg.Torrent.AllowedExtensions),
		maxPeers:          cc.EstablishedConnsPerTorrent,
		records:           make(map[int64]*record),
		sampler:           newSampler(),
		ctx:               ctx, cancel: cancel,
	}
	if err := e.attachTunnelSockets(cfg, tnet); err != nil {
		e.Close()
		return nil, err
	}

	// Belt and braces on the whole point of the package: if anacrolix opened
	// anything on the host stack despite the config above, say so loudly
	// rather than torrenting outside the tunnel.
	if addrs := hostListenAddrs(client, e.listener); len(addrs) > 0 {
		e.Close()
		return nil, fmt.Errorf("refusing to run: the torrent client opened host sockets: %v", addrs)
	}

	e.wg.Add(2)
	go e.runSampler()
	go e.runMonitor()

	slog.Info("torrent engine started",
		"peer_id", cc.PeerID, "client", cc.ExtendedHandshakeClientVersion,
		"dht", e.dht != nil, "listening_in_tunnel", e.listener != nil,
		"pex", !cc.DisablePEX, "seeding", cc.Seed,
		"allowed_extensions", len(e.allowedExtensions))
	return e, nil
}

// clientConfig builds the ClientConfig, which is where most of the leak
// prevention lives.
func clientConfig(cfg *config.AppConfig, tnet Net) (*torrent.ClientConfig, error) {
	cc := torrent.NewDefaultClientConfig()

	// Suppress every host socket. DisableTCP skips the host TCP listener and
	// DisableUTP + NoDHT skip the host UDP socket (client.go:481,484); with
	// no networks requested, NewClient's "no sockets created" check does not
	// fire. These flags do *not* gate cl.dialers, which is why the tunnel
	// dialer added afterwards still works.
	cc.DisableTCP = true
	cc.DisableUTP = true
	cc.NoDHT = true
	// UPnP would have to speak to the provider's gateway through the tunnel,
	// which anacrolix/upnp will not do, and the id it announces is built from
	// this module's path. Off, always - `disable_upnp_port_forward` stays the
	// inert compatibility key it has always been.
	cc.NoDefaultPortForwarding = true

	// Local Service Discovery multicasts infohashes on the *host* LAN, which
	// would announce every torrent outside the tunnel. anacrolix/torrent has no
	// LSD at all, so there is nothing to switch off. It is noted here so nobody
	// "adds the missing feature" later.

	prefix, err := cfg.WireGuard.ClientPrefix()
	if err != nil {
		return nil, err
	}
	// Pin the address family to the tunnel's, so a v6 peer address can never
	// be dialled over a v4-only tunnel.
	cc.DisableIPv6 = !prefix.Addr().Is6()
	cc.DisableIPv4 = prefix.Addr().Is6()
	// Advertise the tunnel address, never a host one.
	if prefix.Addr().Is4() {
		cc.PublicIp4 = prefix.Addr().AsSlice()
	} else {
		cc.PublicIp6 = prefix.Addr().AsSlice()
	}

	// Seed defaults to false, and Torrent.seeding() (torrent.go:2070) consults
	// it, so leaving it unset means never uploading a byte after completing -
	// which would also mean the seed ratio and time caps could never fire,
	// because the ratio would never rise.
	cc.Seed = true

	// PEX only trades peer lists with peers already reached through the
	// tunnel, so it is not a leak, but it widens who learns we are in a swarm.
	cc.DisablePEX = !cfg.Torrent.EnablePex

	cc.DataDir = cfg.Torrent.SavePath

	// Every tracker path that has a hook gets one. The one that does not -
	// UDP announce hostnames - is handled by rewriting the URL; see trackers.go.
	cc.TrackerDialContext = tnet.DialContext
	cc.TrackerListenPacket = func(string, string) (net.PacketConn, error) {
		return tnet.ListenUDP(0)
	}
	// The DHT bootstrap host list is resolved in-tunnel. Without this
	// override the default resolves all eight hostnames through the host
	// resolver on startup; see dht.go.
	cc.DhtStartingNodes = func(string) dht.StartingNodesGetter {
		return tunnelStartingNodes(tnet.LookupHost)
	}

	cc.HTTPDialContext = tnet.DialContext
	cc.WebTransport = tnet.HTTPClient(trackerHTTPTimeout).Transport
	cc.MetainfoSourcesClient = tnet.HTTPClient(trackerHTTPTimeout)

	if err := applyFingerprint(cc); err != nil {
		return nil, err
	}
	applyLimits(cc, &cfg.Torrent)

	cc.AcceptPeerConnections = cfg.Torrent.ListenInTunnel
	return cc, nil
}

// applyFingerprint replaces every identity string anacrolix/torrent derives
// from the build info. See fingerprint.go for why this is mandatory.
func applyFingerprint(cc *torrent.ClientConfig) error {
	peerID, err := newPeerID()
	if err != nil {
		return err
	}
	cc.PeerID = peerID
	cc.Bep20 = mimicPeerIDPrefix
	cc.ExtendedHandshakeClientVersion = mimicClient
	cc.HTTPUserAgent = mimicClient
	// Unreachable while NoDefaultPortForwarding is set, but set anyway so the
	// invariant survives someone re-enabling UPnP.
	cc.UpnpID = mimicClient
	return nil
}

// applyLimits maps the [torrent] and [torrent.limits] config onto the client.
func applyLimits(cc *torrent.ClientConfig, cfg *config.TorrentConfig) {
	if cfg.PeerConnectTimeoutSecs > 0 {
		nominal := time.Duration(cfg.PeerConnectTimeoutSecs) * time.Second
		// reducedDialTimeout (client.go:720) divides Nominal by the pending
		// peer load and floors the result at MinDialTimeout, so Nominal is
		// the ceiling the config key means - and the 3s default floor would
		// silently override a config of, say, 2 seconds.
		cc.NominalDialTimeout = nominal
		cc.MinDialTimeout = min(3*time.Second, nominal)
	}
	if n := cfg.Limits.MaxPeersPerTorrent; n != nil && *n > 0 {
		// The key says per-torrent and the library's knob is per-torrent, so
		// the two line up.
		cc.EstablishedConnsPerTorrent = *n
	}
	// Bursts are left at zero on purpose: the limiter's burst must exceed the
	// ~16 KiB chunk size or transfers stall, and NewClient fills a suitable
	// one in (config.go:276) when it sees a zero.
	if bps := rateLimit(cfg.Limits.DownloadRateLimitKbps); bps != nil {
		cc.DownloadRateLimiter = bps
	}
	if bps := rateLimit(cfg.Limits.UploadRateLimitKbps); bps != nil {
		cc.UploadRateLimiter = bps
	}
}

// rateLimit converts a kbps config value into a limiter, or nil for unlimited.
func rateLimit(kbps uint32) *rate.Limiter {
	if kbps == 0 {
		return nil
	}
	return rate.NewLimiter(rate.Limit(float64(kbps)*1024), 0)
}

// attachTunnelSockets adds back, inside the tunnel, everything the config
// above suppressed on the host.
func (e *Engine) attachTunnelSockets(cfg *config.AppConfig, tnet Net) error {
	// Outbound peer connections. dialer.WithNetwork adapts our DialContext to
	// the dialer.T the client wants, with the network locked to the tunnel's
	// family.
	network := "tcp4"
	if prefix, err := cfg.WireGuard.ClientPrefix(); err == nil && prefix.Addr().Is6() {
		network = "tcp6"
	}
	e.client.AddDialer(dialer.WithNetwork{Network: network, Dialer: tnet})

	if cfg.Torrent.EnableDHT {
		conn, err := tnet.ListenUDP(0)
		if err != nil {
			return fmt.Errorf("failed to open the in-tunnel DHT socket: %w", err)
		}
		server, err := e.client.NewAnacrolixDhtServer(conn)
		if err != nil {
			conn.Close()
			return fmt.Errorf("failed to start the DHT server: %w", err)
		}
		e.dht = server
		e.client.AddDhtServer(torrent.AnacrolixDhtServerWrapper{Server: server})
	}

	if cfg.Torrent.ListenInTunnel {
		listener, err := tnet.ListenTCP(cfg.Torrent.ListenPort)
		if err != nil {
			return err
		}
		e.listener = listener
		e.client.AddListener(listener)
	}
	return nil
}

// hostListenAddrs returns any address the client is listening on that is not
// the in-tunnel listener we added ourselves.
func hostListenAddrs(client *torrent.Client, ours net.Listener) []string {
	var out []string
	for _, addr := range client.ListenAddrs() {
		if ours != nil && addr.String() == ours.Addr().String() {
			continue
		}
		out = append(out, addr.String())
	}
	return out
}

// Client exposes the underlying client to the layers built on top of it.
func (e *Engine) Client() *torrent.Client { return e.client }

// Trackers exposes the announce-URL rewriter, which every add path must run
// its tracker list through.
func (e *Engine) Trackers() *trackerResolver { return e.trackers }

// Close shuts the client down. Per the documented shutdown order, callers stop
// serving HTTP first and flush persistence *after* this returns - the client's
// last piece completions are recorded on the way down, and flushing before
// that loses exactly the pieces verified last.
func (e *Engine) Close() {
	if e.cancel != nil {
		e.cancel()
	}
	if e.client != nil {
		for _, err := range e.client.Close() {
			slog.Warn("error closing the torrent client", "error", err)
		}
	}
	e.wg.Wait()
	if e.dht != nil {
		e.dht.Close()
	}
	if e.listener != nil {
		e.listener.Close()
	}
}
