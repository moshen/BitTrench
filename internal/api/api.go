// Package api is the native JSON API the web UI talks to.
//
// It is designed around what anacrolix/torrent actually exposes. One choice is
// worth calling out: the pieces endpoint returns a base64 bitfield and an
// explicit piece count, rather than a rendered string for a client to scrape.
// The count has to be explicit because the bitfield's last byte is padded, and
// anything that cannot tell padding from data reads those bits as missing
// pieces.
package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/moshen/bittrench/internal/engine"
)

// Prefix is the API root.
const Prefix = "/api/v1"

// maxUploadBytes caps an uploaded .torrent.
const maxUploadBytes = 8 << 20

// Torrents is the slice of the engine this API needs.
type Torrents interface {
	Add(ctx context.Context, req engine.AddRequest) (id int64, duplicate bool, err error)
	Status(id int64) (engine.Status, bool)
	List() []engine.Status
	Files(id int64) []engine.File
	Peers(id int64) []engine.Peer
	AnnounceURLs(id int64) []string
	Bitfield(id int64) ([]byte, int)
	SetFileSelection(ctx context.Context, id int64, selection []bool) error
	Start(ctx context.Context, id int64) error
	Stop(ctx context.Context, id int64) error
	Remove(ctx context.Context, id int64, deleteData bool) error
	SessionRates() (down, up float64)
}

// Handler serves /api/v1.
type Handler struct {
	engine Torrents
}

// New builds the API handler.
func New(eng Torrents) *Handler { return &Handler{engine: eng} }

// Register mounts the API on a mux. Go 1.22+ patterns carry the method and the
// path variable, so there is no router dependency to take.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+Prefix+"/stats", h.stats)
	mux.HandleFunc("GET "+Prefix+"/torrents", h.list)
	mux.HandleFunc("POST "+Prefix+"/torrents", h.add)
	mux.HandleFunc("GET "+Prefix+"/torrents/{id}", h.detail)
	mux.HandleFunc("DELETE "+Prefix+"/torrents/{id}", h.remove)
	mux.HandleFunc("GET "+Prefix+"/torrents/{id}/files", h.files)
	mux.HandleFunc("POST "+Prefix+"/torrents/{id}/files", h.setFiles)
	mux.HandleFunc("GET "+Prefix+"/torrents/{id}/peers", h.peers)
	mux.HandleFunc("GET "+Prefix+"/torrents/{id}/pieces", h.pieces)
	mux.HandleFunc("POST "+Prefix+"/torrents/{id}/start", h.start)
	mux.HandleFunc("POST "+Prefix+"/torrents/{id}/stop", h.stop)
}

// roundRate trims a rate to two decimals before it goes on the wire.
//
// The rates are smoothed floats, so their tail is arithmetic rather than
// measurement. A client that prints what it is given should not have to know
// that, and 0.0000025947061343373236 B/s is not a number to show anybody. Two
// decimals is already past the point where a bytes-per-second figure carries
// meaning.
func roundRate(r float64) float64 {
	// NaN and the infinities are not rates, and encoding/json refuses them
	// outright, which would cost the whole response rather than this one field.
	if math.IsNaN(r) || math.IsInf(r, 0) {
		return 0
	}
	return math.Round(r*100) / 100
}

// Torrent is the list and detail representation.
type Torrent struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	InfoHash string `json:"info_hash"`
	State    string `json:"state"`
	Paused   bool   `json:"paused"`
	// QueuePosition is the torrent's place in the download queue, counted from
	// zero, so the UI can say which one it is waiting behind.
	QueuePosition int    `json:"queue_position"`
	Error         string `json:"error,omitempty"`
	SavePath      string `json:"save_path"`

	TotalBytes     int64 `json:"total_bytes"`
	CompletedBytes int64 `json:"completed_bytes"`
	// SizeWhenDone and LeftUntilDone cover only the files the torrent wants, so
	// progress against them reaches 100% for a torrent under an extension
	// allow-list. TotalBytes stays the torrent's own size, which is what the UI
	// shows as its size.
	SizeWhenDone  int64 `json:"size_when_done"`
	LeftUntilDone int64 `json:"left_until_done"`
	UploadedBytes int64 `json:"uploaded_bytes"`

	DownloadRate float64 `json:"download_rate"`
	UploadRate   float64 `json:"upload_rate"`

	Peers   int     `json:"peers"`
	Seeders int     `json:"seeders"`
	Ratio   float64 `json:"ratio"`
	// ETASeconds is -1 when unknown.
	ETASeconds int64 `json:"eta_seconds"`
	// HasMetadata is false while a magnet is still resolving; every
	// metadata-dependent field above is zero until it is true.
	HasMetadata bool  `json:"has_metadata"`
	AddedAt     int64 `json:"added_at"`
	FinishedAt  int64 `json:"finished_at,omitempty"`

	// Trackers is only populated on the detail endpoint.
	Trackers []string `json:"trackers,omitempty"`
}

