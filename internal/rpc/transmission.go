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
	"sort"
	"strconv"
	"time"

	"github.com/moshen/bittrench/internal/config"
	"github.com/moshen/bittrench/internal/engine"
	"github.com/moshen/bittrench/internal/store"
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

// Transmission's per-torrent seed-limit modes. Only the global one is named
// here; the engine owns the rest of the semantics.
const seedLimitGlobal = 0

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
	Add(ctx context.Context, req engine.AddRequest) (id int64, duplicate bool, err error)
	Status(id int64) (engine.Status, bool)
	List() []engine.Status
	Files(id int64) []engine.File
	SetLabels(ctx context.Context, id int64, labels []string) error
	SetSeedLimits(ctx context.Context, id int64, limits store.SeedLimits) error
	MoveInQueue(ctx context.Context, id int64, move engine.Move) error
	SetFileSelection(ctx context.Context, id int64, selection []bool) error
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
	case "torrent-set":
		return h.torrentSet(ctx, req.Arguments)
	case "queue-move-top":
		return h.queueMove(ctx, req.Arguments, engine.MoveTop)
	case "queue-move-up":
		return h.queueMove(ctx, req.Arguments, engine.MoveUp)
	case "queue-move-down":
		return h.queueMove(ctx, req.Arguments, engine.MoveDown)
	case "queue-move-bottom":
		return h.queueMove(ctx, req.Arguments, engine.MoveBottom)
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
		// A client that can reorder the queue reads these to know it exists and
		// how deep it is.
		"download-queue-size":    h.cfg.Torrent.DownloadQueueSize,
		"download-queue-enabled": h.cfg.Torrent.DownloadQueueSize > 0,
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
	Filename    string   `json:"filename"`
	Metainfo    string   `json:"metainfo"`
	DownloadDir string   `json:"download-dir"`
	Paused      bool     `json:"paused"`
	Labels      []string `json:"labels"`
}

