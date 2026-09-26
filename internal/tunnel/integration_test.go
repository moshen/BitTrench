package tunnel_test

import (
	"context"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/moshen/bittrench/internal/tunneltest"
)

func TestTunnelCarriesTCP(t *testing.T) {
	p := tunneltest.StartPeer(t, netip.MustParseAddr("10.9.0.1"))
	tun := tunneltest.StartTunnel(t, p, netip.MustParseAddr("10.9.0.2"))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ln, err := p.Net.ListenTCP(&net.TCPAddr{IP: p.Addr.AsSlice(), Port: 9000})
	if err != nil {
		t.Fatalf("provider ListenTCP: %v", err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c)
	}()

	c, err := tun.DialContext(ctx, "tcp4", net.JoinHostPort(p.Addr.String(), "9000"))
	if err != nil {
		t.Fatalf("dial through the tunnel: %v", err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 4)
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "ping" {
		t.Errorf("echo = %q, want \"ping\"", buf)
	}
}

// The DHT and UDP trackers ride on this path, and it is an easy one to get
// wrong: a single unroutable destination must not tear down the whole socket and
// take the DHT with it. The netstack has to keep the socket usable for
// subsequent sends.
func TestTunnelCarriesUDPAndSurvivesAnUnroutableSend(t *testing.T) {
	p := tunneltest.StartPeer(t, netip.MustParseAddr("10.9.1.1"))
	tun := tunneltest.StartTunnel(t, p, netip.MustParseAddr("10.9.1.2"))

	server, err := p.Net.ListenUDP(&net.UDPAddr{IP: p.Addr.AsSlice(), Port: 9001})
	if err != nil {
		t.Fatalf("provider ListenUDP: %v", err)
	}
	defer server.Close()
	go func() {
		buf := make([]byte, 64)
		for {
			n, from, err := server.ReadFrom(buf)
			if err != nil {
				return
			}
			server.WriteTo(buf[:n], from)
		}
	}()

	client, err := tun.ListenUDP(0)
	if err != nil {
		t.Fatalf("client ListenUDP: %v", err)
	}
	defer client.Close()

	// An IPv6 destination on a v4-only tunnel: unroutable. The send must not
	// take the socket with it.
	unroutable := &net.UDPAddr{IP: net.ParseIP("2001:db8::1"), Port: 9001}
	if _, err := client.WriteTo([]byte("nowhere"), unroutable); err == nil {
		t.Log("unroutable send unexpectedly succeeded; the socket must still work either way")
	}

	target := &net.UDPAddr{IP: p.Addr.AsSlice(), Port: 9001}
	if _, err := client.WriteTo([]byte("pong"), target); err != nil {
		t.Fatalf("write after an unroutable send: %v", err)
	}
	buf := make([]byte, 64)
	client.SetReadDeadline(time.Now().Add(10 * time.Second))
	n, _, err := client.ReadFrom(buf)
	if err != nil {
		t.Fatalf("read after an unroutable send: %v", err)
	}
	if string(buf[:n]) != "pong" {
		t.Errorf("echo = %q, want \"pong\"", buf[:n])
	}
}

// The stack is isolated from the host's: a listener on the host's loopback
// must be unreachable from inside the tunnel.
func TestHostLoopbackIsUnreachableFromTheTunnel(t *testing.T) {
	p := tunneltest.StartPeer(t, netip.MustParseAddr("10.9.2.1"))
	tun := tunneltest.StartTunnel(t, p, netip.MustParseAddr("10.9.2.2"))

	hostLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("host listener: %v", err)
	}
	defer hostLn.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		c, err := hostLn.Accept()
		if err != nil {
			return
		}
		c.Close()
		accepted <- struct{}{}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if c, err := tun.DialContext(ctx, "tcp4", hostLn.Addr().String()); err == nil {
		c.Close()
		t.Fatal("the tunnel reached a host loopback listener")
	}
	select {
	case <-accepted:
		t.Fatal("the host listener accepted a connection from the tunnel")
	default:
	}
}

// A v4-only tunnel must never hand back a v6 address it cannot route: the
// lookup strategy follows client_cidr, which is what stops an unroutable answer
// reaching a caller that would tear its socket down over it. It doubles as the
// automated form of the invariant on in-tunnel resolution: the answer comes from
// a server that exists only inside the tunnel, so a lookup that escaped to the
// host resolver would fail this test rather than quietly succeeding.
func TestV4OnlyTunnelResolvesV4OnlyInTunnel(t *testing.T) {
	p := tunneltest.StartPeer(t, netip.MustParseAddr("10.9.3.1"))
	tun := tunneltest.StartTunnel(t, p, netip.MustParseAddr("10.9.3.2"))
	probe := tunneltest.ServeDNS(t, p, map[string]string{"tracker.example": "10.9.3.42"}, "")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	addrs, err := tun.LookupHost(ctx, "tracker.example")
	if err != nil {
		t.Fatalf("in-tunnel lookup: %v", err)
	}
	if len(addrs) == 0 {
		t.Fatal("in-tunnel lookup returned nothing")
	}
	for _, a := range addrs {
		ip, perr := netip.ParseAddr(a)
		if perr != nil {
			t.Fatalf("unparseable address %q: %v", a, perr)
		}
		if ip.Is6() && !ip.Is4In6() {
			t.Errorf("v4-only tunnel returned the v6 address %s", a)
		}
	}

	if !probe.Asked("tracker.example") {
		t.Fatal("the in-tunnel DNS server was never queried, so the lookup went somewhere else")
	}
	for _, ty := range probe.Types() {
		if ty == dnsmessage.TypeAAAA {
			t.Error("a v4-only tunnel asked for AAAA")
		}
	}
}
