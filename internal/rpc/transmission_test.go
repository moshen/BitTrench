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
//   - ids accept integers *and* info-hashes, in every method. torrent-get
//     once accepted only numeric strings, so {"ids":[0]} matched nothing at
//     all; and hashes, which Sonarr sends to torrent-remove, torrent-set and
//     queue-move-top, were once rejected as "invalid torrent id".

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
	"github.com/moshen/bittrench/internal/store"
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
	// labelsSet and selectionSet record what torrent-set pushed down, so the
	// tests assert on the engine call rather than on a round-trip.
	labelsSet     map[int64][]string
	selectionSet  map[int64][]bool
	seedLimitsSet map[int64]store.SeedLimits
	setLabelsErr  error
	// addDuplicate makes Add report the infohash as already managed, which is
	// what makes Transmission answer torrent-duplicate.
	addDuplicate bool
	duplicateID  int64
	// moves records queue-move calls in the order they were made, which is the
	// part that matters when a move covers several torrents.
	moves []queueMoveCall
}

type queueMoveCall struct {
	ID   int64
	Move engine.Move
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{
		files:         map[int64][]engine.File{},
		removed:       map[int64]bool{},
		stopped:       map[int64]bool{},
		started:       map[int64]bool{},
		labelsSet:     map[int64][]string{},
		selectionSet:  map[int64][]bool{},
		seedLimitsSet: map[int64]store.SeedLimits{},
	}
}

