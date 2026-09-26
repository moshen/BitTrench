// Package tunnel brings up the user-space WireGuard device and the user-space
// TCP/IP stack that every byte of BitTorrent traffic travels through.
//
// One host UDP socket - the WireGuard bind - is the only thing in the process
// that touches the host network stack (besides the RPC/UI listener, which is
// deliberately on localhost). Everything else dials, listens and resolves
// through the [netstack.Net] returned here. Any new networking code must be
// checked against the invariants in AGENTS.md.
package tunnel

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/tun/netstack"

	"github.com/moshen/bittrench/internal/config"
)

// MTU is the tunnel MTU: wireguard-go's own default, which leaves room for the
// IPv6-capable WireGuard overhead instead of assuming a v4 header. Promote it to
// a config key the moment a provider needs another.
const MTU = device.DefaultMTU

// Tunnel owns the WireGuard device and the user-space network stack riding on
// it. Net is the whole of the daemon's network surface.
type Tunnel struct {
	Net *netstack.Net

	dev       *device.Device
	tun       tun.Device
	clientIP  netip.Addr
	endpoint  netip.AddrPort
	dnsServer []netip.Addr
}

// Start builds the netstack, configures the WireGuard device from cfg and
// brings it up. The returned Tunnel is usable immediately; the first packet
// through it triggers the handshake. Use WaitHandshake to block until the peer
// has actually answered.
func Start(ctx context.Context, cfg *config.WireGuardConfig) (*Tunnel, error) {
	privateKey, err := cfg.PrivateKeyBytes()
	if err != nil {
		return nil, err
	}
	peerKey, err := cfg.PeerPublicKeyBytes()
	if err != nil {
		return nil, err
	}
	prefix, err := cfg.ClientPrefix()
	if err != nil {
		return nil, err
	}
	dnsServers, err := cfg.DNSServers()
	if err != nil {
		return nil, err
	}
	if len(dnsServers) == 0 {
		// Without in-tunnel DNS every lookup a torrent implies would go to
		// the host resolver, which is exactly the leak this daemon exists to
		// close. config.Validate rejects this too; belt and braces, because
		// a netstack built with no servers merely fails to resolve.
		return nil, errors.New("wireguard.dns is required: the tunnel cannot resolve without it")
	}
	// The one deliberate host-stack lookup: the peer's address is needed
	// before the tunnel that would carry the lookup exists.
	endpoint, err := cfg.EndpointAddr(ctx)
	if err != nil {
		return nil, err
	}

	clientIP := prefix.Addr()
	tunDev, tnet, err := netstack.CreateNetTUN([]netip.Addr{clientIP}, dnsServers, MTU)
	if err != nil {
		return nil, fmt.Errorf("failed to create the user-space network stack: %w", err)
	}

	dev := device.NewDevice(tunDev, conn.NewDefaultBind(), slogDeviceLogger())
	if err := dev.IpcSet(uapiConfig(privateKey, peerKey, endpoint, cfg.KeepaliveSeconds)); err != nil {
		dev.Close()
		return nil, fmt.Errorf("failed to configure the WireGuard device: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("failed to bring the WireGuard device up: %w", err)
	}

	return &Tunnel{
		Net:       tnet,
		dev:       dev,
		tun:       tunDev,
		clientIP:  clientIP,
		endpoint:  endpoint,
		dnsServer: dnsServers,
	}, nil
}

// ClientIP is the tunnel-side address assigned by the provider. Peers and the
// DHT are told about this one, never a host address.
func (t *Tunnel) ClientIP() netip.Addr { return t.clientIP }

// Endpoint is the resolved provider endpoint the host socket talks to.
func (t *Tunnel) Endpoint() netip.AddrPort { return t.endpoint }

// Close tears the device down. The netstack goes with it.
func (t *Tunnel) Close() error {
	t.dev.Close()
	return nil
}

// DialContext dials through the tunnel, resolving any hostname through the
// in-tunnel resolver first. This is the func to hand to anything that takes a
// dial hook - the torrent client's tracker and HTTP dialers both do.
func (t *Tunnel) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return t.Net.DialContext(ctx, network, address)
}

// LookupHost resolves a hostname through the tunnel's own resolver, against
// the servers from `[wireguard] dns`. The netstack only issues the queries its
// local addresses can route, so a v4-only tunnel never asks for AAAA.
func (t *Tunnel) LookupHost(ctx context.Context, host string) ([]string, error) {
	return t.Net.LookupContextHost(ctx, host)
}

// ListenUDP opens an in-tunnel UDP socket bound to the tunnel address. Port 0
// lets the netstack pick. This is the socket the DHT server and the UDP
// tracker client are given.
//
// Always go through this rather than calling netstack directly: a UDPAddr with
// no IP (`&net.UDPAddr{Port: 0}`) leaves the netstack unable to infer an
// address family and it **panics** with "invalid protocol number = 0" rather
// than returning an error. Binding the tunnel address explicitly is the fix.
func (t *Tunnel) ListenUDP(port uint16) (net.PacketConn, error) {
	c, err := t.Net.ListenUDPAddrPort(netip.AddrPortFrom(t.clientIP, port))
	if err != nil {
		return nil, fmt.Errorf("failed to open an in-tunnel UDP socket on port %d: %w", port, err)
	}
	return c, nil
}

