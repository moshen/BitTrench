package engine

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/moshen/bittrench/internal/tunneltest"
)

func fakeLookup(zone map[string][]string, calls *atomic.Int64) hostLookup {
	return func(_ context.Context, host string) ([]string, error) {
		if calls != nil {
			calls.Add(1)
		}
		addrs, ok := zone[host]
		if !ok {
			return nil, errors.New("no such host")
		}
		return addrs, nil
	}
}

// The guard rail: no announce URL handed to the client may carry a non-literal
// host for the udp scheme, because tracker/udp/conn-client.go resolves it with
// the host resolver on every write and there is no hook to redirect that.
func TestNoUDPAnnounceURLKeepsAHostname(t *testing.T) {
	r := newTrackerResolver(fakeLookup(map[string][]string{
		"tracker.example.org": {"198.51.100.7"},
		"tracker.example.net": {"2001:db8::5", "198.51.100.8"},
	}, nil))

	in := []string{
		"udp://tracker.example.org:1337/announce",
		"udp://tracker.example.net:451/announce",
		"udp://198.51.100.9:6969/announce",
		"udp://tracker.unresolvable.invalid:1337/announce",
		"http://tracker.example.org/announce",
		"https://tracker.example.org:443/announce",
	}
	out := r.RewriteAll(context.Background(), in)

	for _, raw := range out {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("rewritten URL %q does not parse: %v", raw, err)
		}
		if u.Scheme != "udp" {
			continue
		}
		if net.ParseIP(u.Hostname()) == nil {
			t.Errorf("udp announce %q still carries the hostname %q", raw, u.Hostname())
		}
	}

	// An unresolvable UDP tracker is dropped rather than passed through: a
	// hostname reaching the client is the one outcome that must not happen.
	for _, raw := range out {
		if strings.Contains(raw, "unresolvable") {
			t.Errorf("an unresolvable udp tracker survived as %q", raw)
		}
	}
	if len(out) != 5 {
		t.Errorf("expected 5 surviving trackers, got %d: %v", len(out), out)
	}
}

// HTTP trackers must keep their hostname: it is the Host header and the TLS
// SNI, and TrackerDialContext already resolves and dials them in-tunnel.
func TestHTTPAnnounceURLsAreUntouched(t *testing.T) {
	var calls atomic.Int64
	r := newTrackerResolver(fakeLookup(map[string][]string{"tracker.example.org": {"198.51.100.7"}}, &calls))

	for _, in := range []string{
		"http://tracker.example.org/announce",
		"https://tracker.example.org:443/announce?x=1",
		"wss://tracker.example.org/announce",
	} {
		got, err := r.Rewrite(context.Background(), in)
		if err != nil {
			t.Fatalf("Rewrite(%q): %v", in, err)
		}
		if got != in {
			t.Errorf("Rewrite(%q) = %q, want it unchanged", in, got)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("resolving a non-udp tracker: %d lookups, want 0", calls.Load())
	}
}

func TestUDPLiteralIsNotResolved(t *testing.T) {
	var calls atomic.Int64
	r := newTrackerResolver(fakeLookup(nil, &calls))
	const in = "udp://198.51.100.9:6969/announce"
	got, err := r.Rewrite(context.Background(), in)
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if got != in {
		t.Errorf("Rewrite(%q) = %q, want it unchanged", in, got)
	}
	if calls.Load() != 0 {
		t.Errorf("an IP literal triggered %d lookups", calls.Load())
	}
}

func TestRewritePreservesPortAndPath(t *testing.T) {
	r := newTrackerResolver(fakeLookup(map[string][]string{"t.example": {"198.51.100.7"}}, nil))
	got, err := r.Rewrite(context.Background(), "udp://t.example:1337/announce?key=v")
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if got != "udp://198.51.100.7:1337/announce?key=v" {
		t.Errorf("Rewrite = %q", got)
	}
}

// The tunnel is usually v4-only, so a v6 answer would be unroutable.
func TestRewritePrefersIPv4(t *testing.T) {
	r := newTrackerResolver(fakeLookup(map[string][]string{"t.example": {"2001:db8::5", "198.51.100.8"}}, nil))
	got, err := r.Rewrite(context.Background(), "udp://t.example:1337/announce")
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if !strings.Contains(got, "198.51.100.8") {
		t.Errorf("Rewrite = %q, want the v4 address", got)
	}
}

// Trackers move, so results are cached with a TTL rather than forever - but
// within the TTL a repeated announce must not re-resolve.
func TestResolutionIsCached(t *testing.T) {
	var calls atomic.Int64
	r := newTrackerResolver(fakeLookup(map[string][]string{"t.example": {"198.51.100.7"}}, &calls))
	for range 5 {
		if _, err := r.Rewrite(context.Background(), "udp://t.example:1337/announce"); err != nil {
			t.Fatalf("Rewrite: %v", err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("%d lookups for 5 rewrites, want 1", calls.Load())
	}
	if r.ttl <= 0 {
		t.Error("the cache never expires; trackers move")
	}
}

// End to end: the rewrite must resolve through the tunnel, against a DNS
// server that exists nowhere else.
func TestRewriteResolvesInTunnel(t *testing.T) {
	p := tunneltest.StartPeer(t, netip.MustParseAddr("10.11.0.1"))
	tun := tunneltest.StartTunnel(t, p, netip.MustParseAddr("10.11.0.2"))
	probe := tunneltest.ServeDNS(t, p, map[string]string{"tracker.example": "10.11.0.55"}, "")

	r := newTrackerResolver(tun.LookupHost)
	got, err := r.Rewrite(context.Background(), "udp://tracker.example:1337/announce")
	if err != nil {
		t.Fatalf("Rewrite through the tunnel: %v", err)
	}
	if got != "udp://10.11.0.55:1337/announce" {
		t.Errorf("Rewrite = %q, want the in-tunnel answer", got)
	}
	if !probe.Asked("tracker.example") {
		t.Error("the tracker hostname was not resolved by the in-tunnel server")
	}
}
