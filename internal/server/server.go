// Package server assembles the one host listener the daemon opens on purpose:
// the RPC endpoint and the web UI, on localhost.
//
// Everything else in the process lives inside the tunnel. This listener is
// row 13 of the leak audit - intended, and bound to the configured interface,
// which defaults to 127.0.0.1.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/moshen/bittrench/internal/api"
	"github.com/moshen/bittrench/internal/config"
	"github.com/moshen/bittrench/internal/rpc"
	"github.com/moshen/bittrench/internal/webui"
)

// Engine is everything the HTTP layers need. Both the RPC and API packages
// declare their own narrower views; this is their union.
type Engine interface {
	rpc.Torrents
	api.Torrents
}

// Server is the HTTP surface.
type Server struct {
	http     *http.Server
	listener net.Listener
}

// New builds the mux and binds the listener, so a port conflict is reported at
// startup rather than from inside a goroutine.
func New(cfg *config.AppConfig, eng Engine) (*Server, error) {
	mux := http.NewServeMux()

	rpcHandler, err := rpc.New(eng, cfg)
	if err != nil {
		return nil, err
	}
	rpcHandler.Register(mux)

	// The native API and the UI share the RPC endpoint's credentials. The
	// Transmission handler does its own auth because its 401 has to carry a
	// Transmission-shaped body, and its 409 handshake has to survive it.
	apiHandler := api.New(eng)
	protected := http.NewServeMux()
	apiHandler.Register(protected)
	if err := webui.Register(protected); err != nil {
		return nil, err
	}
	mux.Handle("/", basicAuth(cfg, protected))

	addr, err := cfg.API.ListenAddr()
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", addr.String())
	if err != nil {
		return nil, fmt.Errorf("failed to bind the API listener on %s: %w", addr, err)
	}
	// Auth is optional because the default bind is loopback, where it buys
	// little. Off the loopback it is the only thing between this endpoint and
	// whoever can route to it - and the container image binds 0.0.0.0 by
	// design, so this combination is now reachable by accident rather than
	// only by hand. A warning, not an error: an operator who has put the
	// listener behind something else is entitled to it.
	if !addr.Addr().IsLoopback() && cfg.API.Username == "" && cfg.API.Password == "" {
		slog.Warn("the RPC endpoint and web UI are bound off the loopback with no authentication; "+
			"set [api] username and password, or publish the port to 127.0.0.1 only",
			"addr", addr)
	}

	return &Server{
		http: &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
		},
		listener: listener,
	}, nil
}

// Addr is the bound address.
func (s *Server) Addr() net.Addr { return s.listener.Addr() }

// Serve runs until Shutdown is called.
func (s *Server) Serve() {
	if err := s.http.Serve(s.listener); err != nil && err != http.ErrServerClosed {
		slog.Error("the API server stopped", "error", err)
	}
}

// Shutdown stops accepting and drains in-flight requests. It is step 1 of the
// documented shutdown order: the torrent client must not be closed underneath
// a request that is still reading from it.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

// basicAuth guards the UI and the native API. Empty credentials disable it,
// which is the documented behaviour for a localhost-only bind.
func basicAuth(cfg *config.AppConfig, next http.Handler) http.Handler {
	if cfg.API.Username == "" && cfg.API.Password == "" {
		return next
	}
	expected := "Basic " + base64.StdEncoding.EncodeToString(
		[]byte(cfg.API.Username+":"+cfg.API.Password))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="bittrench"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
