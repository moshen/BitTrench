package engine

import (
	"context"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/torrent/version"

	"github.com/moshen/bittrench/internal/config"
	"github.com/moshen/bittrench/internal/store"
	"github.com/moshen/bittrench/internal/tunneltest"
)

// engineConfig is a valid config pointed at a scratch save path.
func engineConfig(t *testing.T, clientAddr, dnsAddr netip.Addr) *config.AppConfig {
	t.Helper()
	cfg := config.Defaults()
	cfg.WireGuard = config.WireGuardConfig{
		PrivateKey:    "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		PeerPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		Endpoint:      "198.51.100.5:51820",
		ClientIP:      clientAddr.String() + "/32",
		DNS:           dnsAddr.String(),
	}
	cfg.Torrent.SavePath = t.TempDir()
	cfg.Torrent.EnableDHT = false
	return &cfg
}

// newEngineForConfig builds an engine with a throwaway state database.
func newEngineForConfig(t *testing.T, cfg *config.AppConfig, tnet Net) *Engine {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	completion, err := s.NewCompletion(context.Background())
	if err != nil {
		t.Fatalf("NewCompletion: %v", err)
	}
	e, err := New(Options{Config: cfg, Net: tnet, Store: s, Completion: completion})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Close(); completion.Close(); s.Close() })
	return e
}

// The whole purpose of the package: no socket on the host stack. If
// anacrolix/torrent ever stops honouring DisableTCP/DisableUTP/NoDHT, this is
// what catches it.
func TestEngineOpensNoHostSockets(t *testing.T) {
	p := tunneltest.StartPeer(t, netip.MustParseAddr("10.10.0.1"))
	clientAddr := netip.MustParseAddr("10.10.0.2")
	tun := tunneltest.StartTunnel(t, p, clientAddr)
	tunneltest.ServeDNS(t, p, nil, "10.10.0.9")

	cfg := engineConfig(t, clientAddr, p.Addr)
	e := newEngineForConfig(t, cfg, tun)

	if addrs := e.Client().ListenAddrs(); len(addrs) != 0 {
		t.Errorf("the client is listening on %v; it must open no host sockets", addrs)
	}
	if ls := e.Client().Listeners(); len(ls) != 0 {
		t.Errorf("the client registered %d listeners; expected none", len(ls))
	}
}

// With listen_in_tunnel the only listener must be the in-tunnel one, bound to
// the tunnel address - never a host address.
func TestListenInTunnelBindsTheTunnelAddressOnly(t *testing.T) {
	p := tunneltest.StartPeer(t, netip.MustParseAddr("10.10.1.1"))
	clientAddr := netip.MustParseAddr("10.10.1.2")
	tun := tunneltest.StartTunnel(t, p, clientAddr)
	tunneltest.ServeDNS(t, p, nil, "10.10.1.9")

	cfg := engineConfig(t, clientAddr, p.Addr)
	cfg.Torrent.ListenInTunnel = true
	cfg.Torrent.ListenPort = 6881
	e := newEngineForConfig(t, cfg, tun)

	addrs := e.Client().ListenAddrs()
	if len(addrs) != 1 {
		t.Fatalf("expected exactly one listener, got %v", addrs)
	}
	want := netip.AddrPortFrom(clientAddr, 6881).String()
	if addrs[0].String() != want {
		t.Errorf("listening on %s, want the tunnel address %s", addrs[0], want)
	}
}

// The library default broadcasts this module's path and version to every peer.
// Every string derived from build info must be replaced.
func TestFingerprintReplacesEveryAnacrolixDefault(t *testing.T) {
	p := tunneltest.StartPeer(t, netip.MustParseAddr("10.10.2.1"))
	clientAddr := netip.MustParseAddr("10.10.2.2")
	tun := tunneltest.StartTunnel(t, p, clientAddr)

	cc, err := clientConfig(engineConfig(t, clientAddr, p.Addr), tun)
	if err != nil {
		t.Fatalf("clientConfig: %v", err)
	}

	for _, c := range []struct{ name, got, def string }{
		{"ExtendedHandshakeClientVersion", cc.ExtendedHandshakeClientVersion, version.DefaultExtendedHandshakeClientVersion},
		{"HTTPUserAgent", cc.HTTPUserAgent, version.DefaultHttpUserAgent},
		{"Bep20", cc.Bep20, version.DefaultBep20Prefix},
		{"UpnpID", cc.UpnpID, version.DefaultUpnpId},
	} {
		if c.got == c.def {
			t.Errorf("%s is still the anacrolix default %q", c.name, c.def)
		}
		if c.got == "" {
			t.Errorf("%s is empty", c.name)
		}
	}
	// The module path must not reach the wire under any of these.
	for _, s := range []string{cc.ExtendedHandshakeClientVersion, cc.HTTPUserAgent, cc.Bep20, cc.UpnpID, cc.PeerID} {
		if strings.Contains(s, "bittrench") || strings.Contains(s, "anacrolix") {
			t.Errorf("%q leaks the module or library identity", s)
		}
	}
}

