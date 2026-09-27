package rpc

// These tests are the specification for what Sonarr and Radarr require. Each
// pins a contract that broke something real:
//
//   - The GET pre-flight must return 409 with X-Transmission-Session-Id.
//     Sonarr's AuthenticateClient starts with a GET and treats anything else -
//     including a 405 from a POST-only route - as a
//     DownloadClientAuthenticationException.
//   - POSTs must round-trip that token, with 409 and a fresh token otherwise.
//   - session-get must report a version Radarr's regex accepts (>= 2.40) and a
//     usable download-dir, or Sonarr's GetDownloadDirectory returns null and
//     per-category paths silently break.
//   - ids accept integers *and* strings, in every method. torrent-get once
//     accepted only strings, so {"ids":[0]} matched nothing at all.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/moshen/bittrench/internal/config"
	"github.com/moshen/bittrench/internal/engine"
)

// fakeEngine is the RPC layer's collaborator, so the endpoint can be tested
// without a tunnel or a swarm.
type fakeEngine struct {
	torrents []engine.Status
	files    map[int64][]engine.File
	added    []engine.AddRequest
	removed  map[int64]bool
	stopped  map[int64]bool
	started  map[int64]bool
	addErr   error
	down, up float64
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{
		files:   map[int64][]engine.File{},
		removed: map[int64]bool{},
		stopped: map[int64]bool{},
		started: map[int64]bool{},
	}
}

func (f *fakeEngine) Add(_ context.Context, req engine.AddRequest) (int64, error) {
	if f.addErr != nil {
		return 0, f.addErr
	}
	f.added = append(f.added, req)
	id := int64(len(f.torrents) + 1)
	dir := req.DownloadDir
	if dir == "" {
		dir = "/data"
	}
	f.torrents = append(f.torrents, engine.Status{
		ID: id, Name: "added", State: engine.StateInitialising,
		SavePath: dir, Paused: req.Paused, AddedAt: time.Now(),
	})
	return id, nil
}

func (f *fakeEngine) Status(id int64) (engine.Status, bool) {
	for _, s := range f.torrents {
		if s.ID == id {
			return s, true
		}
	}
	return engine.Status{}, false
}

func (f *fakeEngine) List() []engine.Status { return f.torrents }

func (f *fakeEngine) Files(id int64) []engine.File { return f.files[id] }

// SelectedBytes mirrors the engine's own implementation: only the files the
// torrent wants are counted.
func (f *fakeEngine) SelectedBytes(id int64) (completed, total int64, files int) {
	for _, file := range f.files[id] {
		if !file.Selected {
			continue
		}
		completed += file.Completed
		total += file.Length
		files++
	}
	return completed, total, files
}

func (f *fakeEngine) Start(_ context.Context, id int64) error {
	f.started[id] = true
	return nil
}

func (f *fakeEngine) Stop(_ context.Context, id int64) error {
	f.stopped[id] = true
	return nil
}

func (f *fakeEngine) Remove(_ context.Context, id int64, deleteData bool) error {
	f.removed[id] = deleteData
	return nil
}

func (f *fakeEngine) SessionRates() (float64, float64) { return f.down, f.up }

func testConfig() *config.AppConfig {
	cfg := config.Defaults()
	cfg.Torrent.SavePath = "/data/torrents"
	cfg.Torrent.Limits.SeedRatioLimit = 2.0
	cfg.Torrent.Limits.SeedTimeLimitSecs = 7200
	return &cfg
}

