package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/myml/nvrsync/internal/store"
	"github.com/myml/nvrsync/internal/syncer"
)

const (
	defaultSegmentLimit = 200
	maxSegmentLimit     = 1000
	defaultLogLimit     = 200
	maxLogLimit         = 1000
)

// handleStats serves the sync page dashboard in one request.
func (s *server) handleStats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ch, err := parseIntParam(q.Get("ch"), 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ch: "+err.Error())
		return
	}
	from, err := parseTimeParam(q.Get("from"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid from: "+err.Error())
		return
	}
	to, err := parseTimeParam(q.Get("to"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid to: "+err.Error())
		return
	}
	if from != 0 && to != 0 && from > to {
		writeError(w, http.StatusBadRequest, "from must not be after to")
		return
	}
	state := s.st.SyncState()
	resp := struct {
		store.Stats
		Speed     float64 `json:"speed"`
		Running   bool    `json:"running"`
		StartedAt int64   `json:"started_at"`
	}{
		Stats:     s.st.Stats(store.SegFilter{Ch: ch, From: from, To: to}),
		Speed:     state.Speed,
		Running:   state.Running,
		StartedAt: state.StartedAt,
	}
	writeOK(w, http.StatusOK, resp)
}

func (s *server) handleSegments(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	ch, err := parseIntParam(q.Get("ch"), 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ch: "+err.Error())
		return
	}
	from, err := parseTimeParam(q.Get("from"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid from: "+err.Error())
		return
	}
	to, err := parseTimeParam(q.Get("to"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid to: "+err.Error())
		return
	}
	if from != 0 && to != 0 && from > to {
		writeError(w, http.StatusBadRequest, "from must not be after to")
		return
	}
	status := strings.TrimSpace(q.Get("status"))
	if status == "all" {
		status = ""
	}
	limit, err := parseLimit(q.Get("limit"), defaultSegmentLimit, maxSegmentLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid limit: "+err.Error())
		return
	}
	offset, err := parseIntParam(q.Get("offset"), 0)
	if err != nil || offset < 0 {
		writeError(w, http.StatusBadRequest, "invalid offset")
		return
	}

	items, total := s.st.Segments(store.SegFilter{
		Ch:     ch,
		From:   from,
		To:     to,
		Status: status,
		Limit:  limit,
		Offset: offset,
	})
	if items == nil {
		items = []store.Segment{}
	}
	writeOK(w, http.StatusOK, struct {
		Total int             `json:"total"`
		Items []store.Segment `json:"items"`
	}{Total: total, Items: items})
}

func (s *server) handleDays(w http.ResponseWriter, r *http.Request) {
	ch, err := parseIntParam(r.URL.Query().Get("ch"), 0)
	if err != nil || ch < 0 {
		writeError(w, http.StatusBadRequest, "invalid ch")
		return
	}
	days := s.st.Days(ch)
	if days == nil {
		days = []store.DayInfo{}
	}
	writeOK(w, http.StatusOK, days)
}

// handleCatalogRefresh reconciles the local index with the NVR list.
func (s *server) handleCatalogRefresh(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
	defer cancel()
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"
	res, err := s.sy.RefreshCatalogForce(ctx, force)
	if err != nil {
		// A refresh that is already in flight is not an error: the caller
		// simply skips this round.
		if errors.Is(err, syncer.ErrCatalogBusy) {
			writeOK(w, http.StatusOK, map[string]any{"skipped": true, "reason": "对账进行中"})
			return
		}
		if errors.Is(err, syncer.ErrAlreadyRunning) {
			writeOK(w, http.StatusOK, map[string]any{"skipped": true, "reason": "同步进行中"})
			return
		}
		status := http.StatusBadGateway
		writeError(w, status, err.Error())
		return
	}
	writeOK(w, http.StatusOK, res)
}

// handleNVRScan lists NVR recordings for a range without downloading them, so
// the sync page can browse any day before deciding what to pull.
func (s *server) handleNVRScan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Channels []int `json:"channels"`
		From     int64 `json:"from"`
		To       int64 `json:"to"`
	}
	if err := decodeJSON(w, r, &req, true); err != nil {
		writeError(w, http.StatusBadRequest, "invalid scan request: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	listed, added, err := s.sy.ScanOnly(ctx, req.Channels, req.From, req.To)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeOK(w, http.StatusOK, map[string]any{"listed": listed, "added": added})
}

func (s *server) handleSyncStart(w http.ResponseWriter, r *http.Request) {
	var req syncer.SyncRequest
	if err := decodeJSON(w, r, &req, true); err != nil {
		writeError(w, http.StatusBadRequest, "invalid sync request: "+err.Error())
		return
	}
	if req.From != 0 && req.To != 0 && req.From >= req.To {
		writeError(w, http.StatusBadRequest, "from must be before to")
		return
	}
	if err := s.sy.Start(req); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, syncer.ErrAlreadyRunning) {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	writeOK(w, http.StatusOK, map[string]any{"running": true})
}

func (s *server) handleSyncStop(w http.ResponseWriter, r *http.Request) {
	s.sy.Stop()
	writeOK(w, http.StatusOK, map[string]any{"running": s.sy.Running()})
}

func (s *server) handleSyncStatus(w http.ResponseWriter, r *http.Request) {
	writeOK(w, http.StatusOK, s.st.SyncState())
}

func (s *server) handleLogs(w http.ResponseWriter, r *http.Request) {
	limit, err := parseLimit(r.URL.Query().Get("limit"), defaultLogLimit, maxLogLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid limit: "+err.Error())
		return
	}
	level := strings.TrimSpace(r.URL.Query().Get("level"))
	if level == "all" {
		level = ""
	}
	if level != "" && level != "info" && level != "warn" && level != "error" {
		writeError(w, http.StatusBadRequest, `level must be one of "info", "warn", "error"`)
		return
	}

	ch, err := parseIntParam(r.URL.Query().Get("ch"), 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ch: "+err.Error())
		return
	}

	// The store filters by level/channel and returns newest first, so a request
	// for the newest N errors does not get diluted by unrelated log levels.
	logs := s.st.Logs(limit, level, ch)
	if logs == nil {
		logs = []store.LogEntry{}
	}
	writeOK(w, http.StatusOK, logs)
}
