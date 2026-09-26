// Package tunneltest stands up a real WireGuard tunnel inside a test.
//
// Both ends are the same library, so two netstacks and two devices over
// loopback give a genuinely encrypted tunnel in one `go test`, with no VPN
// account and no privileges. That makes the leak assertions testable in CI
// rather than only by a human watching tcpdump: a lookup that escapes to the
// host resolver fails a test here, because the only DNS server that answers
// exists solely inside the tunnel.
package tunneltest

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/net/dns/dnsmessage"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"

	"github.com/moshen/bittrench/internal/config"
	"github.com/moshen/bittrench/internal/tunnel"
)

// Peer stands in for the VPN provider: a second netstack and device listening
// on the host loopback, which is what the tunnel's single host UDP socket
// talks to.
type Peer struct {
	Net  *netstack.Net
	Addr netip.Addr

	dev  *device.Device
	port uint16
	pub  [32]byte
}

// StartPeer brings up the provider end at addr.
func StartPeer(t *testing.T, addr netip.Addr) *Peer {
	t.Helper()
	priv, pub := keypair(t)

	tunDev, tnet, err := netstack.CreateNetTUN([]netip.Addr{addr}, nil, tunnel.MTU)
	if err != nil {
		t.Fatalf("provider netstack: %v", err)
	}
	dev := device.NewDevice(tunDev, conn.NewDefaultBind(), device.NewLogger(device.LogLevelSilent, "peer "))
	t.Cleanup(dev.Close)

	// listen_port=0 lets the kernel pick, so nothing here collides with a
	// parallel test.
	if err := dev.IpcSet(fmt.Sprintf("private_key=%s\nlisten_port=0\n", hex.EncodeToString(priv[:]))); err != nil {
		t.Fatalf("provider IpcSet: %v", err)
	}
	if err := dev.Up(); err != nil {
		t.Fatalf("provider Up: %v", err)
	}
	return &Peer{Net: tnet, Addr: addr, dev: dev, port: listenPort(t, dev), pub: pub}
}

// StartTunnel brings up the code under test against the provider and waits for
// the handshake, so callers get a tunnel that is already carrying traffic.
func StartTunnel(t *testing.T, p *Peer, clientAddr netip.Addr) *tunnel.Tunnel {
	t.Helper()
	priv, pub := keypair(t)
	p.allow(t, pub, clientAddr)

	cfg := &config.WireGuardConfig{
		PrivateKey:       base64.StdEncoding.EncodeToString(priv[:]),
		PeerPublicKey:    base64.StdEncoding.EncodeToString(p.pub[:]),
		Endpoint:         netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), p.port).String(),
		ClientIP:         clientAddr.String() + "/32",
		KeepaliveSeconds: 25,
		DNS:              p.Addr.String(),
	}
	tun, err := tunnel.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("tunnel.Start: %v", err)
	}
	t.Cleanup(func() { tun.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := tun.WaitHandshake(ctx); err != nil {
		t.Fatalf("WaitHandshake: %v", err)
	}
	return tun
}

// allow authorises client as a peer of the provider. The provider learns the
// client's endpoint from the handshake, so none is configured.
func (p *Peer) allow(t *testing.T, clientPub [32]byte, clientAddr netip.Addr) {
	t.Helper()
	cfg := fmt.Sprintf("public_key=%s\nreplace_allowed_ips=true\nallowed_ip=%s/32\n",
		hex.EncodeToString(clientPub[:]), clientAddr.String())
	if err := p.dev.IpcSet(cfg); err != nil {
		t.Fatalf("provider IpcSet peer: %v", err)
	}
}

// DNSProbe records what an in-tunnel DNS server was asked.
type DNSProbe struct {
	mu    sync.Mutex
	names []string
	types []dnsmessage.Type
}

// Names returns the hostnames queried, in order, with the trailing dot
// stripped.
func (d *DNSProbe) Names() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.names...)
}

// Types returns the question types asked, in order.
func (d *DNSProbe) Types() []dnsmessage.Type {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]dnsmessage.Type(nil), d.types...)
}

