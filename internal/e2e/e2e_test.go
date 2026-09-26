package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moshen/bittrench/internal/api"
	"github.com/moshen/bittrench/internal/config"
	"github.com/moshen/bittrench/internal/engine"
	"github.com/moshen/bittrench/internal/rpc"
	"github.com/moshen/bittrench/internal/server"
	"github.com/moshen/bittrench/internal/store"
	"github.com/moshen/bittrench/internal/tunneltest"
)

// A real .torrent driven through the whole stack - engine, store, tunnel,
// Transmission RPC, native API and the UI - on the in-process tunnel. No
// fixture is committed: point BITTRENCH_E2E_TORRENT at a local .torrent to
// run it.
//
//	BITTRENCH_E2E_TORRENT=/path/to/x.torrent mise exec -- go test ./internal/e2e/ -v
//
// This is what found the pause panic; a synthetic torrent in a unit test did
// not, because the crash needed a real piece count and a running hash check.
func TestLocalE2EWithRealTorrent(t *testing.T) {
	fixture := os.Getenv("BITTRENCH_E2E_TORRENT")
	if fixture == "" {
		t.Skip("set BITTRENCH_E2E_TORRENT to a .torrent file to run this")
	}
	blob, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("reading %s: %v", fixture, err)
	}
	ctx := context.Background()

	peer := tunneltest.StartPeer(t, netip.MustParseAddr("10.77.0.1"))
	tun := tunneltest.StartTunnel(t, peer, netip.MustParseAddr("10.77.0.2"))
	tunneltest.ServeDNS(t, peer, nil, "10.77.0.9")

	cfg := config.Defaults()
	cfg.WireGuard = config.WireGuardConfig{
		PrivateKey:    "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		PeerPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		Endpoint:      "198.51.100.5:51820",
		ClientIP:      "10.77.0.2/32",
		DNS:           "10.77.0.1",
	}
	cfg.Torrent.SavePath = t.TempDir()
	cfg.Torrent.EnableDHT = false
	cfg.API.ListenPort = 0

	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()
	completion, err := db.NewCompletion(ctx)
	if err != nil {
		t.Fatalf("NewCompletion: %v", err)
	}
	defer completion.Close()

	eng, err := engine.New(engine.Options{Config: &cfg, Net: tun, Store: db, Completion: completion})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer eng.Close()

	srv, err := server.New(&cfg, eng)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown(ctx)
	base := "http://" + srv.Addr().String()
	t.Logf("daemon listening on %s", base)

	id, err := eng.Add(ctx, engine.AddRequest{Metainfo: blob})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if s, ok := eng.Status(id); ok && s.HasMetadata && s.PieceCount > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	s, _ := eng.Status(id)
	t.Logf("ENGINE  id=%d name=%q hash=%s size=%d pieces=%d state=%s",
		s.ID, s.Name, s.InfoHash.HexString(), s.TotalBytes, s.PieceCount, s.State)
	t.Logf("ENGINE  dir=%s files=%d trackers=%d",
		s.SavePath, len(eng.Files(id)), len(eng.AnnounceURLs(id)))
	for _, f := range eng.Files(id) {
		t.Logf("  file  %s  %d bytes  selected=%v", f.Path, f.Length, f.Selected)
	}
	for _, tr := range eng.AnnounceURLs(id) {
		t.Logf("  tracker %s", tr)
	}
	bits, count := eng.Bitfield(id)
	t.Logf("ENGINE  bitfield %d bytes for %d pieces", len(bits), count)

	// Through the Transmission endpoint, the way Radarr drives it: the GET
	// handshake, then a real torrent-get.
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(base + rpc.Path)
	if err != nil {
		t.Fatalf("rpc handshake: %v", err)
	}
	resp.Body.Close()
	token := resp.Header.Get("X-Transmission-Session-Id")
	t.Logf("RPC     handshake %d, token %s…", resp.StatusCode, token[:8])

	body := `{"method":"torrent-get","arguments":{"fields":["id","name","hashString","totalSize","status","percentDone","downloadDir","eta","leftUntilDone","isFinished"]}}`
	req, _ := http.NewRequest(http.MethodPost, base+rpc.Path, strings.NewReader(body))
	req.Header.Set("X-Transmission-Session-Id", token)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("torrent-get: %v", err)
	}
	t.Logf("RPC     torrent-get -> %d %s", resp.StatusCode, readBody(t, resp))

	for _, path := range []string{
		api.Prefix + "/torrents",
		api.Prefix + "/torrents/1/files",
		api.Prefix + "/torrents/1/pieces",
		api.Prefix + "/stats",
	} {
		resp, err := client.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		t.Logf("API     GET %-34s -> %d %s", path, resp.StatusCode, truncate(readBody(t, resp), 340))
	}

	resp, err = client.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	resp.Body.Close()
	t.Logf("UI      GET / -> %d, Cache-Control: %q", resp.StatusCode, resp.Header.Get("Cache-Control"))

	if err := eng.Stop(ctx, id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	s, _ = eng.Status(id)
	t.Logf("LIFECYCLE stopped -> state=%s paused=%v", s.State, s.Paused)
	if err := eng.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	s, _ = eng.Status(id)
	t.Logf("LIFECYCLE started -> state=%s paused=%v", s.State, s.Paused)

	saved, _ := db.List(ctx)
	t.Logf("STORE   %d row(s); gid=%d name=%q paused=%v", len(saved), saved[0].ID, saved[0].Name, saved[0].Paused)

	if err := eng.Remove(ctx, id, false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	saved, _ = db.List(ctx)
	t.Logf("LIFECYCLE removed -> %d row(s) left", len(saved))
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var v any
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "<non-json>"
	}
	out, _ := json.Marshal(v)
	return string(out)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
