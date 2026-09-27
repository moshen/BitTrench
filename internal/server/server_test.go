package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/moshen/bittrench/internal/api"
	"github.com/moshen/bittrench/internal/config"
	"github.com/moshen/bittrench/internal/engine"
	"github.com/moshen/bittrench/internal/rpc"
	"github.com/moshen/bittrench/internal/store"
)

// stubEngine satisfies both HTTP layers' views of the engine.
type stubEngine struct {
	torrents []engine.Status
	peers    []engine.Peer
}

func (s *stubEngine) Add(context.Context, engine.AddRequest) (int64, bool, error) {
	return 1, false, nil
}
func (s *stubEngine) Status(id int64) (engine.Status, bool) {
	for _, t := range s.torrents {
		if t.ID == id {
			return t, true
		}
	}
	return engine.Status{}, false
}
func (s *stubEngine) List() []engine.Status                            { return s.torrents }
func (s *stubEngine) Files(int64) []engine.File                        { return nil }
func (s *stubEngine) SetLabels(context.Context, int64, []string) error { return nil }

func (s *stubEngine) SetSeedLimits(context.Context, int64, store.SeedLimits) error { return nil }
func (s *stubEngine) MoveInQueue(context.Context, int64, engine.Move) error        { return nil }
func (s *stubEngine) Peers(int64) []engine.Peer                                    { return s.peers }
func (s *stubEngine) AnnounceURLs(int64) []string                                  { return nil }
func (s *stubEngine) Bitfield(int64) ([]byte, int)                                 { return nil, 0 }
func (s *stubEngine) SetFileSelection(context.Context, int64, []bool) error        { return nil }
func (s *stubEngine) Start(context.Context, int64) error                           { return nil }
func (s *stubEngine) Stop(context.Context, int64) error                            { return nil }
func (s *stubEngine) Remove(context.Context, int64, bool) error                    { return nil }
func (s *stubEngine) SessionRates() (float64, float64)                             { return 0, 0 }

func start(t *testing.T, customise func(*config.AppConfig)) (*Server, string) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Torrent.SavePath = t.TempDir()
	// Port 0: the test must not fight whatever is on 6800.
	cfg.API.ListenPort = 0
	if customise != nil {
		customise(&cfg)
	}
	srv, err := New(&cfg, &stubEngine{torrents: []engine.Status{{
		ID: 1, Name: "one", State: engine.StateQueued, Queued: true, QueuePosition: 2,
		TotalBytes: 1000, CompletedBytes: 400, SizeWhenDone: 800, LeftUntilDone: 400,
		// The tail of a smoothed rate, as the sampler used to hand it over.
		DownloadRate: 0.0000025947061343373236, UploadRate: 1234.56789,
	}}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	return srv, "http://" + srv.Addr().String()
}

// The listener is the one host socket the daemon opens on purpose, and it must
// stay on the configured interface.
func TestListensOnLocalhostOnly(t *testing.T) {
	srv, _ := start(t, nil)
	addr := srv.Addr().String()
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("listening on %s, want a 127.0.0.1 address by default", addr)
	}
}

func TestServesTheWebUI(t *testing.T) {
	_, base := start(t, nil)
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "<html") {
		t.Errorf("GET / did not return the UI: %.120s", body)
	}
	// Stale cached JS once resurrected bugs that had already been fixed.
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

