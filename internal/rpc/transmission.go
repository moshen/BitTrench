// Package rpc implements the Transmission-compatible JSON-RPC endpoint that
// Sonarr and Radarr drive the daemon through.
//
// The contract here is not "whatever Transmission happens to do" - it is what
// those two clients actually require, each line of it learned the hard way.
// The tests are the specification; read them before changing anything.
//
// # CSRF session-token handshake
//
// Transmission clients boot their auth flow with a pre-flight GET and expect
// HTTP 409 carrying an X-Transmission-Session-Id header. They cache that token
// and echo it on every POST; a request without it (or with a stale one) gets
// 409 and the current token back. Sonarr's AuthenticateClient starts with that
// GET and treats anything else - including a 405 from a POST-only route - as
// DownloadClientAuthenticationException.
package rpc

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/moshen/bittrench/internal/config"
	"github.com/moshen/bittrench/internal/engine"
)

// Transmission status codes, as the *arr clients interpret them. The whole set
// is spelled out because the clients test against specific values - Sonarr
// reads a torrent as complete when its status is stopped, seeding or seed-wait,
// and refuses to when it is check or check-wait.
const (
	trStatusStopped      = 0
	trStatusCheckWait    = 1
	trStatusCheck        = 2
	trStatusDownloadWait = 3
	trStatusDownloading  = 4
	trStatusSeedWait     = 5
	trStatusSeeding      = 6
)

// trStatLocalError is Transmission's TR_STAT_LOCAL_ERROR. A non-zero `error`
// is what marks a torrent failed; `status` is reported as stopped alongside it.
const trStatLocalError = 3

// reportedVersion is what session-get claims to be.
//
// Radarr regex-parses this and requires >= 2.40, so it cannot be a
// bittrench version string. Reporting a real Transmission release keeps
// the client's own feature gating on ground it understands.
const reportedVersion = "4.0.6"

// rpcVersion is the Transmission RPC protocol version we implement against.
const rpcVersion = 17

// Torrents is the slice of the engine this endpoint needs. Defining it here
// rather than depending on *engine.Engine keeps the RPC layer testable without
// a tunnel, which is what makes the contract above cheap to pin down.
type Torrents interface {
	Add(ctx context.Context, req engine.AddRequest) (int64, error)
	Status(id int64) (engine.Status, bool)
	List() []engine.Status
	Files(id int64) []engine.File
	SelectedBytes(id int64) (completed, total int64, files int)
	Start(ctx context.Context, id int64) error
	Stop(ctx context.Context, id int64) error
	Remove(ctx context.Context, id int64, deleteData bool) error
	SessionRates() (down, up float64)
}

// Handler serves /transmission/rpc.
type Handler struct {
	engine Torrents
	cfg    *config.AppConfig
	// sessionID is stable for the process lifetime, as Transmission's is.
	sessionID string
	// authHeader is the expected Authorization value, empty when auth is off.
	authHeader string
}

// New builds the handler. The session token comes from crypto/rand because it
// is a CSRF token: anything guessable, a PID or a start time, defeats its
// purpose.
func New(eng Torrents, cfg *config.AppConfig) (*Handler, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("failed to generate a session token: %w", err)
	}
	h := &Handler{
		engine:    eng,
		cfg:       cfg,
		sessionID: base64.RawURLEncoding.EncodeToString(buf),
	}
	if cfg.API.Username != "" || cfg.API.Password != "" {
		h.authHeader = "Basic " + base64.StdEncoding.EncodeToString(
			[]byte(cfg.API.Username+":"+cfg.API.Password))
	}
	return h, nil
}

// Path is where the endpoint is mounted. Transmission clients hardcode it.
const Path = "/transmission/rpc"

// Register mounts the endpoint on a mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle(Path, h)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.authorised(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="transmission"`)
		writeJSON(w, http.StatusUnauthorized, response{Result: "Unauthorized"})
		return
	}

	// The GET pre-flight exists purely to hand out the token. Answering it
	// with 405 - which a POST-only route would - breaks Sonarr's whole auth
	// flow, so every method that is not POST gets the handshake.
	if r.Method != http.MethodPost {
		h.conflict(w)
		return
	}
	if !h.tokenMatches(r) {
		h.conflict(w)
		return
	}

	var req request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, response{Result: "invalid request body: " + err.Error()})
		return
	}

	args, err := h.dispatch(r.Context(), req)
	resp := response{Result: "success", Arguments: args, Tag: req.Tag}
	if err != nil {
		// Transmission reports method-level failures in `result` with HTTP
		// 200; an HTTP error would make the *arr clients drop the connection
		// rather than surface the message.
		resp = response{Result: err.Error(), Tag: req.Tag}
	}
	writeJSON(w, http.StatusOK, resp)
}