func (f *fakeEngine) Add(_ context.Context, req engine.AddRequest) (int64, bool, error) {
	if f.addErr != nil {
		return 0, false, f.addErr
	}
	if f.addDuplicate {
		return f.duplicateID, true, nil
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
	return id, false, nil
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

func (f *fakeEngine) SetLabels(_ context.Context, id int64, labels []string) error {
	if f.setLabelsErr != nil {
		return f.setLabelsErr
	}
	f.labelsSet[id] = labels
	for i := range f.torrents {
		if f.torrents[i].ID == id {
			f.torrents[i].Labels = labels
		}
	}
	return nil
}

func (f *fakeEngine) MoveInQueue(_ context.Context, id int64, move engine.Move) error {
	f.moves = append(f.moves, queueMoveCall{ID: id, Move: move})
	return nil
}

func (f *fakeEngine) SetSeedLimits(_ context.Context, id int64, limits store.SeedLimits) error {
	f.seedLimitsSet[id] = limits
	for i := range f.torrents {
		if f.torrents[i].ID == id {
			f.torrents[i].SeedLimits = limits
		}
	}
	return nil
}

func (f *fakeEngine) SetFileSelection(_ context.Context, id int64, selection []bool) error {
	f.selectionSet[id] = selection
	for i := range f.files[id] {
		if i < len(selection) {
			f.files[id][i].Selected = selection[i]
		}
	}
	return nil
}

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

// The ids field accepts integers, in every method. This is the bug that made
// {"ids":[0]} match nothing.
func TestIDsAcceptIntegers(t *testing.T) {
	for _, c := range []struct {
		name string
		ids  any
		want []int64
	}{
		{"integers", []any{1, 3}, []int64{1, 3}},
		{"a bare integer", 2, []int64{2}},
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

// Sonarr names torrents by hashString, not id, in torrent-remove, torrent-set
// and queue-move-top. Rejecting the hash failed its queue removal outright.
func TestIDsAcceptInfoHashes(t *testing.T) {
	hashes := make([]metainfo.Hash, 3)
	for i := range hashes {
		hashes[i][0], hashes[i][19] = 0xc5, byte(i)
	}
	newEngine := func() *fakeEngine {
		eng := newFakeEngine()
		for i, hash := range hashes {
			eng.torrents = append(eng.torrents, engine.Status{ID: int64(i + 1), InfoHash: hash})
		}
		return eng
	}
	second := hashes[1].HexString()

	for _, c := range []struct {
		name string
		ids  any
	}{
		{"a hash", []any{second}},
		{"an uppercase hash", []any{strings.ToUpper(second)}},
		{"a bare hash", second},
	} {
		t.Run(c.name, func(t *testing.T) {
			eng := newEngine()
			h := newHandler(t, eng, nil)

			args, result := call(t, h, "torrent-get", map[string]any{"ids": c.ids, "fields": []string{"id"}})
			if result != "success" {
				t.Fatalf("torrent-get result = %q", result)
			}
			list, _ := args["torrents"].([]any)
			if len(list) != 1 || list[0].(map[string]any)["id"] != float64(2) {
				t.Errorf("torrent-get matched %v, want only torrent 2", list)
			}

			if _, result := call(t, h, "torrent-set", map[string]any{"ids": c.ids, "labels": []string{"tv"}}); result != "success" {
				t.Fatalf("torrent-set result = %q", result)
			}
			if len(eng.labelsSet) != 1 || eng.labelsSet[2] == nil {
				t.Errorf("torrent-set labelled %v, want only torrent 2", eng.labelsSet)
			}

			if _, result := call(t, h, "queue-move-top", map[string]any{"ids": c.ids}); result != "success" {
				t.Fatalf("queue-move-top result = %q", result)
			}
			if len(eng.moves) != 1 || eng.moves[0].ID != 2 {
				t.Errorf("queue-move-top moved %v, want only torrent 2", eng.moves)
			}

			if _, result := call(t, h, "torrent-remove", map[string]any{"ids": c.ids, "delete-local-data": true}); result != "success" {
				t.Fatalf("torrent-remove result = %q", result)
			}
			if len(eng.removed) != 1 || !eng.removed[2] {
				t.Errorf("torrent-remove removed %v, want only torrent 2", eng.removed)
			}
		})
	}
}

// An ids list that names nothing present selects nothing. Only an absent ids
// key means every torrent: torrent-remove deletes data, and reading "no match"
// as "all" there would delete every download.
func TestIDsMatchingNothingSelectNothing(t *testing.T) {
	var unknown metainfo.Hash
	unknown[0] = 0xff
	for _, c := range []struct {
		name string
		ids  any
	}{
		{"an empty list", []any{}},
		{"an unknown hash", []any{unknown.HexString()}},
		{"an unknown id", []any{99}},
	} {
		t.Run(c.name, func(t *testing.T) {
			eng := newFakeEngine()
			eng.torrents = []engine.Status{{ID: 1}, {ID: 2}}
			h := newHandler(t, eng, nil)

			if _, result := call(t, h, "torrent-remove", map[string]any{"ids": c.ids, "delete-local-data": true}); result != "success" {
				t.Fatalf("result = %q", result)
			}
			if len(eng.removed) != 0 {
				t.Errorf("torrent-remove removed %v, want nothing", eng.removed)
			}
		})
	}
}

// A string that is not a hash is an error, rather than something that quietly
// matches nothing. That includes a numeric string: Transmission never reads
// "1" as torrent 1, so accepting it here would only hide a client bug.
func TestIDsRejectGarbage(t *testing.T) {
	eng := newFakeEngine()
	eng.torrents = []engine.Status{{ID: 1}, {ID: 3}}
	h := newHandler(t, eng, nil)
	for _, ids := range []any{
		[]any{"not-an-id"},
		[]any{strings.Repeat("z", 40)},
		[]any{"c5a670b7"},
		[]any{"1"},
		[]any{1, "3"},
		"1",
	} {
		if _, result := call(t, h, "torrent-get", map[string]any{"ids": ids}); !strings.Contains(result, "invalid torrent id") {
			t.Errorf("ids %v: result = %q, want an invalid torrent id error", ids, result)
		}
		if _, result := call(t, h, "torrent-remove", map[string]any{"ids": ids}); !strings.Contains(result, "invalid torrent id") {
			t.Errorf("torrent-remove ids %v: result = %q, want an invalid torrent id error", ids, result)
		}
	}
	if len(eng.removed) != 0 {
		t.Errorf("a refused torrent-remove still removed %v", eng.removed)
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

// The recently-active selector is not implemented and means every torrent, in
// both the original spec's spelling and the current snake_case one. See
// section 3.1 of
// https://github.com/transmission/transmission/blob/main/docs/rpc-spec.md
func TestRecentlyActiveMeansAllTorrents(t *testing.T) {
	for _, ids := range []any{"recently-active", "recently_active", []any{"recently_active"}} {
		t.Run(fmt.Sprint(ids), func(t *testing.T) {
			eng := newFakeEngine()
			eng.torrents = []engine.Status{{ID: 1}, {ID: 2}}
			h := newHandler(t, eng, nil)

			args, result := call(t, h, "torrent-get", map[string]any{"ids": ids, "fields": []string{"id"}})
			if result != "success" {
				t.Fatalf("result = %q", result)
			}
			if list, _ := args["torrents"].([]any); len(list) != 2 {
				t.Errorf("matched %d torrents, want all 2", len(list))
			}
		})
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
		{ID: 1, HasMetadata: true, TotalBytes: 1000, CompletedBytes: 250, MissingBytes: 750,
			SizeWhenDone: 1000, LeftUntilDone: 750, DownloadRate: 75},
		{ID: 2, HasMetadata: true, TotalBytes: 1000, CompletedBytes: 1000, SizeWhenDone: 1000},
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

// An allow-list torrent whose wanted files are complete must read as finished,
// or Sonarr never imports it: its completion tests are `leftUntilDone == 0` with
// a stopped/seeding status, and `isFinished`. The engine supplies SizeWhenDone
// and LeftUntilDone - counting only the wanted files - and this pins that the
// wire fields follow them rather than the whole torrent's figures beside them.
func TestFilteredTorrentReadsAsCompleteOnceSelectedFilesAre(t *testing.T) {
	f := newFakeEngine()
	f.torrents = []engine.Status{{
		ID: 1, Name: "release", State: engine.StateSeeding, HasMetadata: true,
		TotalBytes: 1000, CompletedBytes: 900, MissingBytes: 100,
		SizeWhenDone: 900, LeftUntilDone: 0,
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
		ID: 1, State: engine.StateSeeding, HasMetadata: true,
		TotalBytes: 1000, CompletedBytes: 900, MissingBytes: 100,
		SizeWhenDone: 900, LeftUntilDone: 0,
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
		SizeWhenDone: 900, LeftUntilDone: 450, DownloadRate: 100,
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

// torrent-set is what Sonarr calls to relabel a torrent on import. Answering
// "method not implemented" made its ProcessRequest raise a
// TransmissionException, turning a finished download into a reported failure.
func TestTorrentSetLabels(t *testing.T) {
	f := newFakeEngine()
	f.torrents = []engine.Status{{ID: 1, Labels: []string{"tv-sonarr"}}}
	h := newHandler(t, f, nil)

	_, result := call(t, h, "torrent-set", map[string]any{
		"ids":    []any{1},
		"labels": []string{"tv-sonarr-imported"},
	})
	if result != "success" {
		t.Fatalf("torrent-set returned %q, want success", result)
	}
	if got := f.labelsSet[1]; len(got) != 1 || got[0] != "tv-sonarr-imported" {
		t.Errorf("labels pushed down = %v, want [tv-sonarr-imported]", got)
	}
}

// An absent key must leave a setting alone; an empty one must clear it. Without
// the distinction, any torrent-set would wipe the labels.
func TestTorrentSetDistinguishesAbsentFromEmpty(t *testing.T) {
	f := newFakeEngine()
	f.torrents = []engine.Status{{ID: 1, Labels: []string{"keep"}}}
	h := newHandler(t, f, nil)

	if _, result := call(t, h, "torrent-set", map[string]any{"ids": []any{1}}); result != "success" {
		t.Fatalf("torrent-set: %s", result)
	}
	if _, touched := f.labelsSet[1]; touched {
		t.Error("a torrent-set with no labels key must not touch the labels")
	}

	if _, result := call(t, h, "torrent-set", map[string]any{
		"ids": []any{1}, "labels": []string{},
	}); result != "success" {
		t.Fatalf("torrent-set: %s", result)
	}
	if got, touched := f.labelsSet[1]; !touched || len(got) != 0 {
		t.Errorf("labels = %v, want an explicit clear", got)
	}
}

// files-wanted and files-unwanted are edits to the current selection, and the
// engine takes a whole selection, so the current one is the starting point.
func TestTorrentSetFileWishes(t *testing.T) {
	newEngine := func() *fakeEngine {
		f := newFakeEngine()
		f.torrents = []engine.Status{{ID: 1}}
		f.files[1] = []engine.File{
			{Index: 0, Path: "a.mkv", Length: 10, Selected: true},
			{Index: 1, Path: "b.nfo", Length: 10, Selected: true},
			{Index: 2, Path: "c.iso", Length: 10, Selected: false},
		}
		return f
	}

	tests := []struct {
		name string
		args map[string]any
		want []bool
	}{
		{
			name: "unwanted edits only the named index",
			args: map[string]any{"ids": []any{1}, "files-unwanted": []int{1}},
			want: []bool{true, false, false},
		},
		{
			name: "wanted re-selects a deselected file",
			args: map[string]any{"ids": []any{1}, "files-wanted": []int{2}},
			want: []bool{true, true, true},
		},
		{
			name: "an empty list means every file",
			args: map[string]any{"ids": []any{1}, "files-unwanted": []int{}},
			want: []bool{false, false, false},
		},
		{
			name: "wanted wins over unwanted for the same index",
			args: map[string]any{"ids": []any{1}, "files-unwanted": []int{0}, "files-wanted": []int{0}},
			want: []bool{true, true, false},
		},
		{
			name: "an out-of-range index is skipped, not an error",
			args: map[string]any{"ids": []any{1}, "files-unwanted": []int{99, -1, 0}},
			want: []bool{false, true, false},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newEngine()
			_, result := call(t, newHandler(t, f, nil), "torrent-set", tc.args)
			if result != "success" {
				t.Fatalf("torrent-set returned %q", result)
			}
			got := f.selectionSet[1]
			if len(got) != len(tc.want) {
				t.Fatalf("selection = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("selection = %v, want %v", got, tc.want)
					break
				}
			}
		})
	}
}

// Before metadata there is no file list to index into. Silently dropping the
// instruction would leave the client believing it had applied.
func TestTorrentSetFileWishesNeedMetadata(t *testing.T) {
	f := newFakeEngine()
	f.torrents = []engine.Status{{ID: 1, Name: "infohash:abc"}}
	_, result := call(t, newHandler(t, f, nil), "torrent-set",
		map[string]any{"ids": []any{1}, "files-wanted": []int{0}})
	if result == "success" {
		t.Error("expected a failure result when the torrent has no file list")
	}
}

// With no ids, torrent-set applies to every torrent, like every other method.
func TestTorrentSetWithNoIDsAppliesToAll(t *testing.T) {
	f := newFakeEngine()
	f.torrents = []engine.Status{{ID: 1}, {ID: 2}}
	if _, result := call(t, newHandler(t, f, nil), "torrent-set",
		map[string]any{"labels": []string{"all"}}); result != "success" {
		t.Fatalf("torrent-set: %s", result)
	}
	if len(f.labelsSet) != 2 {
		t.Errorf("labels applied to %d torrents, want 2", len(f.labelsSet))
	}
}

// The other half of what Sonarr sends to torrent-set: its seed criteria. It
// reads these four fields back to decide for itself when a download has seeded
// enough, so they have to round-trip.
func TestTorrentSetSeedLimits(t *testing.T) {
	f := newFakeEngine()
	f.torrents = []engine.Status{{ID: 1}}
	h := newHandler(t, f, nil)

	_, result := call(t, h, "torrent-set", map[string]any{
		"ids":            []any{1},
		"seedRatioLimit": 1.5,
		"seedRatioMode":  1,
		"seedIdleLimit":  30,
		"seedIdleMode":   1,
	})
	if result != "success" {
		t.Fatalf("torrent-set returned %q, want success", result)
	}
	got := f.seedLimitsSet[1]
	want := store.SeedLimits{RatioLimit: 1.5, RatioMode: 1, IdleLimit: 30, IdleMode: 1}
	if got != want {
		t.Errorf("seed limits = %+v, want %+v", got, want)
	}

	args, _ := call(t, h, "torrent-get", map[string]any{
		"fields": []string{"seedRatioLimit", "seedRatioMode", "seedIdleLimit", "seedIdleMode"},
	})
	reported := args["torrents"].([]any)[0].(map[string]any)
	for field, want := range map[string]any{
		"seedRatioLimit": float64(1.5),
		"seedRatioMode":  float64(1),
		"seedIdleLimit":  float64(30),
		"seedIdleMode":   float64(1),
	} {
		if reported[field] != want {
			t.Errorf("%s = %v, want %v", field, reported[field], want)
		}
	}
}

// A torrent nobody has set limits on follows the session, and reports the
// session's own values: a client reading the limit without checking the mode
// would otherwise see "no limit" where one applies.
func TestUntouchedTorrentReportsTheSessionSeedLimits(t *testing.T) {
	f := newFakeEngine()
	f.torrents = []engine.Status{{ID: 1}}
	// testConfig sets a ratio of 2.0 and a seed time of 7200s = 120 minutes.
	args, _ := call(t, newHandler(t, f, nil), "torrent-get", nil)
	got := args["torrents"].([]any)[0].(map[string]any)

	if got["seedRatioMode"] != float64(0) || got["seedIdleMode"] != float64(0) {
		t.Errorf("modes = %v/%v, want 0/0 (follow the session)",
			got["seedRatioMode"], got["seedIdleMode"])
	}
	if got["seedRatioLimit"] != float64(2) {
		t.Errorf("seedRatioLimit = %v, want the session's 2", got["seedRatioLimit"])
	}
	if got["seedIdleLimit"] != float64(120) {
		t.Errorf("seedIdleLimit = %v, want 120 minutes", got["seedIdleLimit"])
	}
}

// Naming one seed field must not reset the other three.
func TestTorrentSetSeedLimitsAreFoldedOntoTheCurrentOnes(t *testing.T) {
	f := newFakeEngine()
	f.torrents = []engine.Status{{ID: 1, SeedLimits: store.SeedLimits{
		RatioLimit: 3, RatioMode: 1, IdleLimit: 45, IdleMode: 1,
	}}}
	h := newHandler(t, f, nil)

	if _, result := call(t, h, "torrent-set", map[string]any{
		"ids": []any{1}, "seedRatioLimit": 9.0,
	}); result != "success" {
		t.Fatalf("torrent-set: %s", result)
	}
	got := f.seedLimitsSet[1]
	want := store.SeedLimits{RatioLimit: 9, RatioMode: 1, IdleLimit: 45, IdleMode: 1}
	if got != want {
		t.Errorf("seed limits = %+v, want %+v - only the named field should move", got, want)
	}
}

// A torrent-set that names no seed field must not write one.
func TestTorrentSetWithoutSeedFieldsWritesNoLimits(t *testing.T) {
	f := newFakeEngine()
	f.torrents = []engine.Status{{ID: 1}}
	if _, result := call(t, newHandler(t, f, nil), "torrent-set",
		map[string]any{"ids": []any{1}, "labels": []string{"x"}}); result != "success" {
		t.Fatalf("torrent-set: %s", result)
	}
	if _, touched := f.seedLimitsSet[1]; touched {
		t.Error("seed limits were written by a torrent-set that did not mention them")
	}
}

// Re-adding a release that is already there is a normal event - Radarr does it
// constantly - and Transmission answers it with torrent-duplicate. Reporting a
// fresh add hides it from a client that tells the two apart.
func TestTorrentAddReportsADuplicate(t *testing.T) {
	f := newFakeEngine()
	f.torrents = []engine.Status{{ID: 7, Name: "already here"}}
	f.addDuplicate, f.duplicateID = true, 7
	h := newHandler(t, f, nil)

	args, result := call(t, h, "torrent-add", map[string]any{"filename": "magnet:?xt=urn:btih:abc"})
	if result != "success" {
		t.Fatalf("a duplicate add is not a failure, got %q", result)
	}
	if _, ok := args["torrent-added"]; ok {
		t.Error("a duplicate was reported as torrent-added")
	}
	dup, ok := args["torrent-duplicate"].(map[string]any)
	if !ok {
		t.Fatalf("no torrent-duplicate in %v", args)
	}
	if dup["id"] != float64(7) {
		t.Errorf("id = %v, want the existing torrent's 7", dup["id"])
	}
}

// Sonarr and Radarr ask for the file count without asking for the files, and a
// client asking only for "wanted" used to get an empty array: the file list was
// fetched for the "files" field alone.
func TestFileDerivedFieldsFetchTheFileList(t *testing.T) {
	f := newFakeEngine()
	f.torrents = []engine.Status{{ID: 1}}
	f.files[1] = []engine.File{
		{Index: 0, Path: "a.mkv", Length: 10, Completed: 10, Selected: true},
		{Index: 1, Path: "b.iso", Length: 20, Selected: false},
	}
	h := newHandler(t, f, nil)

	for _, field := range []string{"fileCount", "file-count", "wanted", "priorities", "fileStats"} {
		t.Run(field, func(t *testing.T) {
			args, _ := call(t, h, "torrent-get", map[string]any{"fields": []string{field}})
			got := args["torrents"].([]any)[0].(map[string]any)[field]
			switch v := got.(type) {
			case float64:
				if v != 2 {
					t.Errorf("%s = %v, want 2", field, v)
				}
			case []any:
				if len(v) != 2 {
					t.Errorf("%s has %d entries, want 2", field, len(v))
				}
			default:
				t.Errorf("%s came back as %T", field, got)
			}
		})
	}
}

// fileStats carries the same three facts as the parallel wanted/priorities
// arrays; clients read one or the other.
func TestFileStats(t *testing.T) {
	f := newFakeEngine()
	f.torrents = []engine.Status{{ID: 1}}
	f.files[1] = []engine.File{
		{Index: 0, Path: "a.mkv", Length: 10, Completed: 4, Selected: true},
		{Index: 1, Path: "b.iso", Length: 20, Selected: false},
	}
	args, _ := call(t, newHandler(t, f, nil), "torrent-get",
		map[string]any{"fields": []string{"fileStats"}})
	stats := args["torrents"].([]any)[0].(map[string]any)["fileStats"].([]any)

	first := stats[0].(map[string]any)
	if first["bytesCompleted"] != float64(4) || first["wanted"] != true || first["priority"] != float64(0) {
		t.Errorf("wanted file's stats = %v", first)
	}
	second := stats[1].(map[string]any)
	if second["wanted"] != false || second["priority"] != float64(-1) {
		t.Errorf("unwanted file's stats = %v", second)
	}
}

// secondsDownloading stops at the finish rather than running forever.
func TestSecondsDownloading(t *testing.T) {
	f := newFakeEngine()
	f.torrents = []engine.Status{
		{ID: 1, AddedAt: time.Now().Add(-10 * time.Minute)},
		{ID: 2, AddedAt: time.Now().Add(-10 * time.Minute), FinishedAt: time.Now().Add(-8 * time.Minute)},
	}
	args, _ := call(t, newHandler(t, f, nil), "torrent-get",
		map[string]any{"fields": []string{"secondsDownloading"}})
	list := args["torrents"].([]any)

	if got := list[0].(map[string]any)["secondsDownloading"].(float64); got < 590 || got > 610 {
		t.Errorf("an unfinished torrent reported %v, want about 600", got)
	}
	if got := list[1].(map[string]any)["secondsDownloading"].(float64); got < 110 || got > 130 {
		t.Errorf("a finished torrent reported %v, want about 120", got)
	}
}

// Sonarr calls queue-move-top after adding whenever its priority is set to
// First. It used to answer "method not implemented", which its ProcessRequest
// raises as a TransmissionException - so the torrent was added and the add was
// then reported as failed.
func TestQueueMoveMethods(t *testing.T) {
	for method, want := range map[string]engine.Move{
		"queue-move-top":    engine.MoveTop,
		"queue-move-up":     engine.MoveUp,
		"queue-move-down":   engine.MoveDown,
		"queue-move-bottom": engine.MoveBottom,
	} {
		t.Run(method, func(t *testing.T) {
			f := newFakeEngine()
			f.torrents = []engine.Status{{ID: 1}}
			_, result := call(t, newHandler(t, f, nil), method, map[string]any{"ids": []any{1}})
			if result != "success" {
				t.Fatalf("%s returned %q, want success", method, result)
			}
			if len(f.moves) != 1 || f.moves[0].ID != 1 || f.moves[0].Move != want {
				t.Errorf("%s produced %+v, want one %v on torrent 1", method, f.moves, want)
			}
		})
	}
}

// Moving several torrents to the top must keep their relative order, which
// means moving the frontmost first - the naive order reverses the set.
func TestQueueMoveKeepsRelativeOrder(t *testing.T) {
	tests := []struct {
		name string
		move string
		want []int64
	}{
		{name: "to the top, frontmost first", move: "queue-move-top", want: []int64{1, 2, 3}},
		{name: "to the bottom, rearmost first", move: "queue-move-bottom", want: []int64{3, 2, 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeEngine()
			f.torrents = []engine.Status{
				{ID: 3, QueuePosition: 2},
				{ID: 1, QueuePosition: 0},
				{ID: 2, QueuePosition: 1},
			}
			if _, result := call(t, newHandler(t, f, nil), tc.move,
				map[string]any{"ids": []any{1, 2, 3}}); result != "success" {
				t.Fatalf("%s: %s", tc.move, result)
			}
			got := make([]int64, 0, len(f.moves))
			for _, m := range f.moves {
				got = append(got, m.ID)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("moved %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("moved in order %v, want %v", got, tc.want)
					break
				}
			}
		})
	}
}

// A queued torrent reports Transmission's download-wait status and its place in
// the queue, which is what a client renders as "Queued (3 of 7)".
func TestQueuedTorrentReportsWaitStatusAndPosition(t *testing.T) {
	f := newFakeEngine()
	f.torrents = []engine.Status{
		{ID: 1, State: engine.StateQueued, Queued: true, QueuePosition: 2, HasMetadata: true},
	}
	args, _ := call(t, newHandler(t, f, nil), "torrent-get",
		map[string]any{"fields": []string{"status", "queuePosition"}})
	got := args["torrents"].([]any)[0].(map[string]any)

	if got["status"] != float64(trStatusDownloadWait) {
		t.Errorf("status = %v, want %d (download-wait)", got["status"], trStatusDownloadWait)
	}
	if got["queuePosition"] != float64(2) {
		t.Errorf("queuePosition = %v, want 2", got["queuePosition"])
	}
}

// A client that can reorder the queue reads session-get to know it exists.
func TestSessionGetAdvertisesTheQueue(t *testing.T) {
	h := newHandler(t, newFakeEngine(), func(cfg *config.AppConfig) {
		cfg.Torrent.DownloadQueueSize = 3
	})
	args, _ := call(t, h, "session-get", nil)
	if args["download-queue-size"] != float64(3) {
		t.Errorf("download-queue-size = %v, want 3", args["download-queue-size"])
	}
	if args["download-queue-enabled"] != true {
		t.Errorf("download-queue-enabled = %v, want true", args["download-queue-enabled"])
	}

	off := newHandler(t, newFakeEngine(), func(cfg *config.AppConfig) {
		cfg.Torrent.DownloadQueueSize = 0
	})
	args, _ = call(t, off, "session-get", nil)
	if args["download-queue-enabled"] != false {
		t.Errorf("a zero queue reported enabled = %v, want false", args["download-queue-enabled"])
	}
}

// Transmission's torrent-start respects the queue and torrent-start-now jumps
// it. Moving to the top rather than ignoring the depth is what keeps the
// operator's limit true: starting one torrent now displaces the one that was
// last in the running set.
func TestTorrentStartNowJumpsTheQueue(t *testing.T) {
	f := newFakeEngine()
	f.torrents = []engine.Status{
		{ID: 1, QueuePosition: 0},
		{ID: 2, QueuePosition: 1, State: engine.StateQueued, Queued: true},
	}
	h := newHandler(t, f, nil)

	if _, result := call(t, h, "torrent-start-now", map[string]any{"ids": []any{2}}); result != "success" {
		t.Fatalf("torrent-start-now returned %q, want success", result)
	}
	if len(f.moves) != 1 || f.moves[0].ID != 2 || f.moves[0].Move != engine.MoveTop {
		t.Errorf("moves = %+v, want torrent 2 to the top", f.moves)
	}
	if !f.started[2] {
		t.Error("torrent-start-now did not start the torrent")
	}
	if f.started[1] {
		t.Error("torrent-start-now touched a torrent it was not given")
	}
}

// Plain torrent-start must not reorder anything: that is the difference between
// the two methods.
func TestTorrentStartDoesNotReorderTheQueue(t *testing.T) {
	f := newFakeEngine()
	f.torrents = []engine.Status{{ID: 1, QueuePosition: 3}}
	if _, result := call(t, newHandler(t, f, nil), "torrent-start",
		map[string]any{"ids": []any{1}}); result != "success" {
		t.Fatalf("torrent-start: %s", result)
	}
	if len(f.moves) != 0 {
		t.Errorf("torrent-start moved things in the queue: %+v", f.moves)
	}
	if !f.started[1] {
		t.Error("torrent-start did not start the torrent")
	}
}
