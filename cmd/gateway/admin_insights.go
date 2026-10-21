package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/djwkvnwincbnak78/nexus/internal/model"
	"github.com/djwkvnwincbnak78/nexus/internal/settings"
)

// GET /admin/api/settings
func (s *server) adminSettingsGet(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil {
		writeError(w, http.StatusServiceUnavailable, "no_settings", "settings store not initialised")
		return
	}
	items := make([]map[string]any, 0, len(settings.Known))
	for k, desc := range settings.Known {
		items = append(items, map[string]any{
			"key": k, "value": s.settings.Get(k, settings.Defaults[k]), "description": desc,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data": s.settings.Snapshot(), "items": items,
	})
}

// PUT /admin/api/settings
func (s *server) adminSettingsPut(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil {
		writeError(w, http.StatusServiceUnavailable, "no_settings", "settings store not initialised")
		return
	}
	var raw map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&raw); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// Accept either {"settings":{...}} or a flat object.
	if nested, ok := raw["settings"].(map[string]any); ok {
		raw = nested
	}
	if len(raw) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "no settings supplied")
		return
	}

	kv := make(map[string]string, len(raw))
	for k, v := range raw {
		if _, ok := settings.Known[k]; !ok {
			writeError(w, http.StatusBadRequest, "unknown_setting", "unknown setting: "+k)
			return
		}
		switch val := v.(type) {
		case string:
			kv[k] = strings.TrimSpace(val)
		case bool:
			kv[k] = strconv.FormatBool(val)
		case float64:
			kv[k] = strconv.FormatFloat(val, 'f', -1, 64)
		default:
			writeError(w, http.StatusBadRequest, "invalid_value", "setting "+k+" must be string, bool or number")
			return
		}
	}

	if shadow, ok := kv["shadow_model"]; ok && shadow != "" {
		if len(s.snapshot().candidates[shadow]) == 0 {
			writeError(w, http.StatusBadRequest, "unknown_model", "shadow_model is not served by any enabled channel: "+shadow)
			return
		}
	}
	if v, ok := kv["shadow_sample_rate"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 100 {
			writeError(w, http.StatusBadRequest, "invalid_value", "shadow_sample_rate must be 0-100")
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.settings.Update(ctx, kv); err != nil {
		writeError(w, http.StatusInternalServerError, "settings_write_failed", err.Error())
		return
	}
	s.metrics.Inc("gateway_settings_updates_total", nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"saved": true, "applied": "hot", "data": s.settings.Snapshot(),
	})
}

type historyRow struct {
	ID               int64         `json:"id"`
	RequestID        string        `json:"request_id"`
	Model            string        `json:"model_name"`
	Provider         string        `json:"provider"`
	RouteReason      string        `json:"route_reason"`
	PromptTokens     int           `json:"prompt_tokens"`
	CompletionTokens int           `json:"completion_tokens"`
	Cost             model.NUMERIC `json:"cost"`
	Status           string        `json:"status"`
	HTTPStatus       int           `json:"http_status"`
	LatencyMS        int           `json:"latency_ms"`
	Cached           bool          `json:"cached"`
	RequestTime      time.Time     `json:"request_time"`
	RequestBody      string        `json:"request_body"`
}