// maxRequestBytes caps a request body. A base64 .torrent in `metainfo` is the
// largest thing a client legitimately sends.
const maxRequestBytes = 16 << 20

func (h *Handler) dispatch(ctx context.Context, req request) (any, error) {
	switch req.Method {
	case "session-get":
		return h.sessionGet(), nil
	case "session-stats":
		return h.sessionStats(), nil
	case "torrent-add":
		return h.torrentAdd(ctx, req.Arguments)
	case "torrent-get":
		return h.torrentGet(req.Arguments)
	case "torrent-remove":
		return h.torrentRemove(ctx, req.Arguments)
	case "torrent-stop":
		return h.torrentSetPaused(ctx, req.Arguments, true)
	case "torrent-start":
		return h.torrentSetPaused(ctx, req.Arguments, false)
	default:
		// Transmission's convention: not an HTTP error.
		return nil, fmt.Errorf("method not implemented: %s", req.Method)
	}
}

func (h *Handler) authorised(r *http.Request) bool {
	if h.authHeader == "" {
		return true
	}
	provided := r.Header.Get("Authorization")
	return subtle.ConstantTimeCompare([]byte(provided), []byte(h.authHeader)) == 1
}

func (h *Handler) tokenMatches(r *http.Request) bool {
	return r.Header.Get("X-Transmission-Session-Id") == h.sessionID
}

// conflict is the canonical 409 that distributes the CSRF token.
func (h *Handler) conflict(w http.ResponseWriter) {
	w.Header().Set("X-Transmission-Session-Id", h.sessionID)
	writeJSON(w, http.StatusConflict, map[string]string{
		"result": "session-conflict",
		"message": "Session token mismatch or missing. " +
			"Re-send with the X-Transmission-Session-Id header.",
	})
}

type request struct {
	Method    string          `json:"method"`
	Arguments json.RawMessage `json:"arguments"`
	Tag       *int64          `json:"tag"`
}

