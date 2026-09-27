package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/moshen/bittrench/internal/api"
	"github.com/moshen/bittrench/internal/config"
	"github.com/moshen/bittrench/internal/engine"
	"github.com/moshen/bittrench/internal/rpc"
	"github.com/moshen/bittrench/internal/store"
)

// stubEngine satisfies both HTTP layers' views of the engine.
type stubEngine struct{ torrents []engine.Status }

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
func (s *stubEngine) SelectedBytes(int64) (int64, int64, int)          { return 0, 0, 0 }
func (s *stubEngine) SetLabels(context.Context, int64, []string) error { return nil }

func (s *stubEngine) SetSeedLimits(context.Context, int64, store.SeedLimits) error { return nil }
func (s *stubEngine) Peers(int64) []engine.Peer                                    { return nil }
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
	srv, err := New(&cfg, &stubEngine{torrents: []engine.Status{{ID: 1, Name: "one"}}})
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
		t.Errorf("torrents = %+v", payload.Torrents)
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