// GET /admin/api/history?limit=50
func (s *server) adminHistory(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, "no_database", "history requires PostgreSQL")
		return
	}
	limit := clampInt(r.URL.Query().Get("limit"), 50, 1, 200)
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, request_id::text, model_name, provider, route_reason,
		       prompt_tokens, completion_tokens, cost, status, http_status,
		       latency_ms, cached, request_time,
		       COALESCE(request_body::text, '')
		  FROM request_logs
		 ORDER BY request_time DESC
		 LIMIT $1`, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "history_failed", err.Error())
		return
	}
	defer rows.Close()

	out := make([]historyRow, 0, limit)
	for rows.Next() {
		var h historyRow
		if err := rows.Scan(&h.ID, &h.RequestID, &h.Model, &h.Provider, &h.RouteReason,
			&h.PromptTokens, &h.CompletionTokens, &h.Cost, &h.Status, &h.HTTPStatus,
			&h.LatencyMS, &h.Cached, &h.RequestTime, &h.RequestBody); err != nil {
			writeError(w, http.StatusInternalServerError, "history_scan", err.Error())
			return
		}
		out = append(out, h)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

type replayInput struct {
	RequestID string          `json:"request_id"`
	Model     string          `json:"model"`
	Messages  []model.Message `json:"messages"`
	MaxTokens int             `json:"max_tokens"`
}

// POST /admin/api/replay
func (s *server) adminReplay(w http.ResponseWriter, r *http.Request) {
	if !s.settings.Bool("replay_enabled", true) {
		writeError(w, http.StatusForbidden, "replay_disabled", "replay is disabled in settings")
		return
	}
	var in replayInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	req := &model.ChatRequest{Model: in.Model, Messages: in.Messages, MaxTokens: in.MaxTokens}
	if len(req.Messages) == 0 {
		if in.RequestID == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "request_id or messages required")
			return
		}
		if s.db == nil {
			writeError(w, http.StatusServiceUnavailable, "no_database", "replay by id requires PostgreSQL")
			return
		}
		var body string
		err := s.db.QueryRowContext(r.Context(), `
			SELECT COALESCE(request_body::text, '')
			  FROM request_logs WHERE request_id = $1
			 ORDER BY request_time DESC LIMIT 1`, in.RequestID).Scan(&body)
		if err != nil || body == "" {
			writeError(w, http.StatusNotFound, "request_not_found", "no stored body for that request id")
			return
		}
		if err := json.Unmarshal([]byte(body), req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_stored_body", err.Error())
			return
		}
	}
	if in.Model != "" {
		req.Model = in.Model
	}
	req.Stream = false
	if req.Model == "" || len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "model and messages are required")
		return
	}
	cands := s.snapshot().candidates[req.Model]
	if len(cands) == 0 {
		writeError(w, http.StatusNotFound, "model_not_found", "no channel serves model "+req.Model)
		return
	}

	timeout := time.Duration(s.settings.Int("replay_timeout_seconds", 120)) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	started := time.Now()
	decision, resp, err := s.complete(ctx, req, cands)
	latency := elapsedMS(started)
	if err != nil {
		writeError(w, http.StatusBadGateway, "replay_failed", err.Error())
		return
	}
	channel := uint(0)
	reason := ""
	if decision != nil {
		channel, reason = decision.Channel.ID, decision.Reason
	}
	s.metrics.Inc("gateway_replays_total", map[string]string{"model": req.Model})
	writeJSON(w, http.StatusOK, map[string]any{
		"model": req.Model, "channel_id": channel, "route_reason": reason,
		"latency_ms": latency, "replayed_from": in.RequestID, "response": resp,
	})
}

type shadowRow struct {
	ID            int64         `json:"id"`
	TraceID       string        `json:"trace_id"`
	MainModel     string        `json:"main_model"`
	ShadowModel   string        `json:"shadow_model"`
	MainChannel   int64         `json:"main_channel"`
	ShadowChannel int64         `json:"shadow_channel"`
	MainLatency   int           `json:"main_latency"`
	ShadowLatency int           `json:"shadow_latency"`
	MainCost      model.NUMERIC `json:"main_cost"`
	ShadowCost    model.NUMERIC `json:"shadow_cost"`
	MainTokens    int           `json:"main_tokens"`
	ShadowTokens  int           `json:"shadow_tokens"`
	Status        string        `json:"status"`
	ErrorMessage  string        `json:"error_message"`
	CreatedAt     time.Time     `json:"created_at"`
}

// GET /admin/api/shadow?limit=100
func (s *server) adminShadow(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, "no_database", "shadow logs require PostgreSQL")
		return
	}
	limit := clampInt(r.URL.Query().Get("limit"), 100, 1, 500)
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, trace_id, main_model, shadow_model,
		       COALESCE(main_channel,0), COALESCE(shadow_channel,0),
		       main_latency, shadow_latency, main_cost, shadow_cost,
		       main_tokens, shadow_tokens, status, COALESCE(error_message,''), created_at
		  FROM shadow_logs ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "shadow_failed", err.Error())
		return
	}
	defer rows.Close()
	out := make([]shadowRow, 0, limit)
	for rows.Next() {
		var x shadowRow
		if err := rows.Scan(&x.ID, &x.TraceID, &x.MainModel, &x.ShadowModel,
			&x.MainChannel, &x.ShadowChannel, &x.MainLatency, &x.ShadowLatency,
			&x.MainCost, &x.ShadowCost, &x.MainTokens, &x.ShadowTokens,
			&x.Status, &x.ErrorMessage, &x.CreatedAt); err != nil {
			writeError(w, http.StatusInternalServerError, "shadow_scan", err.Error())
			return
		}
		out = append(out, x)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func clampInt(raw string, def, lo, hi int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}