func torrentOf(s engine.Status) Torrent {
	t := Torrent{
		ID:             s.ID,
		Name:           s.Name,
		InfoHash:       s.InfoHash.HexString(),
		State:          string(s.State),
		QueuePosition:  s.QueuePosition,
		Paused:         s.Paused,
		Error:          s.Error,
		SavePath:       s.SavePath,
		TotalBytes:     s.TotalBytes,
		CompletedBytes: s.CompletedBytes,
		SizeWhenDone:   s.SizeWhenDone,
		LeftUntilDone:  s.LeftUntilDone,
		UploadedBytes:  s.UploadedBytes,
		DownloadRate:   roundRate(s.DownloadRate),
		UploadRate:     roundRate(s.UploadRate),
		Peers:          s.Peers,
		Seeders:        s.Seeders,
		Ratio:          s.Ratio(),
		ETASeconds:     -1,
		HasMetadata:    s.HasMetadata,
	}
	if eta := s.ETA(); eta >= 0 {
		t.ETASeconds = int64(eta.Seconds())
	}
	if !s.AddedAt.IsZero() {
		t.AddedAt = s.AddedAt.Unix()
	}
	if !s.FinishedAt.IsZero() {
		t.FinishedAt = s.FinishedAt.Unix()
	}
	return t
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	statuses := h.engine.List()
	out := make([]Torrent, 0, len(statuses))
	for _, s := range statuses {
		out = append(out, torrentOf(s))
	}
	writeJSON(w, http.StatusOK, map[string]any{"torrents": out})
}

func (h *Handler) detail(w http.ResponseWriter, r *http.Request) {
	id, ok := h.id(w, r)
	if !ok {
		return
	}
	s, found := h.engine.Status(id)
	if !found {
		writeError(w, http.StatusNotFound, fmt.Errorf("no torrent with id %d", id))
		return
	}
	t := torrentOf(s)
	t.Trackers = h.engine.AnnounceURLs(id)
	writeJSON(w, http.StatusOK, t)
}

// File is one file in a torrent.
type File struct {
	Index     int    `json:"index"`
	Path      string `json:"path"`
	Length    int64  `json:"length"`
	Completed int64  `json:"completed"`
	Selected  bool   `json:"selected"`
}

func (h *Handler) files(w http.ResponseWriter, r *http.Request) {
	id, ok := h.id(w, r)
	if !ok {
		return
	}
	engineFiles := h.engine.Files(id)
	out := make([]File, 0, len(engineFiles))
	for _, f := range engineFiles {
		out = append(out, File{
			Index: f.Index, Path: f.Path, Length: f.Length,
			Completed: f.Completed, Selected: f.Selected,
		})
	}
	// Empty rather than an error before metadata: the UI polls this on a
	// freshly added magnet.
	writeJSON(w, http.StatusOK, map[string]any{"files": out})
}

type selectionRequest struct {
	// Selected is the indices to download. Everything else is skipped.
	Selected []int `json:"selected"`
}

