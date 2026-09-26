package engine

import (
	"context"
	"errors"
	"log/slog"
	"net"

	"github.com/anacrolix/dht/v2"
)

// tunnelStartingNodes resolves the DHT bootstrap hosts through the tunnel.
//
// This override is load-bearing. The default, dht.GlobalBootstrapAddrs, calls
// dht.ResolveHostPorts, which uses a package-global rs/dnscache.Resolver
// (dht/dns.go) - the *host* resolver. Every DHT bootstrap hostname would be
// looked up outside the tunnel on startup, which is precisely the leak this
// package exists to close. Only the resolution changes; the host list is
// upstream's.
func tunnelStartingNodes(lookup hostLookup) dht.StartingNodesGetter {
	return func() ([]dht.Addr, error) {
		// The getter has no context of its own, and it runs on the DHT's
		// bootstrap path, so a hung resolver would hang bootstrap. Bound it.
		ctx, cancel := context.WithTimeout(context.Background(), dhtBootstrapTimeout)
		defer cancel()

		var addrs []dht.Addr
		for _, hostPort := range dht.DefaultGlobalBootstrapHostPorts {
			host, port, err := net.SplitHostPort(hostPort)
			if err != nil {
				continue
			}
			ips, err := lookup(ctx, host)
			if err != nil {
				slog.Debug("DHT bootstrap host did not resolve in-tunnel", "host", host, "error", err)
				continue
			}
			for _, ip := range ips {
				parsed := net.ParseIP(ip)
				if parsed == nil {
					continue
				}
				udp, err := net.ResolveUDPAddr("udp", net.JoinHostPort(ip, port))
				if err != nil {
					// An IP literal, so this parses rather than resolves.
					continue
				}
				addrs = append(addrs, dht.NewAddr(udp))
			}
		}
		if len(addrs) == 0 {
			return nil, errors.New("no DHT bootstrap host resolved through the tunnel")
		}
		return addrs, nil
	}
}