// ListenTCP accepts inbound peer connections inside the tunnel. Nothing on the
// host can reach this listener; only peers routed to the tunnel address can,
// and only if the provider forwards a port. Same address-family caveat as
// ListenUDP.
func (t *Tunnel) ListenTCP(port uint16) (net.Listener, error) {
	l, err := t.Net.ListenTCPAddrPort(netip.AddrPortFrom(t.clientIP, port))
	if err != nil {
		return nil, fmt.Errorf("failed to open an in-tunnel TCP listener on port %d: %w", port, err)
	}
	return l, nil
}

// HTTPClient returns an http.Client whose every connection and every lookup
// goes through the tunnel. Used for HTTP(S) tracker announces, `.torrent`
// fetches by URL, and webseeds.
//
// Transport.Proxy is deliberately left nil. http.DefaultTransport uses
// ProxyFromEnvironment, which would send every tracker announce to whatever
// HTTP_PROXY/HTTPS_PROXY names - over the host stack, outside the tunnel,
// telling the proxy operator every host we talk to. Setting Proxy here would
// be a leak; it belongs in the audit alongside the DHT and tracker paths.
func (t *Tunnel) HTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:           t.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          32,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
	}
}

// WaitHandshake blocks until the peer has completed a handshake, or ctx is
// done. A tunnel whose peer never answers otherwise looks like a working
// daemon that simply cannot find any peers, which is a miserable thing to
// debug - so the daemon waits for proof at startup.
func (t *Tunnel) WaitHandshake(ctx context.Context) error {
	t.nudge()

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		ok, err := t.handshaken()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("no WireGuard handshake with %s: %w", t.endpoint, ctx.Err())
		case <-ticker.C:
		}
	}
}

// nudge gives the device something to send, because wireguard-go only starts a
// handshake when it has an outbound packet to protect. One datagram addressed
// to the configured DNS server is enough; whether anything answers is
// irrelevant, so every error here is ignored on purpose.
func (t *Tunnel) nudge() {
	conn, err := t.Net.DialUDPAddrPort(netip.AddrPort{}, netip.AddrPortFrom(t.dnsServer[0], 53))
	if err != nil {
		return
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	_, _ = conn.Write([]byte{0})
}

// handshaken reports whether the peer has a non-zero last handshake time.
func (t *Tunnel) handshaken() (bool, error) {
	var sb strings.Builder
	if err := t.dev.IpcGetOperation(&sb); err != nil {
		return false, fmt.Errorf("failed to read the WireGuard device state: %w", err)
	}
	for line := range strings.SplitSeq(sb.String(), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || key != "last_handshake_time_sec" {
			continue
		}
		secs, err := strconv.ParseInt(value, 10, 64)
		if err == nil && secs > 0 {
			return true, nil
		}
	}
	return false, nil
}

// uapiConfig renders the WireGuard UAPI text the device expects. Note it takes
// hex keys, not the base64 that WireGuard configuration files (and ours) use.
func uapiConfig(privateKey, peerKey [32]byte, endpoint netip.AddrPort, keepaliveSeconds uint32) string {
	var b strings.Builder
	fmt.Fprintf(&b, "private_key=%s\n", hex.EncodeToString(privateKey[:]))
	b.WriteString("replace_peers=true\n")
	fmt.Fprintf(&b, "public_key=%s\n", hex.EncodeToString(peerKey[:]))
	fmt.Fprintf(&b, "endpoint=%s\n", endpoint.String())
	b.WriteString("replace_allowed_ips=true\n")
	// Full-tunnel: everything the netstack emits goes to the provider. The
	// netstack only holds the addresses given to CreateNetTUN, so listing
	// both families here costs nothing and avoids a v6-capable provider
	// silently dropping traffic.
	b.WriteString("allowed_ip=0.0.0.0/0\n")
	b.WriteString("allowed_ip=::/0\n")
	if keepaliveSeconds > 0 {
		fmt.Fprintf(&b, "persistent_keepalive_interval=%d\n", keepaliveSeconds)
	}
	return b.String()
}

// slogDeviceLogger routes wireguard-go's own logging into slog, so the daemon
// has one log stream rather than the device writing to the `log` package.
func slogDeviceLogger() *device.Logger {
	return &device.Logger{
		Verbosef: func(format string, args ...any) {
			slog.Debug("wireguard: " + fmt.Sprintf(format, args...))
		},
		Errorf: func(format string, args ...any) {
			slog.Error("wireguard: " + fmt.Sprintf(format, args...))
		},
	}
}