func newHandler(t *testing.T, eng Torrents, customise func(*config.AppConfig)) *Handler {
	t.Helper()
	cfg := testConfig()
	if customise != nil {
		customise(cfg)
	}
	h, err := New(eng, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

// do issues a request against the handler, echoing the session token unless
// asked not to.
func do(t *testing.T, h *Handler, method, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == nil {
		reader = strings.NewReader("")
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = strings.NewReader(string(raw))
	}
	req := httptest.NewRequest(method, Path, reader)
	if token != "" {
		req.Header.Set("X-Transmission-Session-Id", token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// call performs a successful RPC and returns the decoded arguments.
func call(t *testing.T, h *Handler, method string, arguments any) (map[string]any, string) {
	t.Helper()
	body := map[string]any{"method": method}
	if arguments != nil {
		body["arguments"] = arguments
	}
	rec := do(t, h, http.MethodPost, h.sessionID, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s returned HTTP %d: %s", method, rec.Code, rec.Body.String())
	}
	var resp struct {
		Result    string         `json:"result"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding %s response: %v (%s)", method, err, rec.Body.String())
	}
	return resp.Arguments, resp.Result
}

func TestGetProbeReturns409WithSessionID(t *testing.T) {
	h := newHandler(t, newFakeEngine(), nil)
	rec := do(t, h, http.MethodGet, "", nil)

	if rec.Code != http.StatusConflict {
		t.Fatalf("GET returned %d, want 409 - Sonarr's auth flow depends on it", rec.Code)
	}
	if got := rec.Header().Get("X-Transmission-Session-Id"); got == "" {
		t.Error("the 409 carried no X-Transmission-Session-Id")
	} else if got != h.sessionID {
		t.Errorf("token = %q, want %q", got, h.sessionID)
	}
}

func TestPostWithoutTokenReturns409AndDoesNotExecute(t *testing.T) {
	eng := newFakeEngine()
	h := newHandler(t, eng, nil)

	rec := do(t, h, http.MethodPost, "", map[string]any{
		"method":    "torrent-add",
		"arguments": map[string]any{"filename": "magnet:?xt=urn:btih:0102030405060708090a0b0c0d0e0f1011121314"},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("POST without a token returned %d, want 409", rec.Code)
	}
	if rec.Header().Get("X-Transmission-Session-Id") != h.sessionID {
		t.Error("the 409 did not carry the current token")
	}
	if len(eng.added) != 0 {
		t.Error("the method ran despite the missing token")
	}
}

func TestPostWithStaleTokenReturns409(t *testing.T) {
	h := newHandler(t, newFakeEngine(), nil)
	rec := do(t, h, http.MethodPost, "stale-token", map[string]any{"method": "session-get"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("a stale token returned %d, want 409", rec.Code)
	}
	if rec.Header().Get("X-Transmission-Session-Id") != h.sessionID {
		t.Error("the 409 did not carry a fresh token")
	}
}

func TestPostWithCorrectTokenProceeds(t *testing.T) {
	h := newHandler(t, newFakeEngine(), nil)
	args, result := call(t, h, "session-get", nil)
	if result != "success" {
		t.Fatalf("result = %q", result)
	}
	if args["version"] == nil {
		t.Error("session-get returned no version")
	}
}

// Radarr regex-parses the version and requires >= 2.40.
func TestSessionGetReportsWhatTheArrClientsRead(t *testing.T) {
	h := newHandler(t, newFakeEngine(), nil)
	args, _ := call(t, h, "session-get", nil)

	version, _ := args["version"].(string)
	major, minor := parseVersion(t, version)
	if major < 2 || (major == 2 && minor < 40) {
		t.Errorf("version = %q, which Radarr will reject as < 2.40", version)
	}
	if got := args["rpc-version"]; got != float64(rpcVersion) {
		t.Errorf("rpc-version = %v, want %d", got, rpcVersion)
	}
	if got := args["download-dir"]; got != "/data/torrents" {
		t.Errorf("download-dir = %v; Sonarr's per-category paths break without it", got)
	}
	if got := args["seedRatioLimit"]; got != 2.0 {
		t.Errorf("seedRatioLimit = %v", got)
	}
	if got := args["seedRatioLimited"]; got != true {
		t.Errorf("seedRatioLimited = %v", got)
	}
	// Transmission reports the idle limit in minutes; ours is seconds.
	if got := args["idle-seeding-limit"]; got != float64(120) {
		t.Errorf("idle-seeding-limit = %v, want 120 minutes for 7200 seconds", got)
	}
	if got := args["idle-seeding-limit-enabled"]; got != true {
		t.Errorf("idle-seeding-limit-enabled = %v", got)
	}
}

func TestSessionGetDisablesLimitsAtZero(t *testing.T) {
	h := newHandler(t, newFakeEngine(), func(c *config.AppConfig) {
		c.Torrent.Limits.SeedRatioLimit = 0
		c.Torrent.Limits.SeedTimeLimitSecs = 0
	})
	args, _ := call(t, h, "session-get", nil)
	if args["seedRatioLimited"] != false {
		t.Error("a zero ratio limit should report seedRatioLimited false")
	}
	if args["idle-seeding-limit-enabled"] != false {
		t.Error("a zero time limit should report idle-seeding-limit-enabled false")
	}
}

// The ids field accepts integers and strings, in every method. This is the bug
// that made {"ids":[0]} match nothing.
func TestIDsAcceptIntegersAndStrings(t *testing.T) {
	for _, c := range []struct {
		name string
		ids  any
		want []int64
	}{
		{"integers", []any{1, 3}, []int64{1, 3}},
		{"strings", []any{"1", "3"}, []int64{1, 3}},
		{"mixed", []any{1, "3"}, []int64{1, 3}},
		{"a bare integer", 2, []int64{2}},
		{"a bare string", "2", []int64{2}},
	} {
		t.Run(c.name, func(t *testing.T) {
			eng := newFakeEngine()
			for i := int64(1); i <= 3; i++ {
				eng.torrents = append(eng.torrents, engine.Status{ID: i, Name: fmt.Sprintf("t%d", i)})
			}
			h := newHandler(t, eng, nil)

			args, result := call(t, h, "torrent-get", map[string]any{
				"ids": c.ids, "fields": []string{"id"},
			})
			if result != "success" {
				t.Fatalf("result = %q", result)
			}
			list, _ := args["torrents"].([]any)
			if len(list) != len(c.want) {
				t.Fatalf("matched %d torrents, want %d", len(list), len(c.want))
			}
			for i, want := range c.want {
				got := list[i].(map[string]any)["id"]
				if got != float64(want) {
					t.Errorf("torrent %d id = %v, want %d", i, got, want)
				}
			}

			// The same parsing must hold for stop, start and remove.
			call(t, h, "torrent-stop", map[string]any{"ids": c.ids})
			for _, want := range c.want {
				if !eng.stopped[want] {
					t.Errorf("torrent-stop did not stop %d", want)
				}
			}
		})
	}
}

// No ids at all means every torrent.
func TestNoIDsMeansAllTorrents(t *testing.T) {
	eng := newFakeEngine()
	eng.torrents = []engine.Status{{ID: 1}, {ID: 2}}
	h := newHandler(t, eng, nil)

	args, _ := call(t, h, "torrent-get", map[string]any{"fields": []string{"id"}})
	if list, _ := args["torrents"].([]any); len(list) != 2 {
		t.Errorf("no ids matched %d torrents, want all 2", len(list))
	}
}

// When fields is present, only those keys come back.
func TestFieldProjection(t *testing.T) {
	eng := newFakeEngine()
	eng.torrents = []engine.Status{{ID: 1, Name: "release", TotalBytes: 100, CompletedBytes: 50}}
	h := newHandler(t, eng, nil)

	args, _ := call(t, h, "torrent-get", map[string]any{"fields": []string{"id", "name"}})
	list, _ := args["torrents"].([]any)
	if len(list) != 1 {
		t.Fatalf("got %d torrents", len(list))
	}
	got := list[0].(map[string]any)
	if len(got) != 2 {
		t.Errorf("projection returned %d keys, want exactly the 2 requested: %v", len(got), got)
	}
	if got["name"] != "release" {
		t.Errorf("name = %v", got["name"])
	}
	if _, present := got["totalSize"]; present {
		t.Error("an unrequested field was returned")
	}
}

// name and hashString are how the *arr clients match a download back to the
// release they requested, so neither may be absent or stubbed.
func TestTorrentGetReportsNameAndHash(t *testing.T) {
	var hash metainfo.Hash
	hash[0], hash[19] = 0xab, 0xcd
	eng := newFakeEngine()
	eng.torrents = []engine.Status{{ID: 1, Name: "Some.Release", InfoHash: hash}}
	h := newHandler(t, eng, nil)

	args, _ := call(t, h, "torrent-get", nil)
	got := args["torrents"].([]any)[0].(map[string]any)
	if got["name"] != "Some.Release" {
		t.Errorf("name = %v, want the torrent name", got["name"])
	}
	if got["hashString"] != hash.HexString() {
		t.Errorf("hashString = %v, want %s", got["hashString"], hash.HexString())
	}
}

func TestStatusMapping(t *testing.T) {
	for _, c := range []struct {
		state engine.State
		want  float64
	}{
		{engine.StateDownloading, trStatusDownloading},
		{engine.StateSeeding, trStatusSeeding},
		{engine.StateStopped, trStatusStopped},
		{engine.StateInitialising, trStatusCheckWait},
		{engine.StateError, trStatusStopped},
	} {
		t.Run(string(c.state), func(t *testing.T) {
			eng := newFakeEngine()
			eng.torrents = []engine.Status{{ID: 1, State: c.state}}
			h := newHandler(t, eng, nil)
			args, _ := call(t, h, "torrent-get", map[string]any{"fields": []string{"status"}})
			got := args["torrents"].([]any)[0].(map[string]any)["status"]
			if got != c.want {
				t.Errorf("state %q mapped to %v, want %v", c.state, got, c.want)
			}
		})
	}
}

// A torrent-level failure is reported through error/errorString, with
// TR_STAT_LOCAL_ERROR and a stopped status alongside it.
func TestErrorsSurfaceAsLocalError(t *testing.T) {
	eng := newFakeEngine()
	eng.torrents = []engine.Status{
		{ID: 1, State: engine.StateDownloading},
		{ID: 2, State: engine.StateError, Error: "no files match the allow-list"},
	}
	h := newHandler(t, eng, nil)
	args, _ := call(t, h, "torrent-get", nil)
	list := args["torrents"].([]any)

	healthy := list[0].(map[string]any)
	if healthy["error"] != float64(0) || healthy["errorString"] != "" {
		t.Errorf("a healthy torrent reported error=%v %q", healthy["error"], healthy["errorString"])
	}
	failed := list[1].(map[string]any)
	if failed["error"] != float64(trStatLocalError) {
		t.Errorf("error = %v, want %d", failed["error"], trStatLocalError)
	}
	if failed["errorString"] != "no files match the allow-list" {
		t.Errorf("errorString = %v", failed["errorString"])
	}
	if failed["status"] != float64(trStatusStopped) {
		t.Errorf("a failed torrent's status = %v, want stopped", failed["status"])
	}
}

// download-dir on torrent-add is how *arr category subdirectories work, so the
// argument has to be honoured rather than accepted and dropped.
func TestTorrentAddHonoursDownloadDir(t *testing.T) {
	eng := newFakeEngine()
	h := newHandler(t, eng, nil)

	_, result := call(t, h, "torrent-add", map[string]any{
		"filename":     "magnet:?xt=urn:btih:0102030405060708090a0b0c0d0e0f1011121314",
		"download-dir": "/data/tv",
	})
	if result != "success" {
		t.Fatalf("result = %q", result)
	}
	if len(eng.added) != 1 {
		t.Fatalf("the engine saw %d adds", len(eng.added))
	}
	if eng.added[0].DownloadDir != "/data/tv" {
		t.Errorf("download-dir reached the engine as %q", eng.added[0].DownloadDir)
	}

	// And it must be reported back per torrent, not as the session path.
	args, _ := call(t, h, "torrent-get", map[string]any{"fields": []string{"downloadDir"}})
	got := args["torrents"].([]any)[0].(map[string]any)["downloadDir"]
	if got != "/data/tv" {
		t.Errorf("downloadDir = %v, want the torrent's own directory", got)
	}
}

func TestTorrentAddAcceptsBase64Metainfo(t *testing.T) {
	eng := newFakeEngine()
	h := newHandler(t, eng, nil)
	blob := []byte("d8:announce3:abce")

	_, result := call(t, h, "torrent-add", map[string]any{
		"metainfo": base64.StdEncoding.EncodeToString(blob),
	})
	if result != "success" {
		t.Fatalf("result = %q", result)
	}
	if len(eng.added) != 1 || string(eng.added[0].Metainfo) != string(blob) {
		t.Errorf("metainfo did not reach the engine intact: %+v", eng.added)
	}
}

func TestTorrentAddRejectsBadBase64(t *testing.T) {
	h := newHandler(t, newFakeEngine(), nil)
	_, result := call(t, h, "torrent-add", map[string]any{"metainfo": "!!!not base64!!!"})
	if !strings.Contains(result, "base64") {
		t.Errorf("result = %q, want it to mention base64", result)
	}
}

func TestTorrentAddRequiresASource(t *testing.T) {
	h := newHandler(t, newFakeEngine(), nil)
	_, result := call(t, h, "torrent-add", map[string]any{"download-dir": "/data"})
	if !strings.Contains(result, "filename") {
		t.Errorf("result = %q, want it to name the missing argument", result)
	}
}

// torrent-add answers with torrent-added, which is where clients read the id.
func TestTorrentAddReturnsTheID(t *testing.T) {
	h := newHandler(t, newFakeEngine(), nil)
	args, _ := call(t, h, "torrent-add", map[string]any{
		"filename": "magnet:?xt=urn:btih:0102030405060708090a0b0c0d0e0f1011121314",
	})
	added, ok := args["torrent-added"].(map[string]any)
	if !ok {
		t.Fatalf("no torrent-added in the response: %v", args)
	}
	if added["id"] != float64(1) {
		t.Errorf("id = %v, want 1", added["id"])
	}
	if _, present := added["hashString"]; !present {
		t.Error("torrent-added carried no hashString")
	}
}

func TestTorrentRemovePassesDeleteLocalData(t *testing.T) {
	eng := newFakeEngine()
	eng.torrents = []engine.Status{{ID: 1}, {ID: 2}}
	h := newHandler(t, eng, nil)

	call(t, h, "torrent-remove", map[string]any{"ids": []any{2}, "delete-local-data": true})
	if _, removed := eng.removed[1]; removed {
		t.Error("torrent-remove removed a torrent that was not selected")
	}
	if deleteData, removed := eng.removed[2]; !removed || !deleteData {
		t.Errorf("torrent 2 removed=%v deleteData=%v", removed, deleteData)
	}
}

func TestTorrentStartAndStop(t *testing.T) {
	eng := newFakeEngine()
	eng.torrents = []engine.Status{{ID: 1}, {ID: 2}}
	h := newHandler(t, eng, nil)

	call(t, h, "torrent-stop", map[string]any{"ids": []any{1}})
	if !eng.stopped[1] || eng.stopped[2] {
		t.Errorf("stopped = %v, want only torrent 1", eng.stopped)
	}
	call(t, h, "torrent-start", map[string]any{"ids": []any{2}})
	if !eng.started[2] || eng.started[1] {
		t.Errorf("started = %v, want only torrent 2", eng.started)
	}
}

// Files must only be fetched when asked for - Radarr polls torrent-get
// constantly, and on a fresh magnet there are none.
func TestFilesAreOnlyFetchedWhenRequested(t *testing.T) {
	eng := newFakeEngine()
	eng.torrents = []engine.Status{{ID: 1, HasMetadata: true}}
	eng.files[1] = []engine.File{
		{Index: 0, Path: "movie.mkv", Length: 100, Completed: 50, Selected: true},
		{Index: 1, Path: "extra.nfo", Length: 10, Selected: false},
	}
	h := newHandler(t, eng, nil)

	args, _ := call(t, h, "torrent-get", map[string]any{"fields": []string{"files", "wanted"}})
	got := args["torrents"].([]any)[0].(map[string]any)
	files, _ := got["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("files = %v", got["files"])
	}
	first := files[0].(map[string]any)
	if first["name"] != "movie.mkv" || first["length"] != float64(100) || first["bytesCompleted"] != float64(50) {
		t.Errorf("file 0 = %v", first)
	}
	wanted, _ := got["wanted"].([]any)
	if len(wanted) != 2 || wanted[0] != float64(1) || wanted[1] != float64(0) {
		t.Errorf("wanted = %v, want [1 0]", wanted)
	}
}

// A magnet whose metadata has not arrived must answer torrent-get with
// fields:["files"] without failing - this is the most common path in the
// system and it used to be a nil dereference.
func TestTorrentGetWithFilesOnAFreshMagnet(t *testing.T) {
	eng := newFakeEngine()
	eng.torrents = []engine.Status{{ID: 1, Name: "pending", HasMetadata: false}}
	h := newHandler(t, eng, nil)

	args, result := call(t, h, "torrent-get", map[string]any{"fields": []string{"id", "name", "files"}})
	if result != "success" {
		t.Fatalf("result = %q", result)
	}
	got := args["torrents"].([]any)[0].(map[string]any)
	if files, _ := got["files"].([]any); len(files) != 0 {
		t.Errorf("files = %v, want empty", files)
	}
	if got["name"] != "pending" {
		t.Errorf("name = %v", got["name"])
	}
}

func TestSessionStats(t *testing.T) {
	eng := newFakeEngine()
	eng.down, eng.up = 1024.7, 2048.2
	eng.torrents = []engine.Status{{ID: 1}, {ID: 2, Paused: true}}
	h := newHandler(t, eng, nil)

	args, _ := call(t, h, "session-stats", nil)
	if args["downloadSpeed"] != float64(1024) {
		t.Errorf("downloadSpeed = %v, want 1024", args["downloadSpeed"])
	}
	if args["uploadSpeed"] != float64(2048) {
		t.Errorf("uploadSpeed = %v, want 2048", args["uploadSpeed"])
	}
	if args["activeTorrentCount"] != float64(1) {
		t.Errorf("activeTorrentCount = %v, want 1", args["activeTorrentCount"])
	}
	if args["pausedTorrentCount"] != float64(1) {
		t.Errorf("pausedTorrentCount = %v, want 1", args["pausedTorrentCount"])
	}
}

// An unimplemented method is reported in `result` with HTTP 200 - an HTTP
// error would make the *arr clients drop the connection instead of surfacing
// the message.
func TestUnknownMethodIsReportedInResult(t *testing.T) {
	h := newHandler(t, newFakeEngine(), nil)
	rec := do(t, h, http.MethodPost, h.sessionID, map[string]any{"method": "torrent-set-location"})
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d, want 200", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if got, _ := resp["result"].(string); !strings.Contains(got, "method not implemented") {
		t.Errorf("result = %q", got)
	}
}

// The tag a client sends must come back on the response it belongs to.
func TestTagIsEchoed(t *testing.T) {
	h := newHandler(t, newFakeEngine(), nil)
	rec := do(t, h, http.MethodPost, h.sessionID, map[string]any{"method": "session-get", "tag": 42})
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["tag"] != float64(42) {
		t.Errorf("tag = %v, want 42", resp["tag"])
	}
}

func TestBasicAuth(t *testing.T) {
	eng := newFakeEngine()
	h := newHandler(t, eng, func(c *config.AppConfig) {
		c.API.Username = "user"
		c.API.Password = "pass"
	})

	// No credentials: 401 with a challenge, on the GET probe too - otherwise
	// the client cannot tell auth-required from a broken endpoint.
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		rec := do(t, h, method, h.sessionID, map[string]any{"method": "session-get"})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without credentials = %d, want 401", method, rec.Code)
		}
		if !strings.Contains(rec.Header().Get("WWW-Authenticate"), "Basic") {
			t.Errorf("%s: no Basic challenge", method)
		}
	}

	// Wrong credentials: still 401.
	req := httptest.NewRequest(http.MethodGet, Path, nil)
	req.SetBasicAuth("user", "wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong password = %d, want 401", rec.Code)
	}

	// Correct credentials: the GET probe gets its 409 handshake.
	req = httptest.NewRequest(http.MethodGet, Path, nil)
	req.SetBasicAuth("user", "pass")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("authenticated GET = %d, want 409", rec.Code)
	}
	if rec.Header().Get("X-Transmission-Session-Id") == "" {
		t.Error("the authenticated probe carried no session token")
	}
}

// Stale cached JS once resurrected bugs that had already been fixed.
func TestResponsesAreNotCacheable(t *testing.T) {
	h := newHandler(t, newFakeEngine(), nil)
	rec := do(t, h, http.MethodPost, h.sessionID, map[string]any{"method": "session-get"})
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

// eta is -1 for unknown, which is what Transmission clients expect.
func TestETAAndPercentDone(t *testing.T) {
	eng := newFakeEngine()
	eng.torrents = []engine.Status{
		{ID: 1, HasMetadata: true, TotalBytes: 1000, CompletedBytes: 250, MissingBytes: 750, DownloadRate: 75},
		{ID: 2, HasMetadata: true, TotalBytes: 1000, CompletedBytes: 1000},
	}
	h := newHandler(t, eng, nil)

	args, _ := call(t, h, "torrent-get", map[string]any{"fields": []string{"eta", "percentDone", "isFinished", "leftUntilDone"}})
	list := args["torrents"].([]any)

	downloading := list[0].(map[string]any)
	if downloading["eta"] != float64(10) {
		t.Errorf("eta = %v, want 10", downloading["eta"])
	}
	if downloading["percentDone"] != 0.25 {
		t.Errorf("percentDone = %v, want 0.25", downloading["percentDone"])
	}
	if downloading["isFinished"] != false {
		t.Errorf("isFinished = %v", downloading["isFinished"])
	}
	if downloading["leftUntilDone"] != float64(750) {
		t.Errorf("leftUntilDone = %v", downloading["leftUntilDone"])
	}

	done := list[1].(map[string]any)
	if done["eta"] != float64(-1) {
		t.Errorf("a finished torrent's eta = %v, want -1", done["eta"])
	}
	if done["isFinished"] != true {
		t.Errorf("isFinished = %v, want true", done["isFinished"])
	}
}

func parseVersion(t *testing.T, v string) (major, minor int) {
	t.Helper()
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		t.Fatalf("version %q is not major.minor", v)
	}
	var err error
	if major, err = strconv.Atoi(parts[0]); err != nil {
		t.Fatalf("version %q: %v", v, err)
	}
	if minor, err = strconv.Atoi(parts[1]); err != nil {
		t.Fatalf("version %q: %v", v, err)
	}
	return major, minor
}

// An allow-list torrent whose selected file is complete must read as finished,
// or Sonarr never imports it: its completion tests are `leftUntilDone == 0`
// with a stopped/seeding status, and `isFinished`. Reporting the whole
// torrent's missing bytes fails both for as long as a deselected file exists,
// which is forever.
func TestFilteredTorrentReadsAsCompleteOnceSelectedFilesAre(t *testing.T) {
	f := newFakeEngine()
	f.torrents = []engine.Status{{
		ID: 1, Name: "release", State: engine.StateDownloading, HasMetadata: true,
		TotalBytes: 1000, CompletedBytes: 900, MissingBytes: 100,
	}}
	f.files[1] = []engine.File{
		{Index: 0, Path: "movie.mkv", Length: 900, Completed: 900, Selected: true},
		{Index: 1, Path: "extras.iso", Length: 100, Completed: 0, Selected: false},
	}
	h := newHandler(t, f, nil)

	args, _ := call(t, h, "torrent-get", nil)
	got := args["torrents"].([]any)[0].(map[string]any)

	for field, want := range map[string]any{
		"leftUntilDone":   float64(0),
		"sizeWhenDone":    float64(900),
		"percentDone":     float64(1),
		"isFinished":      true,
		"status":          float64(trStatusSeeding),
		"percentComplete": float64(0.9),
		"totalSize":       float64(1000),
		"eta":             float64(-1),
	} {
		if got[field] != want {
			t.Errorf("%s = %v, want %v", field, got[field], want)
		}
	}
}

// Sonarr's own completion expression, evaluated against what we send.
func TestSonarrWouldCallAFilteredTorrentComplete(t *testing.T) {
	f := newFakeEngine()
	f.torrents = []engine.Status{{
		ID: 1, State: engine.StateDownloading, HasMetadata: true,
		TotalBytes: 1000, CompletedBytes: 900, MissingBytes: 100,
	}}
	f.files[1] = []engine.File{
		{Index: 0, Length: 900, Completed: 900, Selected: true},
		{Index: 1, Length: 100, Selected: false},
	}
	args, _ := call(t, newHandler(t, f, nil), "torrent-get", nil)
	got := args["torrents"].([]any)[0].(map[string]any)

	status := got["status"]
	byLeft := got["leftUntilDone"] == float64(0) &&
		(status == float64(trStatusStopped) || status == float64(trStatusSeeding) ||
			status == float64(trStatusSeedWait))
	byFinished := got["isFinished"] == true &&
		status != float64(trStatusCheck) && status != float64(trStatusCheckWait)
	if !byLeft || !byFinished {
		t.Errorf("neither completion route fires: byLeft=%v byFinished=%v (status=%v)",
			byLeft, byFinished, status)
	}
}

// A partially-downloaded selection still reports honest progress, measured
// against the selection rather than the torrent.
func TestProgressIsMeasuredAgainstTheSelection(t *testing.T) {
	f := newFakeEngine()
	f.torrents = []engine.Status{{
		ID: 1, State: engine.StateDownloading, HasMetadata: true,
		TotalBytes: 1000, CompletedBytes: 450, MissingBytes: 550,
		DownloadRate: 100,
	}}
	f.files[1] = []engine.File{
		{Index: 0, Length: 900, Completed: 450, Selected: true},
		{Index: 1, Length: 100, Selected: false},
	}
	args, _ := call(t, newHandler(t, f, nil), "torrent-get", nil)
	got := args["torrents"].([]any)[0].(map[string]any)

	if got["leftUntilDone"] != float64(450) {
		t.Errorf("leftUntilDone = %v, want 450 (of the wanted file)", got["leftUntilDone"])
	}
	if got["percentDone"] != float64(0.5) {
		t.Errorf("percentDone = %v, want 0.5", got["percentDone"])
	}
	if got["isFinished"] != false {
		t.Error("isFinished should be false while the selection is incomplete")
	}
	// 450 wanted bytes at 100 B/s, not the 550 the whole torrent is missing.
	if got["eta"] != float64(4) {
		t.Errorf("eta = %v, want 4", got["eta"])
	}
	if got["status"] != float64(trStatusDownloading) {
		t.Errorf("status = %v, want downloading", got["status"])
	}
}

// Before metadata there is no selection to measure. Reporting a confident zero
// would make a fresh magnet look complete, so the whole-torrent figures stand
// in until the info dict resolves.
func TestPreMetadataFallsBackToTheTorrentFigures(t *testing.T) {
	f := newFakeEngine()
	f.torrents = []engine.Status{{
		ID: 1, Name: "infohash:abc", State: engine.StateInitialising,
	}}
	args, _ := call(t, newHandler(t, f, nil), "torrent-get", nil)
	got := args["torrents"].([]any)[0].(map[string]any)

	if got["isFinished"] != false {
		t.Error("a torrent with no metadata must not read as finished")
	}
	if got["percentDone"] != float64(0) {
		t.Errorf("percentDone = %v, want 0", got["percentDone"])
	}
	if got["status"] != float64(trStatusCheckWait) {
		t.Errorf("status = %v, want check-wait", got["status"])
	}
}

// Both clients send their category as a label on torrent-add whenever the
// reported version is >= 4.0, which ours is. Dropping it left them matching
// categories by directory instead.
func TestTorrentAddRecordsLabels(t *testing.T) {
	f := newFakeEngine()
	h := newHandler(t, f, nil)

	if _, result := call(t, h, "torrent-add", map[string]any{
		"filename": "magnet:?xt=urn:btih:abc",
		"labels":   []string{"tv-sonarr"},
	}); result != "success" {
		t.Fatalf("torrent-add: %s", result)
	}
	if len(f.added) != 1 {
		t.Fatalf("expected one add, got %d", len(f.added))
	}
	if got := f.added[0].Labels; len(got) != 1 || got[0] != "tv-sonarr" {
		t.Errorf("labels = %v, want [tv-sonarr]", got)
	}
}

// torrent-get must report labels, and as an array even when there are none:
// Sonarr gates its label filter on the collection being non-empty, and a null
// would be a deserialisation surprise for no benefit.
func TestTorrentGetReportsLabels(t *testing.T) {
	f := newFakeEngine()
	f.torrents = []engine.Status{
		{ID: 1, Name: "labelled", Labels: []string{"tv-sonarr"}},
		{ID: 2, Name: "bare"},
	}
	args, _ := call(t, newHandler(t, f, nil), "torrent-get",
		map[string]any{"fields": []string{"id", "labels"}})
	list := args["torrents"].([]any)

	labelled := list[0].(map[string]any)["labels"]
	if got, ok := labelled.([]any); !ok || len(got) != 1 || got[0] != "tv-sonarr" {
		t.Errorf("labels = %#v, want [tv-sonarr]", labelled)
	}
	bare := list[1].(map[string]any)["labels"]
	if got, ok := bare.([]any); !ok || len(got) != 0 {
		t.Errorf("a torrent with no labels reported %#v, want []", bare)
	}
}
