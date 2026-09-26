package engine

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

// UDP tracker announces are the one path with no injectable hook.
//
// tracker/udp/conn-client.go:82 calls net.ResolveUDPAddr on the tracker's
// host:port for *every write*, using the host resolver, with no way to
// override it. ClientTrackerConfig.LookupTrackerIp exists but is documented as
// deprecated and is not currently wired in.
//
// The workaround is to never hand the client a hostname: resolve it through
// the tunnel and rewrite `udp://tracker.example.org:1337/announce` to
// `udp://198.51.100.7:1337/announce` before the URL reaches the client.
// net.ResolveUDPAddr on an IP literal performs no lookup, so the leaking call
// becomes a parse.
//
// HTTP trackers are deliberately left alone: they need the hostname for the
// Host header and TLS SNI, and TrackerDialContext already resolves and dials
// them in-tunnel.

// trackerCacheTTL is how long a resolved tracker address is reused. Trackers
// move, and a stale IP is a tracker that silently stops working, so this is
// short enough to recover on its own.
const trackerCacheTTL = 15 * time.Minute

// hostLookup resolves a hostname to IP strings. In production this is the
// tunnel's resolver; nothing else is acceptable.
type hostLookup func(ctx context.Context, host string) ([]string, error)

// trackerResolver rewrites UDP announce URLs to IP literals, caching results.
type trackerResolver struct {
	lookup hostLookup
	ttl    time.Duration

	mu    sync.Mutex
	cache map[string]trackerCacheEntry
}

type trackerCacheEntry struct {
	ip      string
	expires time.Time
}

func newTrackerResolver(lookup hostLookup) *trackerResolver {
	return &trackerResolver{
		lookup: lookup,
		ttl:    trackerCacheTTL,
		cache:  make(map[string]trackerCacheEntry),
	}
}

// RewriteAll maps every announce URL through Rewrite, dropping any UDP tracker
// whose hostname will not resolve. A tracker we cannot resolve in-tunnel is
// useless to us - passing it through unresolved would hand the leaking path a
// hostname, which is the one outcome that must not happen.
func (r *trackerResolver) RewriteAll(ctx context.Context, urls []string) []string {
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		rewritten, err := r.Rewrite(ctx, u)
		if err != nil {
			continue
		}
		out = append(out, rewritten)
	}
	return out
}

// Rewrite returns the announce URL with a UDP tracker's hostname replaced by a
// resolved IP literal. Non-UDP URLs, and UDP URLs that already carry a
// literal, are returned unchanged.
func (r *trackerResolver) Rewrite(ctx context.Context, announce string) (string, error) {
	u, err := url.Parse(announce)
	if err != nil {
		return "", fmt.Errorf("invalid announce URL %q: %w", announce, err)
	}
	if !strings.EqualFold(u.Scheme, "udp") {
		return announce, nil
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("announce URL %q has no host", announce)
	}
	if net.ParseIP(host) != nil {
		return announce, nil
	}

	ip, err := r.resolve(ctx, host)
	if err != nil {
		return "", err
	}
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(ip, port)
	} else {
		u.Host = ip
	}
	return u.String(), nil
}

// resolve returns a cached or freshly looked-up address for host, preferring
// IPv4 because the tunnel is usually v4-only and a v6 answer would be
// unroutable.
func (r *trackerResolver) resolve(ctx context.Context, host string) (string, error) {
	now := time.Now()
	r.mu.Lock()
	entry, ok := r.cache[host]
	r.mu.Unlock()
	if ok && now.Before(entry.expires) {
		return entry.ip, nil
	}

	addrs, err := r.lookup(ctx, host)
	if err != nil {
		return "", fmt.Errorf("failed to resolve the tracker host %q in-tunnel: %w", host, err)
	}
	ip := preferV4(addrs)
	if ip == "" {
		return "", fmt.Errorf("tracker host %q resolved to no usable address", host)
	}

	r.mu.Lock()
	r.cache[host] = trackerCacheEntry{ip: ip, expires: now.Add(r.ttl)}
	r.mu.Unlock()
	return ip, nil
}

func preferV4(addrs []string) string {
	var fallback string
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil {
			continue
		}
		if ip.To4() != nil {
			return a
		}
		if fallback == "" {
			fallback = a
		}
	}
	return fallback
}
