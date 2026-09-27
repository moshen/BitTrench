package engine

import (
	"context"
	"fmt"
	"net/netip"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moshen/bittrench/internal/config"
	"github.com/moshen/bittrench/internal/store"
	"github.com/moshen/bittrench/internal/tunneltest"
)

// subnet hands each harness its own /24 so tests can run in parallel without
// two netstacks claiming the same tunnel address.
var subnet atomic.Int32

// cfgOpts are the knobs a test wants to vary.
type cfgOpts struct {
	savePath          string
	allowedExtensions []string
	seedRatioLimit    float64
	seedTimeLimitSecs uint64
	listenInTunnel    bool
	enablePex         bool
	downloadQueueSize uint32
}

// harness is an engine on a real tunnel with a real state database.
type harness struct {
	engine *Engine
	store  *store.Store
	cfg    *config.AppConfig
	dbPath string
	tun    Net
}

func newHarness(t *testing.T, customise func(*cfgOpts)) *harness {
	t.Helper()

	opts := cfgOpts{savePath: t.TempDir()}
	if customise != nil {
		customise(&opts)
	}

	octet := subnet.Add(1) + 20
	peerAddr := netip.MustParseAddr(fmt.Sprintf("10.%d.0.1", octet))
	clientAddr := netip.MustParseAddr(fmt.Sprintf("10.%d.0.2", octet))
	p := tunneltest.StartPeer(t, peerAddr)
	tun := tunneltest.StartTunnel(t, p, clientAddr)
	tunneltest.ServeDNS(t, p, nil, peerAddr.String())

	cfg := config.Defaults()
	cfg.WireGuard = config.WireGuardConfig{
		PrivateKey:    "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		PeerPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		Endpoint:      "198.51.100.5:51820",
		ClientIP:      clientAddr.String() + "/32",
		DNS:           peerAddr.String(),
	}
	cfg.Torrent.SavePath = opts.savePath
	cfg.Torrent.AllowedExtensions = opts.allowedExtensions
	cfg.Torrent.ListenInTunnel = opts.listenInTunnel
	cfg.Torrent.EnablePex = opts.enablePex
	// Off unless a test asks for it, so every existing test keeps running every
	// torrent it adds.
	cfg.Torrent.DownloadQueueSize = opts.downloadQueueSize
	cfg.Torrent.Limits.SeedRatioLimit = opts.seedRatioLimit
	cfg.Torrent.Limits.SeedTimeLimitSecs = opts.seedTimeLimitSecs
	// DHT off: bootstrap would sit resolving against a DNS server that
	// answers, but no node replies, which just slows the tests down.
	cfg.Torrent.EnableDHT = false

	h := &harness{cfg: &cfg, dbPath: filepath.Join(t.TempDir(), "state.db"), tun: tun}
	h.engine = h.open(t)
	return h
}

// open builds a store, a completion and an engine against the harness's
// database, and registers cleanup.
func (h *harness) open(t *testing.T) *Engine {
	t.Helper()
	s, err := store.Open(h.dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	completion, err := s.NewCompletion(context.Background())
	if err != nil {
		t.Fatalf("NewCompletion: %v", err)
	}
	e, err := New(Options{Config: h.cfg, Net: h.tun, Store: s, Completion: completion})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.store = s
	t.Cleanup(func() {
		e.Close()
		completion.Close()
		s.Close()
	})
	return e
}

// restart closes the engine and opens a fresh one against the same database,
// which is what a daemon restart looks like from the store's point of view.
func (h *harness) restart(t *testing.T) *Engine {
	t.Helper()
	h.engine.Close()
	h.store.Close()
	h.engine = h.open(t)
	return h.engine
}

func (h *harness) waitForMetadata(t *testing.T, id int64) {
	t.Helper()
	waitForMetadata(t, h.engine, id)
}

// waitForMetadata blocks until the info dict has resolved and the engine has
// finished its post-metadata work - recording the name, sizing the completion
// bitfields, and applying the file selection. Waiting only on GotInfo races
// that work.
func waitForMetadata(t *testing.T, e *Engine, id int64) {
	t.Helper()
	rec := e.record(id)
	if rec == nil {
		t.Fatalf("no torrent with id %d", id)
	}
	select {
	case <-rec.ready:
	case <-time.After(15 * time.Second):
		t.Fatalf("timed out waiting for the metadata of torrent %d", id)
	}
}