func TestServesTheNativeAPI(t *testing.T) {
	_, base := start(t, nil)
	resp, err := http.Get(base + api.Prefix + "/torrents")
	if err != nil {
		t.Fatalf("GET torrents: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var payload struct {
		Torrents []api.Torrent `json:"torrents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(payload.Torrents) != 1 || payload.Torrents[0].Name != "one" {
		t.Fatalf("torrents = %+v", payload.Torrents)
	}
	// The web UI takes progress from the wanted figures and names the queue
	// position, so all three have to reach it.
	got := payload.Torrents[0]
	if got.SizeWhenDone != 800 || got.LeftUntilDone != 400 {
		t.Errorf("wanted figures = %d/%d, want 800/400", got.LeftUntilDone, got.SizeWhenDone)
	}
	if got.QueuePosition != 2 {
		t.Errorf("queue_position = %d, want 2", got.QueuePosition)
	}
	if got.State != string(engine.StateQueued) {
		t.Errorf("state = %q, want %q", got.State, engine.StateQueued)
	}
	// Rates go out rounded. A client that prints what it is given must not be
	// handed 0.0000025947061343373236 to render as a speed.
	if got.DownloadRate != 0 {
		t.Errorf("download_rate = %v, want 0: the tail of a decayed rate is not a speed",
			got.DownloadRate)
	}
	if got.UploadRate != 1234.57 {
		t.Errorf("upload_rate = %v, want 1234.57", got.UploadRate)
	}
}

// The Transmission endpoint must keep its 409 handshake when mounted next to
// everything else - a catch-all route swallowing it would break Sonarr.
func TestTransmissionHandshakeSurvivesTheMux(t *testing.T) {
	_, base := start(t, nil)
	resp, err := http.Get(base + rpc.Path)
	if err != nil {
		t.Fatalf("GET rpc: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("GET %s = %d, want 409", rpc.Path, resp.StatusCode)
	}
	if resp.Header.Get("X-Transmission-Session-Id") == "" {
		t.Error("no session token on the handshake")
	}
}

func TestBasicAuthGuardsTheUIAndAPI(t *testing.T) {
	_, base := start(t, func(c *config.AppConfig) {
		c.API.Username = "user"
		c.API.Password = "pass"
	})

	for _, path := range []string{"/", api.Prefix + "/torrents"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("unauthenticated GET %s = %d, want 401", path, resp.StatusCode)
		}

		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("user:pass")))
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("authenticated GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("authenticated GET %s = %d, want 200", path, resp.StatusCode)
		}
	}
}

// Every asset the page asks for must be served, with a media type a browser
// will accept.
//
// The references are read out of index.html rather than listed here, because
// the bug this pins was a mismatch between the two: the page asked for
// /web/styles.css and /web/app.js while the assets were mounted at the root, so
// both 404'd. A 404 carries text/plain and nosniff, which is what a browser
// reports as a MIME type mismatch, and the UI rendered unstyled and inert.
func TestEveryAssetThePageReferencesIsServed(t *testing.T) {
	_, base := start(t, nil)

	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	page, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the page: %v", err)
	}

	refs := regexp.MustCompile(`(?:href|src)="(/[^"]+)"`).FindAllStringSubmatch(string(page), -1)
	if len(refs) < 3 {
		t.Fatalf("found %d local references in index.html, expected the stylesheet, "+
			"the script and the favicon at least", len(refs))
	}

	wantType := map[string]string{
		".css": "text/css",
		".js":  "text/javascript",
		".svg": "image/svg+xml",
	}
	for _, ref := range refs {
		path := ref[1]
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(base + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s returned %d, so the page cannot load it", path, resp.StatusCode)
			}
			want, ok := wantType[strings.ToLower(filepath.Ext(path))]
			if !ok {
				return
			}
			if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, want) {
				t.Errorf("%s served as %q, want %q: a browser with nosniff refuses it",
					path, got, want)
			}
		})
	}
}

// A peer whose rate is not a number must not cost the whole response.
//
// anacrolix divides bytes by an elapsed time that can be zero, so a peer can
// report +Inf or NaN. encoding/json refuses both, and the API used to have
// already sent 200 and the JSON content type by then: the body came out empty
// and the UI reported "Peers unavailable: JSON.parse: unexpected end of data at
// line 1 column 1".
func TestPeersSurviveANonFiniteRate(t *testing.T) {
	cfg := config.Defaults()
	cfg.Torrent.SavePath = t.TempDir()
	cfg.API.ListenPort = 0
	srv, err := New(&cfg, &stubEngine{
		torrents: []engine.Status{{ID: 1, Name: "one"}},
		peers: []engine.Peer{
			{Addr: "10.0.0.5:6881", Client: "qBittorrent/5.1.0",
				DownloadRate: math.Inf(1), UploadRate: math.NaN()},
			{Addr: "10.0.0.6:6881", DownloadRate: 1234.5678},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	resp, err := http.Get("http://" + srv.Addr().String() + api.Prefix + "/torrents/1/peers")
	if err != nil {
		t.Fatalf("GET peers: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	if len(body) == 0 {
		t.Fatal("empty body: this is the bug, and the client cannot parse it")
	}

	var payload struct {
		Peers []api.Peer `json:"peers"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("the response is not JSON: %v (%q)", err, body)
	}
	if len(payload.Peers) != 2 {
		t.Fatalf("peers = %+v, want 2", payload.Peers)
	}
	if payload.Peers[0].DownloadRate != 0 || payload.Peers[0].UploadRate != 0 {
		t.Errorf("a non-finite rate came through as %v/%v, want 0/0",
			payload.Peers[0].DownloadRate, payload.Peers[0].UploadRate)
	}
	if payload.Peers[1].DownloadRate != 1234.57 {
		t.Errorf("a real rate = %v, want it rounded to 1234.57", payload.Peers[1].DownloadRate)
	}
}