// anacrolix fills the peer id tail with raw crypto/rand bytes when given only
// a Bep20 prefix. Real clients use printable characters, so a raw tail is
// itself a fingerprint.
func TestPeerIDIsPrefixedAndPrintable(t *testing.T) {
	seen := make(map[string]bool)
	for range 64 {
		id, err := newPeerID()
		if err != nil {
			t.Fatalf("newPeerID: %v", err)
		}
		if len(id) != peerIDLength {
			t.Fatalf("peer id %q is %d bytes, want %d", id, len(id), peerIDLength)
		}
		if !strings.HasPrefix(id, mimicPeerIDPrefix) {
			t.Fatalf("peer id %q lacks the prefix %q", id, mimicPeerIDPrefix)
		}
		for _, c := range id[len(mimicPeerIDPrefix):] {
			if !strings.ContainsRune(peerIDTailAlphabet, c) {
				t.Fatalf("peer id %q has a non-alphanumeric tail byte %q", id, c)
			}
		}
		seen[id] = true
	}
	if len(seen) < 60 {
		t.Errorf("peer ids repeat: %d unique out of 64", len(seen))
	}
}

// Seed defaults to false, which silently disables all uploading - and with it
// the seed ratio and time caps, because the ratio can never rise.
func TestSeedingIsEnabled(t *testing.T) {
	p := tunneltest.StartPeer(t, netip.MustParseAddr("10.10.3.1"))
	clientAddr := netip.MustParseAddr("10.10.3.2")
	tun := tunneltest.StartTunnel(t, p, clientAddr)

	cc, err := clientConfig(engineConfig(t, clientAddr, p.Addr), tun)
	if err != nil {
		t.Fatalf("clientConfig: %v", err)
	}
	if !cc.Seed {
		t.Error("cfg.Seed is false, so the daemon would never upload a byte")
	}
	if cc.NoUpload {
		t.Error("NoUpload is set")
	}
	if !cc.NoDefaultPortForwarding {
		t.Error("UPnP port forwarding is enabled")
	}
	if !cc.DisableTCP || !cc.DisableUTP || !cc.NoDHT {
		t.Error("a host socket path is left enabled")
	}
}

func TestPexFollowsTheConfig(t *testing.T) {
	p := tunneltest.StartPeer(t, netip.MustParseAddr("10.10.4.1"))
	clientAddr := netip.MustParseAddr("10.10.4.2")
	tun := tunneltest.StartTunnel(t, p, clientAddr)

	cfg := engineConfig(t, clientAddr, p.Addr)
	cc, err := clientConfig(cfg, tun)
	if err != nil {
		t.Fatalf("clientConfig: %v", err)
	}
	if !cc.DisablePEX {
		t.Error("PEX should be off by default")
	}

	cfg.Torrent.EnablePex = true
	cc, err = clientConfig(cfg, tun)
	if err != nil {
		t.Fatalf("clientConfig: %v", err)
	}
	if cc.DisablePEX {
		t.Error("enable_pex = true should turn PEX on")
	}
}

// peer_connect_timeout_secs is the ceiling, and anacrolix's 3s MinDialTimeout
// floor would otherwise silently override a shorter config.
func TestDialTimeoutsFollowTheConfig(t *testing.T) {
	p := tunneltest.StartPeer(t, netip.MustParseAddr("10.10.5.1"))
	clientAddr := netip.MustParseAddr("10.10.5.2")
	tun := tunneltest.StartTunnel(t, p, clientAddr)

	cfg := engineConfig(t, clientAddr, p.Addr)
	cfg.Torrent.PeerConnectTimeoutSecs = 2
	cc, err := clientConfig(cfg, tun)
	if err != nil {
		t.Fatalf("clientConfig: %v", err)
	}
	if cc.NominalDialTimeout != 2*time.Second {
		t.Errorf("NominalDialTimeout = %v, want 2s", cc.NominalDialTimeout)
	}
	if cc.MinDialTimeout > cc.NominalDialTimeout {
		t.Errorf("MinDialTimeout %v exceeds the configured ceiling %v", cc.MinDialTimeout, cc.NominalDialTimeout)
	}
}

// The DHT bootstrap list must be resolved in-tunnel. The library's default
// resolves all eight hostnames through the host resolver, which is exactly the
// leak this daemon exists to close.
func TestDHTBootstrapResolvesInTunnel(t *testing.T) {
	p := tunneltest.StartPeer(t, netip.MustParseAddr("10.10.6.1"))
	clientAddr := netip.MustParseAddr("10.10.6.2")
	tun := tunneltest.StartTunnel(t, p, clientAddr)
	probe := tunneltest.ServeDNS(t, p, nil, "10.10.6.77")

	addrs, err := tunnelStartingNodes(tun.LookupHost)()
	if err != nil {
		t.Fatalf("bootstrap through the tunnel: %v", err)
	}
	if len(addrs) == 0 {
		t.Fatal("no bootstrap addresses resolved")
	}
	for _, a := range addrs {
		if !strings.HasPrefix(a.String(), "10.10.6.77:") {
			t.Errorf("bootstrap address %s did not come from the in-tunnel resolver", a)
		}
	}
	// Every upstream bootstrap host should have been asked of the in-tunnel
	// server, and of nothing else.
	for _, hostPort := range dht.DefaultGlobalBootstrapHostPorts {
		host := hostPort[:strings.LastIndex(hostPort, ":")]
		if !probe.Asked(host) {
			t.Errorf("bootstrap host %s was not resolved in-tunnel", host)
		}
	}
}

// A bootstrap that resolves nothing must fail loudly rather than silently
// falling back.
func TestDHTBootstrapFailsWhenNothingResolves(t *testing.T) {
	lookup := func(context.Context, string) ([]string, error) {
		return nil, context.DeadlineExceeded
	}
	if _, err := tunnelStartingNodes(lookup)(); err == nil {
		t.Error("expected an error when no bootstrap host resolves")
	}
}