func (h *Handler) torrentAdd(ctx context.Context, raw json.RawMessage) (any, error) {
	var args addArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}

	// Sonarr and Radarr both send their category as a label whenever the client
	// reports version >= 4.0, which we do. Dropping it left them falling back
	// to matching categories by directory.
	req := engine.AddRequest{
		DownloadDir: args.DownloadDir,
		Paused:      args.Paused,
		Labels:      args.Labels,
	}
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

	id, duplicate, err := h.engine.Add(ctx, req)
	if err != nil {
		return nil, err
	}
	status, ok := h.engine.Status(id)
	if !ok {
		return nil, fmt.Errorf("torrent %d disappeared immediately after being added", id)
	}
	// Clients read the id out of whichever key comes back, and tell the two
	// apart: re-adding a release that is already there is a normal event, and
	// reporting it as a fresh add hides it.
	key := "torrent-added"
	if duplicate {
		key = "torrent-duplicate"
	}
	return map[string]any{key: h.torrentFields(status, h.engine.Files(id))}, nil
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

	// Every field derived from the file list, not just "files" - Sonarr and
	// Radarr ask for the file count without asking for the files, and a client
	// asking only for "wanted" used to get an empty array. No named fields at
	// all means "everything", so the list is needed then too.
	wantFiles := len(args.Fields) == 0
	for _, f := range args.Fields {
		switch f {
		case "files", "fileStats", "fileCount", "file-count", "wanted", "priorities":
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

type setArgs struct {
	IDs           json.RawMessage `json:"ids"`
	Labels        *[]string       `json:"labels"`
	FilesWanted   *[]int          `json:"files-wanted"`
	FilesUnwanted *[]int          `json:"files-unwanted"`

	SeedRatioLimit *float64 `json:"seedRatioLimit"`
	SeedRatioMode  *int     `json:"seedRatioMode"`
	SeedIdleLimit  *int64   `json:"seedIdleLimit"`
	SeedIdleMode   *int     `json:"seedIdleMode"`
}

// seedLimits folds the seed arguments onto a torrent's current caps, leaving
// anything the client did not name alone.
func (a setArgs) seedLimits(current store.SeedLimits) (store.SeedLimits, bool) {
	next, touched := current, false
	if a.SeedRatioLimit != nil {
		next.RatioLimit, touched = *a.SeedRatioLimit, true
	}
	if a.SeedRatioMode != nil {
		next.RatioMode, touched = *a.SeedRatioMode, true
	}
	if a.SeedIdleLimit != nil {
		next.IdleLimit, touched = *a.SeedIdleLimit, true
	}
	if a.SeedIdleMode != nil {
		next.IdleMode, touched = *a.SeedIdleMode, true
	}
	return next, touched
}

// torrentSet applies the mutable per-torrent settings a client can change.
//
// Sonarr calls this twice over a download's life: once after adding, to push
// its seed criteria down, and again on import, to swap the category label. Both
// used to answer "method not implemented", which its ProcessRequest raises as a
// TransmissionException - so a configured seed limit or imported category turned
// a working download into a reported failure.
//
// Every argument is a pointer so an absent key is distinguishable from an empty
// one: `labels: []` clears the labels, while omitting the key leaves them
// alone. Unknown arguments are ignored, as Transmission ignores them.
func (h *Handler) torrentSet(ctx context.Context, raw json.RawMessage) (any, error) {
	var args setArgs
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
		if args.Labels != nil {
			if err := h.engine.SetLabels(ctx, s.ID, *args.Labels); err != nil {
				return nil, err
			}
		}
		if limits, touched := args.seedLimits(s.SeedLimits); touched {
			if err := h.engine.SetSeedLimits(ctx, s.ID, limits); err != nil {
				return nil, err
			}
		}
		if args.FilesWanted == nil && args.FilesUnwanted == nil {
			continue
		}
		if err := h.applyFileWishes(ctx, s.ID, args); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// applyFileWishes turns files-wanted/files-unwanted index lists into the
// engine's whole-selection form.
//
// The lists are edits to the current selection, not a replacement for it, so
// the current one is the starting point. An empty list means every file, which
// is Transmission's own reading. Indices outside the file list are skipped
// rather than rejected, also matching it.
func (h *Handler) applyFileWishes(ctx context.Context, id int64, args setArgs) error {
	files := h.engine.Files(id)
	if len(files) == 0 {
		// No metadata yet, so there is no file list to index into. Refusing is
		// better than silently dropping the instruction.
		return fmt.Errorf("torrent %d has no file list yet", id)
	}
	selection := make([]bool, len(files))
	for i, f := range files {
		selection[i] = f.Selected
	}

	apply := func(indices *[]int, want bool) {
		if indices == nil {
			return
		}
		if len(*indices) == 0 {
			for i := range selection {
				selection[i] = want
			}
			return
		}
		for _, i := range *indices {
			if i >= 0 && i < len(selection) {
				selection[i] = want
			}
		}
	}
	// Unwanted first, so an index named in both ends up wanted - the same
	// precedence a client gets from Transmission.
	apply(args.FilesUnwanted, false)
	apply(args.FilesWanted, true)

	return h.engine.SetFileSelection(ctx, id, selection)
}

// queueMove repositions torrents in the download queue.
//
// Sonarr calls queue-move-top after adding, whenever its Recent or Older
// priority is set to First. It used to answer "method not implemented", which
// its ProcessRequest raises as a TransmissionException - so the torrent was
// added and the add was then reported as having failed.
//
// Moves are applied in queue order rather than the order the ids arrived in, so
// moving a set of torrents to the top keeps their relative order instead of
// reversing it.
func (h *Handler) queueMove(ctx context.Context, raw json.RawMessage, move engine.Move) (any, error) {
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

	selected := make([]engine.Status, 0, len(ids))
	for _, s := range h.engine.List() {
		if filtered && !ids[s.ID] {
			continue
		}
		selected = append(selected, s)
	}
	// To the top or up, the frontmost torrent moves first; to the bottom or
	// down, the rearmost does. Either way the set arrives in its original order.
	sort.SliceStable(selected, func(i, j int) bool {
		if move == engine.MoveTop || move == engine.MoveUp {
			return selected[i].QueuePosition < selected[j].QueuePosition
		}
		return selected[i].QueuePosition > selected[j].QueuePosition
	})
	for _, s := range selected {
		if err := h.engine.MoveInQueue(ctx, s.ID, move); err != nil {
			return nil, err
		}
	}
	return nil, nil
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
	// The engine keeps both sets of figures, and Status.Complete is the daemon's
	// one answer to "is it done", so there is nothing to decide here.
	sizeWhenDone, leftUntilDone := s.SizeWhenDone, s.LeftUntilDone
	complete := s.Complete()

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

	// secondsDownloading is wall-clock time from the add to finishing, or to
	// now while it is still going. An approximation: time spent paused counts
	// towards it, which the engine does not record separately. Both clients ask
	// for the field and neither acts on it, so an honest approximation beats the
	// zero they were getting.
	var secondsDownloading int64
	switch {
	case s.AddedAt.IsZero():
	case !s.FinishedAt.IsZero():
		secondsDownloading = int64(s.FinishedAt.Sub(s.AddedAt).Seconds())
	default:
		secondsDownloading = int64(time.Since(s.AddedAt).Seconds())
	}

	var addedDate int64
	if !s.AddedAt.IsZero() {
		addedDate = s.AddedAt.Unix()
	}

	labels := s.Labels
	if labels == nil {
		labels = []string{}
	}

	// A torrent following the session limit reports the session's value, not a
	// zero: a client that reads the limit without checking the mode would
	// otherwise see "no limit" where one applies.
	seedRatioLimit := s.SeedLimits.RatioLimit
	if s.SeedLimits.RatioMode == seedLimitGlobal {
		seedRatioLimit = h.cfg.Torrent.Limits.SeedRatioLimit
	}
	seedIdleLimit := s.SeedLimits.IdleLimit
	if s.SeedLimits.IdleMode == seedLimitGlobal {
		// Transmission carries this in minutes; our config is in seconds.
		seedIdleLimit = int64(h.cfg.Torrent.Limits.SeedTimeLimitSecs / 60)
	}

	fileList := make([]map[string]any, 0, len(files))
	fileStats := make([]map[string]any, 0, len(files))
	wanted := make([]int, 0, len(files))
	priorities := make([]int, 0, len(files))
	for _, f := range files {
		fileList = append(fileList, map[string]any{
			"name":           f.Path,
			"length":         f.Length,
			"bytesCompleted": f.Completed,
		})
		priority := 0
		if !f.Selected {
			priority = -1
		}
		// fileStats is the modern per-file shape, carrying the same three facts
		// the parallel wanted/priorities arrays do. Clients read one or the
		// other, so both are published.
		fileStats = append(fileStats, map[string]any{
			"bytesCompleted": f.Completed,
			"wanted":         f.Selected,
			"priority":       priority,
		})
		if f.Selected {
			wanted = append(wanted, 1)
		} else {
			wanted = append(wanted, 0)
		}
		priorities = append(priorities, priority)
	}

	return map[string]any{
		"id": s.ID,
		// name and hashString are how the *arr clients match a download back
		// to the release they asked for. Name is safe before metadata: it
		// falls back to the magnet's display name, then to the infohash.
		"name":       s.Name,
		"hashString": s.InfoHash.HexString(),
		"status":     trStatus(s),
		"totalSize":  s.TotalBytes,
		"haveValid":  s.CompletedBytes,
		// downloadedEver is bytes written to disk for this torrent; the
		// completed size is the honest approximation we can offer.
		"downloadedEver":     s.CompletedBytes,
		"uploadedEver":       s.UploadedBytes,
		"leftUntilDone":      leftUntilDone,
		"sizeWhenDone":       sizeWhenDone,
		"percentDone":        percentDone,
		"percentComplete":    percentComplete,
		"rateDownload":       int64(s.DownloadRate),
		"rateUpload":         int64(s.UploadRate),
		"eta":                etaSeconds(s),
		"isFinished":         complete,
		"isStalled":          false,
		"peersConnected":     s.Peers,
		"peersSendingToUs":   s.Seeders,
		"secondsSeeding":     secondsSeeding,
		"secondsDownloading": secondsDownloading,
		"addedDate":          addedDate,
		"uploadRatio":        s.Ratio(),
		// The torrent's own caps as a client set them. Mode 0 is Transmission's
		// TR_RATIOLIMIT_GLOBAL - follow the session limit - which is what an
		// untouched torrent reports, and then the limit reported alongside it is
		// the session's so a client reading only the limit still sees a usable
		// number. Sonarr decides for itself when a download has seeded enough,
		// from exactly these four fields.
		"seedRatioLimit": seedRatioLimit,
		"seedRatioMode":  s.SeedLimits.RatioMode,
		"seedIdleLimit":  seedIdleLimit,
		"seedIdleMode":   s.SeedLimits.IdleMode,
		// The per-torrent directory, not the session-wide save path - once
		// download-dir is honoured per torrent, reporting the session path
		// for every torrent is simply a lie.
		"downloadDir": s.SavePath,
		"files":       fileList,
		"fileStats":   fileStats,
		"wanted":      wanted,
		"priorities":  priorities,
		// Both spellings: file-count is Transmission's, fileCount is Vuze's,
		// and Sonarr and Radarr ask for both without knowing which they will get.
		"file-count":    len(files),
		"fileCount":     len(files),
		"error":         errCode,
		"errorString":   s.Error,
		"queuePosition": s.QueuePosition,
		// Always an array, never null: a client that filters on it should see
		// "no labels", and Transmission itself sends [].
		"labels": labels,
	}
}

func trStatus(s engine.Status) int {
	switch s.State {
	case engine.StateQueued:
		return trStatusDownloadWait
	case engine.StateDownloading:
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

// etaSeconds reports Transmission's -1 for "unknown". Status.ETA already counts
// only the wanted bytes.
func etaSeconds(s engine.Status) int64 {
	eta := s.ETA()
	if eta < 0 {
		return -1
	}
	return int64(eta.Seconds())
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