type response struct {
	Result    string `json:"result"`
	Arguments any    `json:"arguments,omitempty"`
	Tag       *int64 `json:"tag,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Stale cached JS once resurrected bugs that had already been fixed, so
	// nothing this daemon serves is cacheable.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Debug("failed to write an RPC response", "error", err)
	}
}

func (h *Handler) sessionGet() map[string]any {
	limits := h.cfg.Torrent.Limits
	return map[string]any{
		"version":             reportedVersion,
		"rpc-version":         rpcVersion,
		"rpc-version-minimum": 1,
		// Sonarr's GetDownloadDirectory reads this, and its per-category path
		// resolution silently breaks when it is missing.
		"download-dir":     h.cfg.Torrent.SavePath,
		"seedRatioLimit":   limits.SeedRatioLimit,
		"seedRatioLimited": limits.SeedRatioLimit > 0,
		// Transmission reports this in *minutes*; ours is configured in
		// seconds.
		"idle-seeding-limit":         limits.SeedTimeLimitSecs / 60,
		"idle-seeding-limit-enabled": limits.SeedTimeLimitSecs > 0,
	}
}

func (h *Handler) sessionStats() map[string]any {
	down, up := h.engine.SessionRates()
	var active, paused int
	for _, s := range h.engine.List() {
		if s.Paused {
			paused++
		} else {
			active++
		}
	}
	return map[string]any{
		"downloadSpeed":      int64(down),
		"uploadSpeed":        int64(up),
		"activeTorrentCount": active,
		"pausedTorrentCount": paused,
		"torrentCount":       active + paused,
	}
}

type addArgs struct {
	Filename    string `json:"filename"`
	Metainfo    string `json:"metainfo"`
	DownloadDir string `json:"download-dir"`
	Paused      bool   `json:"paused"`
}

func (h *Handler) torrentAdd(ctx context.Context, raw json.RawMessage) (any, error) {
	var args addArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}

	req := engine.AddRequest{DownloadDir: args.DownloadDir, Paused: args.Paused}
	switch {
	case args.Metainfo != "":
		blob, err := base64.StdEncoding.DecodeString(args.Metainfo)
		if err != nil {
			return nil, fmt.Errorf("invalid base64 in metainfo: %w", err)
		}
		req.Metainfo = blob
	case args.Filename != "":
		req.Source = args.Filename
	default:
		return nil, fmt.Errorf("missing 'filename' or 'metainfo' argument")
	}

	id, err := h.engine.Add(ctx, req)
	if err != nil {
		return nil, err
	}
	status, ok := h.engine.Status(id)
	if !ok {
		return nil, fmt.Errorf("torrent %d disappeared immediately after being added", id)
	}
	// Transmission answers with torrent-added; clients read the id from it.
	return map[string]any{"torrent-added": h.torrentFields(status, nil)}, nil
}

type getArgs struct {
	Fields []string        `json:"fields"`
	IDs    json.RawMessage `json:"ids"`
}

func (h *Handler) torrentGet(raw json.RawMessage) (any, error) {
	var args getArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}
	ids, filtered, err := parseIDs(args.IDs)
	if err != nil {
		return nil, err
	}

	wantFiles := false
	for _, f := range args.Fields {
		if f == "files" {
			wantFiles = true
		}
	}

	torrents := make([]map[string]any, 0)
	for _, s := range h.engine.List() {
		if filtered && !ids[s.ID] {
			continue
		}
		var files []engine.File
		if wantFiles {
			files = h.engine.Files(s.ID)
		}
		torrents = append(torrents, project(h.torrentFields(s, files), args.Fields))
	}
	return map[string]any{"torrents": torrents}, nil
}

type removeArgs struct {
	IDs             json.RawMessage `json:"ids"`
	DeleteLocalData bool            `json:"delete-local-data"`
}

func (h *Handler) torrentRemove(ctx context.Context, raw json.RawMessage) (any, error) {
	var args removeArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}
	ids, filtered, err := parseIDs(args.IDs)
	if err != nil {
		return nil, err
	}
	for _, s := range h.engine.List() {
		if filtered && !ids[s.ID] {
			continue
		}
		if err := h.engine.Remove(ctx, s.ID, args.DeleteLocalData); err != nil {
			slog.Warn("torrent-remove failed", "id", s.ID, "error", err)
		}
	}
	return nil, nil
}

func (h *Handler) torrentSetPaused(ctx context.Context, raw json.RawMessage, paused bool) (any, error) {
	var args getArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}
	ids, filtered, err := parseIDs(args.IDs)
	if err != nil {
		return nil, err
	}
	for _, s := range h.engine.List() {
		if filtered && !ids[s.ID] {
			continue
		}
		var err error
		if paused {
			err = h.engine.Stop(ctx, s.ID)
		} else {
			err = h.engine.Start(ctx, s.ID)
		}
		if err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// parseIDs accepts both integers and strings, in every method.
//
// The Transmission RPC spec allows either, and clients use both. A previous
// version of this endpoint accepted only strings in torrent-get, so
// `{"ids":[0]}` silently matched nothing - the torrent looked like it had
// vanished. filtered is false when no ids were given, which means "all".
func parseIDs(raw json.RawMessage) (ids map[int64]bool, filtered bool, err error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false, nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		// A bare id, or the "recently-active" string, rather than an array.
		var single json.RawMessage = raw
		entries = []json.RawMessage{single}
	}

	ids = make(map[int64]bool, len(entries))
	for _, entry := range entries {
		var n int64
		if err := json.Unmarshal(entry, &n); err == nil {
			ids[n] = true
			continue
		}
		var s string
		if err := json.Unmarshal(entry, &s); err != nil {
			return nil, false, fmt.Errorf("invalid torrent id %s", entry)
		}
		// "recently-active" is a Transmission selector we do not implement;
		// treating it as "all" is closer to right than matching nothing.
		if s == "recently-active" {
			return nil, false, nil
		}
		parsed, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, false, fmt.Errorf("invalid torrent id %q", s)
		}
		ids[parsed] = true
	}
	if len(ids) == 0 {
		return nil, false, nil
	}
	return ids, true, nil
}

// torrentFields is the full field set. Projection to the client's `fields`
// list happens afterwards.
func (h *Handler) torrentFields(s engine.Status, files []engine.File) map[string]any {
	// Transmission measures leftUntilDone, sizeWhenDone, percentDone and
	// isFinished against the files the torrent *wants*; only totalSize,
	// haveValid and percentComplete are whole-torrent numbers. Reporting the
	// whole torrent for the first group leaves a filtered torrent - one under
	// an allowed_extensions allow-list - permanently 90%-and-downloading, so
	// Sonarr's `leftUntilDone == 0` and `isFinished` completion tests never
	// fire and the release is never imported.
	selCompleted, selTotal, selFiles := h.engine.SelectedBytes(s.ID)
	haveSelection := selFiles > 0

	// Before metadata there is no selection to measure, so fall back to the
	// whole-torrent figures rather than reporting a confident zero.
	sizeWhenDone, leftUntilDone := s.TotalBytes, s.MissingBytes
	if haveSelection {
		sizeWhenDone = selTotal
		leftUntilDone = max(selTotal-selCompleted, 0)
	}
	// Derived from whichever figure applied, so an unfiltered torrent keeps the
	// meaning it always had. HasMetadata gates it because a magnet whose info
	// dict has not resolved has nothing missing yet and must not read as done.
	complete := s.HasMetadata && leftUntilDone == 0

	var percentDone float64
	switch {
	case sizeWhenDone > 0:
		percentDone = float64(sizeWhenDone-leftUntilDone) / float64(sizeWhenDone)
	case complete:
		percentDone = 1
	}

	var percentComplete float64
	if s.TotalBytes > 0 {
		percentComplete = float64(s.CompletedBytes) / float64(s.TotalBytes)
	}

	errCode := 0
	if s.Error != "" {
		errCode = trStatLocalError
	}

	var secondsSeeding int64
	if !s.FinishedAt.IsZero() {
		secondsSeeding = int64(time.Since(s.FinishedAt).Seconds())
	}

	var addedDate int64
	if !s.AddedAt.IsZero() {
		addedDate = s.AddedAt.Unix()
	}

	fileList := make([]map[string]any, 0, len(files))
	wanted := make([]int, 0, len(files))
	priorities := make([]int, 0, len(files))
	for _, f := range files {
		fileList = append(fileList, map[string]any{
			"name":           f.Path,
			"length":         f.Length,
			"bytesCompleted": f.Completed,
		})
		if f.Selected {
			wanted = append(wanted, 1)
			priorities = append(priorities, 0)
		} else {
			wanted = append(wanted, 0)
			priorities = append(priorities, -1)
		}
	}

	return map[string]any{
		"id": s.ID,
		// name and hashString are how the *arr clients match a download back
		// to the release they asked for. Name is safe before metadata: it
		// falls back to the magnet's display name, then to the infohash.
		"name":       s.Name,
		"hashString": s.InfoHash.HexString(),
		"status":     trStatus(s, complete),
		"totalSize":  s.TotalBytes,
		"haveValid":  s.CompletedBytes,
		// downloadedEver is bytes written to disk for this torrent; the
		// completed size is the honest approximation we can offer.
		"downloadedEver":   s.CompletedBytes,
		"uploadedEver":     s.UploadedBytes,
		"leftUntilDone":    leftUntilDone,
		"sizeWhenDone":     sizeWhenDone,
		"percentDone":      percentDone,
		"percentComplete":  percentComplete,
		"rateDownload":     int64(s.DownloadRate),
		"rateUpload":       int64(s.UploadRate),
		"eta":              etaSeconds(s, leftUntilDone),
		"isFinished":       complete,
		"isStalled":        false,
		"peersConnected":   s.Peers,
		"peersSendingToUs": s.Seeders,
		"secondsSeeding":   secondsSeeding,
		"addedDate":        addedDate,
		"uploadRatio":      s.Ratio(),
		"seedRatioLimit":   h.cfg.Torrent.Limits.SeedRatioLimit,
		// 0 is Transmission's TR_RATIOLIMIT_GLOBAL: follow the session limit.
		"seedRatioMode": 0,
		// The per-torrent directory, not the session-wide save path - once
		// download-dir is honoured per torrent, reporting the session path
		// for every torrent is simply a lie.
		"downloadDir": s.SavePath,
		"files":       fileList,
		"wanted":      wanted,
		"priorities":  priorities,
		"error":       errCode,
		"errorString": s.Error,
	}
}

func trStatus(s engine.Status, complete bool) int {
	switch s.State {
	case engine.StateDownloading:
		// The engine calls a filtered torrent "downloading" for as long as any
		// piece is missing, wanted or not. Once everything selected has
		// arrived there is nothing left to download, and saying so is what
		// puts the torrent in the state a client tests for completion.
		if complete {
			return trStatusSeeding
		}
		return trStatusDownloading
	case engine.StateSeeding:
		return trStatusSeeding
	case engine.StateInitialising:
		return trStatusCheckWait
	case engine.StateError, engine.StateStopped:
		return trStatusStopped
	default:
		return trStatusStopped
	}
}

// etaSeconds reports Transmission's -1 for "unknown".
//
// Status.ETA divides the whole torrent's missing bytes by the rate, which
// overstates a filtered torrent's remaining time and never reaches zero, so
// the wanted bytes are passed in instead.
func etaSeconds(s engine.Status, leftUntilDone int64) int64 {
	if leftUntilDone <= 0 || s.DownloadRate <= 0 {
		return -1
	}
	return int64(float64(leftUntilDone) / s.DownloadRate)
}

// project returns only the requested keys. When fields is empty the whole set
// is returned, which is what torrent-add answers with.
func project(all map[string]any, fields []string) map[string]any {
	if len(fields) == 0 {
		return all
	}
	out := make(map[string]any, len(fields))
	for _, f := range fields {
		if v, ok := all[f]; ok {
			out[f] = v
		}
	}
	return out
}