// Asked reports whether name was queried.
func (d *DNSProbe) Asked(name string) bool {
	for _, n := range d.Names() {
		if strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}

// ServeDNS runs a DNS server on the provider's tunnel address, port 53 - the
// address StartTunnel configures as the tunnel's resolver.
//
// A queries are answered from zone, falling back to fallbackA when the name is
// absent (pass an empty fallbackA to answer NXDOMAIN-ish empty instead). AAAA
// queries are answered with a v6 address the tunnel cannot route, so a v4-only
// client that wrongly asked for one is caught by the answer as well as by the
// recorded question.
func ServeDNS(t *testing.T, p *Peer, zone map[string]string, fallbackA string) *DNSProbe {
	t.Helper()
	conn, err := p.Net.ListenUDP(&net.UDPAddr{IP: p.Addr.AsSlice(), Port: 53})
	if err != nil {
		t.Fatalf("in-tunnel DNS listen: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	probe := &DNSProbe{}
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			reply, err := answer(buf[:n], probe, zone, fallbackA)
			if err != nil {
				continue
			}
			conn.WriteTo(reply, from)
		}
	}()
	return probe
}

func answer(query []byte, probe *DNSProbe, zone map[string]string, fallbackA string) ([]byte, error) {
	var parser dnsmessage.Parser
	header, err := parser.Start(query)
	if err != nil {
		return nil, err
	}
	questions, err := parser.AllQuestions()
	if err != nil || len(questions) == 0 {
		return nil, fmt.Errorf("no questions: %w", err)
	}

	probe.mu.Lock()
	for _, q := range questions {
		probe.names = append(probe.names, strings.TrimSuffix(q.Name.String(), "."))
		probe.types = append(probe.types, q.Type)
	}
	probe.mu.Unlock()

	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:                 header.ID,
		Response:           true,
		Authoritative:      true,
		RecursionDesired:   header.RecursionDesired,
		RecursionAvailable: true,
	})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	for _, q := range questions {
		if err := b.Question(q); err != nil {
			return nil, err
		}
	}
	if err := b.StartAnswers(); err != nil {
		return nil, err
	}
	for _, q := range questions {
		rh := dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 60}
		switch q.Type {
		case dnsmessage.TypeA:
			name := strings.TrimSuffix(q.Name.String(), ".")
			value, ok := zone[name]
			if !ok {
				value = fallbackA
			}
			addr, err := netip.ParseAddr(value)
			if err != nil || !addr.Is4() {
				continue
			}
			if err := b.AResource(rh, dnsmessage.AResource{A: addr.As4()}); err != nil {
				return nil, err
			}
		case dnsmessage.TypeAAAA:
			var v6 [16]byte
			copy(v6[:], netip.MustParseAddr("2001:db8::42").AsSlice())
			if err := b.AAAAResource(rh, dnsmessage.AAAAResource{AAAA: v6}); err != nil {
				return nil, err
			}
		}
	}
	return b.Finish()
}

func keypair(t *testing.T) (priv, pub [32]byte) {
	t.Helper()
	if _, err := rand.Read(priv[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	// Curve25519 clamping, as WireGuard requires.
	priv[0] &= 248
	priv[31] = (priv[31] & 127) | 64
	out, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		t.Fatalf("curve25519: %v", err)
	}
	copy(pub[:], out)
	return priv, pub
}

func listenPort(t *testing.T, dev *device.Device) uint16 {
	t.Helper()
	var sb strings.Builder
	if err := dev.IpcGetOperation(&sb); err != nil {
		t.Fatalf("IpcGetOperation: %v", err)
	}
	for line := range strings.SplitSeq(sb.String(), "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "listen_port="); ok {
			port, err := strconv.ParseUint(value, 10, 16)
			if err != nil {
				t.Fatalf("bad listen_port %q: %v", value, err)
			}
			return uint16(port)
		}
	}
	t.Fatal("device reported no listen_port")
	return 0
}