func (h *Handler) setFiles(w http.ResponseWriter, r *http.Request) {
	id, ok := h.id(w, r)
	if !ok {
		return
	}
	var req selectionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	files := h.engine.Files(id)
	if len(files) == 0 {
		writeError(w, http.StatusConflict, errors.New("the torrent's metadata has not arrived yet"))
		return
	}
	selection := make([]bool, len(files))
	for _, index := range req.Selected {
		if index < 0 || index >= len(selection) {
			writeError(w, http.StatusBadRequest, fmt.Errorf("file index %d is out of range", index))
			return
		}
		selection[index] = true
	}
	if err := h.engine.SetFileSelection(r.Context(), id, selection); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// Peer is one live peer connection.
type Peer struct {
	Addr         string  `json:"addr"`
	Client       string  `json:"client"`
	Source       string  `json:"source"`
	DownloadRate float64 `json:"download_rate"`
	UploadRate   float64 `json:"upload_rate"`
	BytesRead    int64   `json:"bytes_read"`
	BytesWritten int64   `json:"bytes_written"`
	PiecesHave   int     `json:"pieces_have"`
}

func (h *Handler) peers(w http.ResponseWriter, r *http.Request) {
	id, ok := h.id(w, r)
	if !ok {
		return
	}
	enginePeers := h.engine.Peers(id)
	out := make([]Peer, 0, len(enginePeers))
	for _, p := range enginePeers {
		out = append(out, Peer{
			Addr: p.Addr, Client: p.Client, Source: p.Source,
			DownloadRate: roundRate(p.DownloadRate), UploadRate: roundRate(p.UploadRate),
			BytesRead: p.BytesRead, BytesWritten: p.BytesWritten,
			PiecesHave: p.PiecesHave,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"peers": out})
}

// Pieces is the completed-pieces bitfield.
type Pieces struct {
	// Bitfield is base64 of the raw bitfield, least significant bit first
	// within each byte.
	Bitfield string `json:"bitfield"`
	// Count is the number of pieces. It is explicit because the bitfield is
	// byte-aligned: without it the trailing padding bits of the last byte read
	// as missing pieces.
	Count int `json:"count"`
}

func (h *Handler) pieces(w http.ResponseWriter, r *http.Request) {
	id, ok := h.id(w, r)
	if !ok {
		return
	}
	bits, count := h.engine.Bitfield(id)
	writeJSON(w, http.StatusOK, Pieces{
		Bitfield: base64.StdEncoding.EncodeToString(bits),
		Count:    count,
	})
}

type addRequest struct {
	// Source is a magnet URI or an http(s) URL to a .torrent.
	Source string `json:"source"`
	// Metainfo is a base64 .torrent, for the file-upload path.
	Metainfo    string `json:"metainfo"`
	DownloadDir string `json:"download_dir"`
	Paused      bool   `json:"paused"`
}

func (h *Handler) add(w http.ResponseWriter, r *http.Request) {
	var req addRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxUploadBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}

	add := engine.AddRequest{DownloadDir: req.DownloadDir, Paused: req.Paused}
	switch {
	case req.Metainfo != "":
		blob, err := base64.StdEncoding.DecodeString(req.Metainfo)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid base64 in metainfo: %w", err))
			return
		}
		add.Metainfo = blob
	case req.Source != "":
		add.Source = req.Source
	default:
		writeError(w, http.StatusBadRequest, errors.New("either source or metainfo is required"))
		return
	}

	id, _, err := h.engine.Add(r.Context(), add)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s, _ := h.engine.Status(id)
	writeJSON(w, http.StatusCreated, torrentOf(s))
}

func (h *Handler) remove(w http.ResponseWriter, r *http.Request) {
	id, ok := h.id(w, r)
	if !ok {
		return
	}
	deleteData, _ := strconv.ParseBool(r.URL.Query().Get("delete_data"))
	if err := h.engine.Remove(r.Context(), id, deleteData); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) start(w http.ResponseWriter, r *http.Request) {
	h.setPaused(w, r, false)
}

func (h *Handler) stop(w http.ResponseWriter, r *http.Request) {
	h.setPaused(w, r, true)
}

func (h *Handler) setPaused(w http.ResponseWriter, r *http.Request, paused bool) {
	id, ok := h.id(w, r)
	if !ok {
		return
	}
	var err error
	if paused {
		err = h.engine.Stop(r.Context(), id)
	} else {
		err = h.engine.Start(r.Context(), id)
	}
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	down, up := h.engine.SessionRates()
	var active, paused, errored int
	var total, completed int64
	for _, s := range h.engine.List() {
		switch {
		case s.Error != "":
			errored++
		case s.Paused:
			paused++
		default:
			active++
		}
		total += s.TotalBytes
		completed += s.CompletedBytes
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"download_rate":   roundRate(down),
		"upload_rate":     roundRate(up),
		"torrents":        active + paused + errored,
		"active":          active,
		"paused":          paused,
		"errored":         errored,
		"total_bytes":     total,
		"completed_bytes": completed,
	})
}

// id extracts and validates the {id} path value.
func (h *Handler) id(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid torrent id %q", raw))
		return 0, false
	}
	return id, true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	// Marshalled before the status goes out, not streamed after it.
	//
	// Encoding straight to the ResponseWriter means a value encoding/json
	// refuses (a NaN rate, say) arrives as 200 with an empty body and a
	// debug-level log: the client reports a JSON parse error at line 1 column 1
	// and the server believes it succeeded. Buffering costs a copy of a response
	// that is already small, and turns that into a 500 that says what happened.
	raw, err := json.Marshal(body)
	if err != nil {
		slog.Error("failed to encode an API response", "error", err)
		http.Error(w, `{"error":"failed to encode the response"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Stale cached responses once resurrected bugs that had already been
	// fixed, so nothing this daemon serves is cacheable.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if _, err := w.Write(append(raw, '\n')); err != nil {
		slog.Debug("failed to write an API response", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"error": err.Error()})
}
