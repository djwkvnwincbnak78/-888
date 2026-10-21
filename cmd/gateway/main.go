package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	mrand "math/rand"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/djwkvnwincbnak78/nexus/internal/admin"
	"github.com/djwkvnwincbnak78/nexus/internal/audit"
	"github.com/djwkvnwincbnak78/nexus/internal/authz"
	"github.com/djwkvnwincbnak78/nexus/internal/billing"
	"github.com/djwkvnwincbnak78/nexus/internal/cache"
	"github.com/djwkvnwincbnak78/nexus/internal/compress"
	"github.com/djwkvnwincbnak78/nexus/internal/middleware"
	"github.com/djwkvnwincbnak78/nexus/internal/model"
	"github.com/djwkvnwincbnak78/nexus/internal/observability"
	"github.com/djwkvnwincbnak78/nexus/internal/provider"
	"github.com/djwkvnwincbnak78/nexus/internal/rerank"
	"github.com/djwkvnwincbnak78/nexus/internal/router"
	"github.com/djwkvnwincbnak78/nexus/internal/secretbox"
	"github.com/djwkvnwincbnak78/nexus/internal/security"
	"github.com/djwkvnwincbnak78/nexus/internal/settings"
	"github.com/djwkvnwincbnak78/nexus/internal/tracing"
	"github.com/djwkvnwincbnak78/nexus/web"
)

const version = "1.2.0"

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

type keyPolicy struct {
	key string
	cfg *middleware.UserConfig
}

type adminKey struct {
	key      string
	tenantID uint
	role     authz.Role
}

type config struct {
	port           string
	dbDSN          string
	redisDSN       string
	keys           []keyPolicy
	prices         map[string]model.NUMERIC
	candidates     map[string][]router.Candidate
	providers      map[string]model.Provider
	policy         security.Policy
	balance        model.NUMERIC
	requestTimeout time.Duration
	streamTimeout  time.Duration
	adminKeys      []adminKey
	compress       bool
	outboundClient *http.Client
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// newRequestID returns a UUID-v4-shaped id; request_logs.request_id is UUID.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-" + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// parseAdminKeys reads ADMIN_API_KEYS="key:tenantID:role,...".
func parseAdminKeys(raw string) ([]adminKey, error) {
	var out []adminKey
	for _, entry := range splitCSV(raw) {
		parts := strings.Split(entry, ":")
		if len(parts) != 3 {
			return nil, fmt.Errorf("ADMIN_API_KEYS entry %q must be key:tenantID:role", entry)
		}
		tid, err := strconv.ParseUint(parts[1], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("ADMIN_API_KEYS tenant %q: %w", parts[1], err)
		}
		role := authz.Role(parts[2])
		switch role {
		case authz.RoleOwner, authz.RoleAdmin, authz.RoleMember, authz.RoleViewer:
		default:
			return nil, fmt.Errorf("ADMIN_API_KEYS role %q invalid", parts[2])
		}
		out = append(out, adminKey{key: parts[0], tenantID: uint(tid), role: role})
	}
	return out, nil
}

// parseKeys reads GATEWAY_API_KEYS="sk-a:1:600:gpt-4o|claude-3,sk-b:2:120".
// The optional 4th field is a '|' separated model whitelist.
func parseKeys(raw string) ([]keyPolicy, error) {
	var out []keyPolicy
	for _, entry := range splitCSV(raw) {
		parts := strings.Split(entry, ":")
		if len(parts) < 3 || len(parts) > 4 {
			return nil, fmt.Errorf("GATEWAY_API_KEYS entry %q must be key:userID:rateLimit[:model1|model2]", entry)
		}
		uid, err := strconv.ParseUint(parts[1], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("GATEWAY_API_KEYS user id %q: %w", parts[1], err)
		}
		rate, err := strconv.Atoi(parts[2])
		if err != nil || rate < 0 {
			return nil, fmt.Errorf("GATEWAY_API_KEYS rate %q invalid", parts[2])
		}
		cfg := &middleware.UserConfig{UserID: uint(uid), RateLimit: rate}
		if len(parts) == 4 && strings.TrimSpace(parts[3]) != "" {
			cfg.ModelWhitelist = strings.Split(parts[3], "|")
		}
		out = append(out, keyPolicy{key: parts[0], cfg: cfg})
	}
	if len(out) == 0 {
		return nil, errors.New("GATEWAY_API_KEYS is required (format: key:userID:rateLimit[:models])")
	}
	return out, nil
}

func loadConfig() (*config, error) {
	cfg := &config{
		port:           env("PORT", "8080"),
		dbDSN:          os.Getenv("DB_DSN"),
		redisDSN:       os.Getenv("REDIS_DSN"),
		prices:         make(map[string]model.NUMERIC),
		candidates:     make(map[string][]router.Candidate),
		providers:      make(map[string]model.Provider),
		requestTimeout: time.Duration(envInt("REQUEST_TIMEOUT_SECONDS", 120)) * time.Second,
		streamTimeout:  time.Duration(envInt("STREAM_TIMEOUT_SECONDS", 300)) * time.Second,
	}

	keys, err := parseKeys(env("GATEWAY_API_KEYS", ""))
	if err != nil {
		return nil, err
	}
	cfg.keys = keys

	if cfg.dbDSN != "" && !strings.HasPrefix(cfg.dbDSN, "postgres://") && !strings.HasPrefix(cfg.dbDSN, "postgresql://") {
		return nil, errors.New("DB_DSN must be a postgres:// URL")
	}

	hosts := splitCSV(env("OUTBOUND_ALLOWLIST", "api.openai.com,api.anthropic.com"))
	cfg.policy = security.Policy{
		Hosts:           hosts,
		Ports:           splitCSV(env("OUTBOUND_PORTS", "443")),
		AllowPrivateIPs: env("OUTBOUND_ALLOW_PRIVATE", "false") == "true",
	}
	if cfg.policy.AllowPrivateIPs {
		slog.Warn("ssrf_private_addresses_allowed",
			"note", "OUTBOUND_ALLOW_PRIVATE=true disables the private-range block; only use with self-hosted upstreams")
	}
	safeClient := cfg.policy.NewClient(0, 3) // no global timeout: bounded per request by context
	cfg.outboundClient = safeClient

	openAIKey := os.Getenv("OPENAI_API_KEY")
	anthropicKey := os.Getenv("ANTHROPIC_API_KEY")
	ollamaModels := splitCSV(env("OLLAMA_MODELS", ""))
	ollamaURL := env("OLLAMA_BASE_URL", "")

	openAIModels := splitCSV(env("OPENAI_MODELS", ""))
	anthropicModels := splitCSV(env("ANTHROPIC_MODELS", ""))
	if openAIKey == "" && anthropicKey == "" && len(ollamaModels) == 0 {
		return nil, errors.New("at least one of OPENAI_API_KEY / ANTHROPIC_API_KEY / OLLAMA_MODELS is required")
	}
	if openAIKey == "" && len(openAIModels) > 0 {
		return nil, errors.New("OPENAI_MODELS set but OPENAI_API_KEY missing")
	}
	if anthropicKey == "" && len(anthropicModels) > 0 {
		return nil, errors.New("ANTHROPIC_MODELS set but ANTHROPIC_API_KEY missing")
	}

	openAIPrice := model.FromFloat(float64(envInt("OPENAI_PRICE_PER_1M", 10)))
	anthropicPrice := model.FromFloat(float64(envInt("ANTHROPIC_PRICE_PER_1M", 15)))
	openAIWeight := envInt("OPENAI_CHANNEL_WEIGHT", 10)
	anthropicWeight := envInt("ANTHROPIC_CHANNEL_WEIGHT", 5)

	if openAIKey != "" {
		cfg.providers["openai"] = provider.NewOpenAI(provider.Config{
			BaseURL: env("OPENAI_BASE_URL", "https://api.openai.com/v1"),
			APIKey:  openAIKey,
			Client:  safeClient,
		})
		ch := &model.Channel{
			ID: 1, Name: "openai-primary", Provider: "openai",
			BaseURL: env("OPENAI_BASE_URL", "https://api.openai.com/v1"),
			Weight:  openAIWeight, Models: openAIModels, RateLimit: envInt("OPENAI_CHANNEL_RATE", 600),
		}
		for _, m := range openAIModels {
			cfg.prices[m] = openAIPrice
			cfg.candidates[m] = append(cfg.candidates[m], router.Candidate{
				Model:   &model.ModelChannel{ModelName: m, ChannelID: ch.ID, PricePer1M: openAIPrice},
				Channel: ch, Healthy: true,
			})
		}
	}

	if anthropicKey != "" {
		cfg.providers["anthropic"] = provider.NewAnthropic(provider.Config{
			BaseURL:    env("ANTHROPIC_BASE_URL", "https://api.anthropic.com/v1"),
			APIKey:     anthropicKey,
			APIVersion: env("ANTHROPIC_VERSION", "2023-06-01"),
			Client:     safeClient,
		})
		ch := &model.Channel{
			ID: 2, Name: "anthropic-primary", Provider: "anthropic",
			BaseURL: env("ANTHROPIC_BASE_URL", "https://api.anthropic.com/v1"),
			Weight:  anthropicWeight, Models: anthropicModels, RateLimit: envInt("ANTHROPIC_CHANNEL_RATE", 300),
		}
		for _, m := range anthropicModels {
			cfg.prices[m] = anthropicPrice
			cfg.candidates[m] = append(cfg.candidates[m], router.Candidate{
				Model:   &model.ModelChannel{ModelName: m, ChannelID: ch.ID, PricePer1M: anthropicPrice},
				Channel: ch, Healthy: true,
			})
		}
	}

	// Local Ollama: private HTTP endpoint, so the SSRF allowlist must be
	// explicitly opened (OUTBOUND_ALLOW_PRIVATE) or we bypass it intentionally
	// for a loopback daemon the operator configured themselves.
	if ollamaURL != "" || len(ollamaModels) > 0 {
		if ollamaURL == "" {
			ollamaURL = "http://127.0.0.1:11434"
		}
		if len(ollamaModels) == 0 {
			return nil, errors.New("OLLAMA_BASE_URL set but OLLAMA_MODELS missing")
		}
		var ollamaClient = safeClient
		if !cfg.policy.AllowPrivateIPs {
			slog.Warn("ollama_direct_client",
				"note", "OUTBOUND_ALLOW_PRIVATE=false; using a direct client for the operator-configured Ollama endpoint")
			ollamaClient = nil
		}
		cfg.providers["ollama"] = provider.NewOllama(provider.Config{
			BaseURL: ollamaURL,
			Client:  ollamaClient,
			Retry:   provider.RetryPolicy{Attempts: 2, Base: 100 * time.Millisecond, Max: time.Second},
		})
		ch := &model.Channel{
			ID: 3, Name: "ollama-local", Provider: "ollama",
			BaseURL: ollamaURL, Weight: envInt("OLLAMA_CHANNEL_WEIGHT", 5),
			Models: ollamaModels, RateLimit: envInt("OLLAMA_CHANNEL_RATE", 300),
		}
		price := model.FromFloat(float64(envInt("OLLAMA_PRICE_PER_1M", 0)))
		for _, m := range ollamaModels {
			cfg.prices[m] = price
			cfg.candidates[m] = append(cfg.candidates[m], router.Candidate{
				Model:   &model.ModelChannel{ModelName: m, ChannelID: ch.ID, PricePer1M: price},
				Channel: ch, Healthy: true,
			})
		}
	}

	if len(cfg.candidates) == 0 {
		return nil, errors.New("no models configured: set OPENAI_MODELS / ANTHROPIC_MODELS / OLLAMA_MODELS")
	}

	adminKeys, err := parseAdminKeys(env("ADMIN_API_KEYS", ""))
	if err != nil {
		return nil, err
	}
	cfg.adminKeys = adminKeys

	cfg.compress = env("PROMPT_COMPRESSION", "true") == "true"
	cfg.balance = model.FromFloat(float64(envInt("INITIAL_BALANCE", 1000)))
	return cfg, nil
}

// ---------------------------------------------------------------------------
// Server
// ---------------------------------------------------------------------------

// catalog is an immutable snapshot of routes+providers, swapped atomically
// when channels change in PostgreSQL.
type catalog struct {
	candidates map[string][]router.Candidate
	providers  map[uint]model.Provider
}

type server struct {
	cfg        *config
	cache      *cache.Cache
	ledger     *billing.Ledger
	router     *router.Router
	tracker    *router.LatencyTracker
	limiter    *middleware.Limiter
	metrics    *observability.Metrics
	audit      audit.Sink
	reranker   *rerank.BM25
	admin      authz.Store
	adminSvc   *admin.Service
	cat        atomic.Pointer[catalog]
	db         *sql.DB
	box        *secretbox.Box
	settings   *settings.Store
	shadowSink func(shadowRecord) error

	storeBackend  string
	ledgerBackend string
	started       time.Time
	requests      atomic.Int64
	failures      atomic.Int64
}

func newServer(cfg *config, store cache.Store, limit *middleware.Limiter,
	ledger *billing.Ledger, metrics *observability.Metrics, sink audit.Sink) *server {
	tracker := router.NewLatencyTracker()
	srv := &server{
		cfg:      cfg,
		cache:    cache.New(store, cache.Config{}), // exact-only until a real embedding backend is injected
		ledger:   ledger,
		router:   router.NewRouter(tracker),
		tracker:  tracker,
		limiter:  limit,
		metrics:  metrics,
		audit:    sink,
		reranker: rerank.New(),
		settings: settings.New(nil),
		started:  time.Now(),
	}
	cat := &catalog{candidates: cfg.candidates, providers: make(map[uint]model.Provider)}
	for _, cands := range cfg.candidates {
		for _, c := range cands {
			if p, ok := cfg.providers[c.Channel.Provider]; ok {
				cat.providers[c.Channel.ID] = p
			}
		}
	}
	srv.cat.Store(cat)
	return srv
}

// snapshot returns the current immutable catalog.
func (s *server) snapshot() *catalog {
	if c := s.cat.Load(); c != nil {
		return c
	}
	return &catalog{candidates: map[string][]router.Candidate{}, providers: map[uint]model.Provider{}}
}

// reloadCatalog rebuilds routes/providers from PostgreSQL after an admin edit.
func (s *server) reloadCatalog(_ *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.loadCatalogFromDB(ctx); err != nil {
		slog.Warn("catalog_reload_failed", "err", err)
	}
}

// parseTextArray converts PostgreSQL's {"a","b"} literal into a slice.
func parseTextArray(raw string) []string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "{")
	raw = strings.TrimSuffix(raw, "}")
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, `"`)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// loadCatalogFromDB reads enabled channels and decrypts their keys. It is also
// called by the 30s refresh loop, so DB edits take effect without a restart.
func (s *server) loadCatalogFromDB(ctx context.Context) error {
	if s.db == nil {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, COALESCE(base_url, ''), provider,
		       COALESCE(api_key, ''), COALESCE(api_key_cipher, ''), models::text, weight
		  FROM channels WHERE enabled = TRUE ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()

	cat := &catalog{candidates: make(map[string][]router.Candidate), providers: make(map[uint]model.Provider)}
	for rows.Next() {
		var (
			id                  uint
			name, baseURL, prov string
			plainKey, cipherKey string
			modelsRaw           string
			weight              int
		)
		if err := rows.Scan(&id, &name, &baseURL, &prov, &plainKey, &cipherKey, &modelsRaw, &weight); err != nil {
			return err
		}
		models := parseTextArray(modelsRaw)
		key := plainKey
		if cipherKey != "" && s.box != nil {
			if dec, derr := s.box.Open(cipherKey); derr == nil {
				key = dec
			} else {
				slog.Warn("channel_key_decrypt_failed", "channel", id, "err", derr)
				continue
			}
		}
		if key == "" {
			slog.Warn("channel_skipped_no_key", "channel", id)
			continue
		}
		if weight <= 0 {
			weight = 1
		}
		ch := &model.Channel{ID: id, Name: name, Provider: prov, BaseURL: baseURL,
			Weight: weight, Models: models}

		var p model.Provider
		switch prov {
		case "openai":
			p = provider.NewOpenAI(provider.Config{BaseURL: baseURL, APIKey: key, Client: s.cfg.outboundClient})
		case "anthropic":
			p = provider.NewAnthropic(provider.Config{BaseURL: baseURL, APIKey: key, Client: s.cfg.outboundClient})
		case "ollama":
			p = provider.NewOllama(provider.Config{BaseURL: baseURL, Client: nil})
		default:
			slog.Warn("channel_unknown_provider", "channel", id, "provider", prov)
			continue
		}
		cat.providers[id] = p
		for _, m := range models {
			price := s.cfg.prices[m]
			cat.candidates[m] = append(cat.candidates[m], router.Candidate{
				Model:   &model.ModelChannel{ModelName: m, ChannelID: id, PricePer1M: price},
				Channel: ch, Healthy: true,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(cat.candidates) == 0 {
		slog.Warn("catalog_empty_after_reload", "note", "keeping previous snapshot")
		return nil
	}
	s.cat.Store(cat)
	slog.Info("catalog_loaded", "models", len(cat.candidates), "providers", len(cat.providers))
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write_json_failed", "err", err, "status", status)
	}
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"type": code, "message": msg},
	})
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	exact, semantic, miss := s.cache.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"version":        version,
		"uptime_seconds": int(time.Since(s.started).Seconds()),
		"storage":        s.storeBackend,
		"ledger":         s.ledgerBackend,
		"semantic_cache": s.cache.SemanticEnabled(),
		"requests":       s.requests.Load(),
		"failures":       s.failures.Load(),
		"audit_dropped":  s.audit.Dropped(),
		"cache": map[string]int64{
			"exact_hits": exact, "semantic_hits": semantic, "misses": miss,
		},
	})
}

func userFromContext(ctx context.Context) (*middleware.UserConfig, bool) {
	return middleware.UserConfigFrom(ctx)
}

func estimateTokens(s string) int { return len(s)/4 + 1 }

func promptTokens(req *model.ChatRequest) int {
	total := 0
	for _, m := range req.Messages {
		total += estimateTokens(m.Content) + 4
	}
	if total == 0 {
		total = 1
	}
	return total
}

// systemPrompt joins system/developer turns.
func systemPrompt(req *model.ChatRequest) string {
	var b strings.Builder
	for _, m := range req.Messages {
		if m.Role == "system" || m.Role == "developer" {
			b.WriteString(m.Content)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// promptText joins the conversational turns (system excluded: it is part of
// the cache key separately).
func promptText(req *model.ChatRequest) string {
	var b strings.Builder
	for _, m := range req.Messages {
		if m.Role == "system" || m.Role == "developer" {
			continue
		}
		b.WriteString(m.Role)
		b.WriteString(":")
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

func cacheKey(tenantID uint, req *model.ChatRequest) cache.Key {
	return cache.Key{
		TenantID:    strconv.FormatUint(uint64(tenantID), 10),
		Model:       req.Model,
		System:      systemPrompt(req),
		Prompt:      promptText(req),
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		StopWords:   req.Stop,
		// ToolsDigest stays empty: tools are not yet supported by the wire
		// model, and an empty digest must not be conflated with "no tools".
	}
}

func (s *server) record(e audit.Entry) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.audit.Write(ctx, e); err != nil {
		slog.Warn("audit_write_failed", "err", err)
	}
}

func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := newRequestID()
	if tp := r.Header.Get("traceparent"); tp != "" {
		r = r.WithContext(tracing.WithTraceparent(r.Context(), tp))
	}
	ctxTrace, span := tracing.Start(r.Context(), "chat.completions")
	defer span.End()
	r = r.WithContext(ctxTrace)
	w.Header().Set("X-Request-Id", reqID)
	w.Header().Set("X-Trace-Id", span.TraceID)
	span.SetAttribute("http.method", r.Method)
	span.SetAttribute("http.path", r.URL.Path)
	s.requests.Add(1)

	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusInternalServerError, "no_user_context", "request reached handler without user policy")
		return
	}

	rawBody, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if readErr != nil {
		s.record(audit.Entry{RequestID: reqID, UserID: user.UserID, Status: "error",
			HTTPStatus: http.StatusBadRequest, ErrorMessage: readErr.Error(), LatencyMS: elapsedMS(start)})
		writeError(w, http.StatusBadRequest, "invalid_request", "cannot read body: "+readErr.Error())
		return
	}
	var req model.ChatRequest
	if err := json.Unmarshal(rawBody, &req); err != nil {
		s.record(audit.Entry{RequestID: reqID, UserID: user.UserID, Status: "error",
			HTTPStatus: http.StatusBadRequest, ErrorMessage: err.Error(), LatencyMS: elapsedMS(start)})
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body: "+err.Error())
		return
	}
	if req.Model == "" || len(req.Messages) == 0 {
		s.record(audit.Entry{RequestID: reqID, UserID: user.UserID, Model: req.Model, Status: "error",
			HTTPStatus: http.StatusBadRequest, ErrorMessage: "model and messages required", LatencyMS: elapsedMS(start)})
		writeError(w, http.StatusBadRequest, "invalid_request", "model and messages are required")
		return
	}
	span.SetAttribute("gen_ai.request.model", req.Model)
	span.SetAttribute("tenant.id", user.UserID)
	cands := s.snapshot().candidates[req.Model]
	if len(cands) == 0 {
		s.record(audit.Entry{RequestID: reqID, UserID: user.UserID, Model: req.Model, Status: "error",
			HTTPStatus: http.StatusNotFound, ErrorMessage: "model not served", LatencyMS: elapsedMS(start)})
		writeError(w, http.StatusNotFound, "model_not_found", "no channel serves model "+req.Model)
		return
	}

	// Per-request deadline: streaming gets its own (longer) budget. There is no
	// global http.Client.Timeout, so this context is the only bound.
	timeout := s.cfg.requestTimeout
	if req.Stream {
		timeout = s.cfg.streamTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	// Killer feature #1: deterministic prompt compression before the cache key
	// and before billing, so savings are real and cache keys stay consistent.
	if s.settings.Bool("enable_compress", s.cfg.compress) {
		savedTokens := 0
		for i := range req.Messages {
			m := &req.Messages[i]
			if m.Role == "tool" || len(m.ToolCalls) > 0 {
				continue // never rewrite tool payloads
			}
			out, st := compress.Text(m.Content)
			if out != m.Content {
				m.Content = out
				savedTokens += st.TokensSaved
			}
		}
		if savedTokens > 0 {
			s.metrics.Add("gateway_prompt_tokens_saved_total",
				map[string]string{"model": req.Model}, uint64(savedTokens))
			w.Header().Set("X-Tokens-Saved", strconv.Itoa(savedTokens))
			span.SetAttribute("prompt.tokens_saved", savedTokens)
		}
	}

	key := cacheKey(user.UserID, &req)

	// --- cache lookup (exact only; semantic is opt-in and disabled) ---
	if cached, kind, err := s.cache.Get(ctx, key); err == nil && kind != cache.LookupMiss {
		s.metrics.Inc("gateway_cache_hits_total", map[string]string{"kind": string(kind)})
		w.Header().Set("X-Cache", string(kind))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(cached.Body); err != nil {
			slog.Warn("write_cached_body_failed", "err", err, "request_id", reqID)
		}
		s.record(audit.Entry{
			RequestID: reqID, UserID: user.UserID, Model: req.Model,
			RouteReason: "cache_" + string(kind), Status: "cache_hit", Cached: true,
			HTTPStatus: http.StatusOK, PromptTokens: cached.TokensSaved,
			LatencyMS: elapsedMS(start),
		})
		slog.Info("cache_hit", "model", req.Model, "kind", kind, "user", user.UserID, "request_id", reqID)
		return
	}

	// --- reserve budget ---
	estPrompt := promptTokens(&req)
	reserveCtx, reserveSpan := tracing.Start(ctx, "ledger.reserve")
	res, err := s.ledger.Reserve(reserveCtx, user.UserID, req.Model, estPrompt, req.MaxTokens)
	reserveSpan.SetAttribute("billing.model", req.Model)
	reserveSpan.SetAttribute("billing.prompt_tokens", estPrompt)
	reserveSpan.End()
	if err != nil {
		s.metrics.Inc("gateway_billing_rejections_total", nil)
		s.record(audit.Entry{RequestID: reqID, UserID: user.UserID, Model: req.Model,
			Status: "error", HTTPStatus: http.StatusPaymentRequired,
			ErrorMessage: err.Error(), LatencyMS: elapsedMS(start)})
		writeError(w, http.StatusPaymentRequired, "insufficient_balance", err.Error())
		return
	}
	settled := false
	defer func() {
		if settled {
			return
		}
		relCtx, relCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer relCancel()
		_, _ = s.ledger.Reconcile(relCtx, res.ID)
	}()

	if req.Stream {
		s.streamChat(ctx, w, user, &req, cands, res, reqID, start, rawBody)
		settled = true
		return
	}

	decision, resp, err := s.complete(ctx, &req, cands)
	if err != nil {
		s.failures.Add(1)
		s.metrics.Inc("gateway_upstream_errors_total", map[string]string{"model": req.Model})
		status := http.StatusBadGateway
		if code, ok := provider.StatusOf(err); ok && code >= 400 && code <= 599 {
			status = code
		}
		settleCtx, settleCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, settlementErr := s.ledger.Settle(settleCtx, res.ID, estPrompt, 0)
		settleCancel()
		if settlementErr != nil {
			slog.Error("settle_failed", "err", settlementErr, "reservation", res.ID)
		}
		settled = true
		reason, channelID := "", uint(0)
		if decision != nil {
			reason, channelID = decision.Reason, decision.Channel.ID
		}
		s.record(audit.Entry{
			RequestID: reqID, UserID: user.UserID, Model: req.Model,
			Provider: providerName(cands), RouteReason: reason, Status: "error",
			HTTPStatus: status, ErrorMessage: err.Error(), PromptTokens: estPrompt,
			LatencyMS: elapsedMS(start),
			Metadata:  map[string]any{"channel_id": channelID, "phase": "upstream"},
		})
		slog.Error("upstream_failed", "model", req.Model, "err", err, "status", status, "request_id", reqID)
		writeError(w, status, "upstream_error", err.Error())
		return
	}

	usedPrompt, usedCompletion := estPrompt, 0
	if resp.Usage != nil {
		usedPrompt, usedCompletion = resp.Usage.PromptTokens, resp.Usage.CompletionTokens
	}
	settleCtx, settleSpan := tracing.Start(ctx, "ledger.settle")
	set, err := s.ledger.Settle(settleCtx, res.ID, usedPrompt, usedCompletion)
	if set != nil {
		settleSpan.SetAttribute("billing.cost", set.Actual.String())
		settleSpan.SetAttribute("billing.price_version", set.PriceVersion)
	}
	settleSpan.End()
	if err != nil {
		slog.Error("settle_failed", "err", err, "reservation", res.ID, "request_id", reqID)
	}
	settled = true
	s.tracker.Observe(decision.Channel.ID, float64(elapsedMS(start)))

	body, _ := json.Marshal(resp)
	if err := s.cache.Put(ctx, key, body, usedPrompt+usedCompletion); err != nil {
		slog.Warn("cache_put_failed", "err", err)
	}
	s.metrics.Inc("gateway_route_decisions_total", map[string]string{
		"reason": decision.Reason, "model": req.Model,
	})

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Route-Reason", decision.Reason)
	w.Header().Set("X-Channel-ID", strconv.FormatUint(uint64(decision.Channel.ID), 10))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		slog.Warn("write_response_failed", "err", err, "request_id", reqID)
	}

	cost, priceVersion := model.NUMERIC(0), int64(0)
	if set != nil {
		cost, priceVersion = set.Actual, set.PriceVersion
	}
	s.record(audit.Entry{
		RequestID: reqID, UserID: user.UserID, Model: req.Model,
		Provider: decision.Channel.Provider, RouteReason: decision.Reason,
		Status: "success", HTTPStatus: http.StatusOK,
		PromptTokens: usedPrompt, CompletionTokens: usedCompletion,
		Cost: cost, PriceVersion: priceVersion, LatencyMS: elapsedMS(start),
		Body:     rawBody,
		Metadata: map[string]any{"channel_id": decision.Channel.ID},
	})
	s.maybeShadow(reqID, user.UserID, &req, decision, resp, rawBody, elapsedMS(start))
	slog.Info("request_ok", "model", req.Model, "channel", decision.Channel.ID,
		"reason", decision.Reason, "cost", cost.String(),
		"latency_ms", elapsedMS(start), "request_id", reqID)
}

func providerName(cands []router.Candidate) string {
	if len(cands) == 0 {
		return ""
	}
	return cands[0].Channel.Provider
}

func elapsedMS(t time.Time) int { return int(time.Since(t).Milliseconds()) }

// complete performs the upstream call with error-type-aware failover and a
// bounded retry budget (exponential backoff + full jitter).
func (s *server) complete(ctx context.Context, req *model.ChatRequest, cands []router.Candidate) (*router.Decision, *model.ChatResponse, error) {
	attempts := envInt("RETRY_ATTEMPTS", 2)
	var lastDecision *router.Decision
	var lastErr error
	for attempt := 0; attempt <= attempts; attempt++ {
		decision, resp, err := s.completeOnce(ctx, req, cands)
		if err == nil {
			s.metrics.Inc("gateway_channel_requests_total", map[string]string{
				"channel": strconv.FormatUint(uint64(decision.Channel.ID), 10), "status": "success",
			})
			return decision, resp, nil
		}
		lastDecision, lastErr = decision, err
		if decision != nil {
			s.metrics.Inc("gateway_channel_requests_total", map[string]string{
				"channel": strconv.FormatUint(uint64(decision.Channel.ID), 10), "status": "failure",
			})
		}
		if attempt == attempts || !retryableUpstream(err) {
			break
		}
		backoff := time.Duration(100*(1<<attempt)) * time.Millisecond
		jitter := time.Duration(mrand.Int63n(int64(backoff/2) + 1))
		select {
		case <-time.After(backoff + jitter):
		case <-ctx.Done():
			return lastDecision, nil, ctx.Err()
		}
	}
	return lastDecision, nil, lastErr
}

// retryableUpstream reports whether an error deserves another attempt.
func retryableUpstream(err error) bool {
	if err == nil {
		return false
	}
	status, ok := provider.StatusOf(err)
	if !ok {
		return true // transport-level
	}
	return status == http.StatusTooManyRequests || status >= 500
}

func (s *server) completeOnce(ctx context.Context, req *model.ChatRequest, cands []router.Candidate) (*router.Decision, *model.ChatResponse, error) {
	var lastResp *model.ChatResponse
	decision, err := s.router.Failover(ctx, req.Model, cands, router.StrategyWeighted, 0, func(d *router.Decision) (router.Outcome, error) {
		p, ok := s.snapshot().providers[d.Channel.ID]
		if !ok {
			return router.OutcomeServerError, fmt.Errorf("no provider adapter for %s", d.Channel.Provider)
		}
		chatCtx, chatSpan := tracing.Start(ctx, "provider.chat")
		chatSpan.SetAttribute("gen_ai.provider", d.Channel.Provider)
		chatSpan.SetAttribute("gen_ai.request.model", req.Model)
		resp, err := p.Chat(chatCtx, req)
		if err != nil {
			chatSpan.SetAttribute("error", err.Error())
		}
		chatSpan.End()
		if err != nil {
			status := 0
			if code, ok := provider.StatusOf(err); ok {
				status = code
			}
			outcome := router.Classify(status)
			if status == 0 {
				outcome = router.OutcomeServerError
			}
			return outcome, err
		}
		lastResp = resp
		return router.OutcomeSuccess, nil
	})
	if err != nil {
		return decision, nil, err
	}
	return decision, lastResp, nil
}

func (s *server) streamChat(ctx context.Context, w http.ResponseWriter, user *middleware.UserConfig,
	req *model.ChatRequest, cands []router.Candidate, res *billing.Reservation, reqID string, start time.Time, rawBody []byte) {

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "no_flusher", "streaming unsupported by transport")
		return
	}
	rc := http.NewResponseController(w)
	setDeadline := func() {
		// Per-write deadline: a client that stops reading can no longer pin a
		// goroutine forever. 30s is generous for a single SSE frame.
		_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	}

	decision, err := s.router.Select(ctx, req.Model, cands, router.StrategyWeighted)
	if err != nil {
		relCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = s.ledger.Reconcile(relCtx, res.ID)
		cancel()
		writeError(w, http.StatusServiceUnavailable, "no_channel", err.Error())
		return
	}
	s.metrics.Inc("gateway_route_decisions_total", map[string]string{
		"reason": decision.Reason, "model": req.Model,
	})

	p, ok := s.snapshot().providers[decision.Channel.ID]
	if !ok {
		relCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = s.ledger.Reconcile(relCtx, res.ID)
		cancel()
		writeError(w, http.StatusBadGateway, "no_adapter", "no adapter for "+decision.Channel.Provider)
		return
	}

	streamCtx, streamSpan := tracing.Start(ctx, "provider.stream")
	streamSpan.SetAttribute("gen_ai.provider", decision.Channel.Provider)
	streamSpan.SetAttribute("gen_ai.request.model", req.Model)
	defer streamSpan.End()
	chunks, errs, err := p.Stream(streamCtx, req)
	if err != nil {
		s.router.Report(decision.Channel.ID, router.OutcomeServerError)
		relCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = s.ledger.Reconcile(relCtx, res.ID)
		cancel()
		status := http.StatusBadGateway
		if code, ok := provider.StatusOf(err); ok && code >= 400 && code <= 599 {
			status = code
		}
		writeError(w, status, "upstream_error", err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Route-Reason", decision.Reason)
	w.Header().Set("X-Request-Id", reqID)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	var completionTokens int
	writeFailed := false
	for chunk := range chunks {
		if chunk.Usage != nil {
			completionTokens = chunk.Usage.CompletionTokens
		}
		payload, err := json.Marshal(chunk)
		if err != nil {
			continue
		}
		setDeadline()
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			writeFailed = true
			break
		}
		flusher.Flush()
	}
	for err := range errs {
		if err != nil {
			s.failures.Add(1)
			s.metrics.Inc("gateway_upstream_errors_total", map[string]string{"model": req.Model})
			s.router.Report(decision.Channel.ID, router.OutcomeServerError)
			slog.Error("stream_failed", "err", err, "model", req.Model, "request_id", reqID)
			setDeadline()
			_, _ = fmt.Fprintf(w, "data: {\"error\":{\"type\":\"upstream_error\",\"message\":%q}}\n\n", err.Error())
			flusher.Flush()
		}
	}
	if !writeFailed {
		setDeadline()
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}

	settleCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	set, err := s.ledger.Settle(settleCtx, res.ID, promptTokens(req), completionTokens)
	if err != nil {
		slog.Error("settle_failed", "err", err, "reservation", res.ID, "request_id", reqID)
	}
	s.tracker.Observe(decision.Channel.ID, float64(elapsedMS(start)))

	cost, priceVersion := model.NUMERIC(0), int64(0)
	if set != nil {
		cost, priceVersion = set.Actual, set.PriceVersion
	}
	status := "success"
	if writeFailed {
		status = "aborted"
	}
	s.record(audit.Entry{
		RequestID: reqID, UserID: user.UserID, Model: req.Model,
		Provider: decision.Channel.Provider, RouteReason: decision.Reason,
		Status: status, HTTPStatus: http.StatusOK,
		PromptTokens: promptTokens(req), CompletionTokens: completionTokens,
		Cost: cost, PriceVersion: priceVersion, LatencyMS: elapsedMS(start), Body: rawBody,
		Metadata: map[string]any{"channel_id": decision.Channel.ID, "stream": true},
	})
	slog.Info("stream_ok", "model", req.Model, "channel", decision.Channel.ID,
		"reason", decision.Reason, "user", user.UserID,
		"latency_ms", elapsedMS(start), "request_id", reqID)
}

// ---------------------------------------------------------------------------
// P1 endpoints: /v1/models, /v1/embeddings, /v1/rerank
// ---------------------------------------------------------------------------

// handleModels lists models. By default it returns the configured catalogue
// (no upstream round trip); ?refresh=1 asks every capable provider.
func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	seen := make(map[string]bool, len(s.cfg.candidates))
	out := make([]provider.ModelInfo, 0, len(s.cfg.candidates))

	if r.URL.Query().Get("refresh") == "1" {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		for _, p := range s.snapshot().providers {
			lister, ok := p.(provider.ModelLister)
			if !ok {
				continue
			}
			name := p.Name()
			list, err := lister.ListModels(ctx)
			if err != nil {
				slog.Warn("list_models_failed", "provider", name, "err", err)
				continue
			}
			for _, m := range list {
				if seen[m.ID] {
					continue
				}
				seen[m.ID] = true
				if m.OwnedBy == "" {
					m.OwnedBy = name
				}
				if m.Object == "" {
					m.Object = "model"
				}
				if m.Created == 0 {
					m.Created = s.started.Unix()
				}
				out = append(out, m)
			}
		}
	}

	if len(out) == 0 {
		for name, cands := range s.snapshot().candidates {
			if seen[name] {
				continue
			}
			seen[name] = true
			owned := "gateway"
			if len(cands) > 0 {
				owned = cands[0].Channel.Provider
			}
			out = append(out, provider.ModelInfo{
				ID: name, Object: "model", OwnedBy: owned, Created: s.started.Unix(),
			})
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	s.metrics.SetGauge("gateway_models_available", float64(len(out)))
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}

func (s *server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := newRequestID()
	w.Header().Set("X-Request-Id", reqID)

	ctxTrace, span := tracing.Start(r.Context(), "embeddings")
	defer span.End()
	r = r.WithContext(ctxTrace)
	w.Header().Set("X-Trace-Id", span.TraceID)

	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusInternalServerError, "no_user_context", "missing user policy")
		return
	}

	var in provider.EmbeddingRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20))
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body: "+err.Error())
		return
	}
	if in.Model == "" || len(in.Input) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "model and input are required")
		return
	}
	cands := s.snapshot().candidates[in.Model]
	if len(cands) == 0 {
		writeError(w, http.StatusNotFound, "model_not_found", "no channel serves model "+in.Model)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.requestTimeout)
	defer cancel()

	for _, c := range cands {
		p := s.snapshot().providers[c.Channel.ID]
		ep, ok := p.(provider.EmbeddingsProvider)
		if !ok {
			continue
		}
		resp, err := ep.Embeddings(ctx, &in)
		if err != nil {
			slog.Warn("embeddings_failed", "provider", c.Channel.Provider, "err", err, "request_id", reqID)
			continue
		}
		s.tracker.Observe(c.Channel.ID, float64(elapsedMS(start)))
		s.metrics.Inc("gateway_embeddings_total", map[string]string{"model": in.Model})
		tokens := 0
		if resp.Usage != nil {
			tokens = resp.Usage.PromptTokens
		}
		s.record(audit.Entry{
			RequestID: reqID, UserID: user.UserID, Model: in.Model,
			Provider: c.Channel.Provider, RouteReason: "embeddings",
			Status: "success", HTTPStatus: http.StatusOK,
			PromptTokens: tokens, LatencyMS: elapsedMS(start),
			Metadata: map[string]any{"channel_id": c.Channel.ID, "inputs": len(in.Input)},
		})
		writeJSON(w, http.StatusOK, resp)
		return
	}

	s.metrics.Inc("gateway_embeddings_total", map[string]string{"model": in.Model, "status": "unsupported"})
	s.record(audit.Entry{RequestID: reqID, UserID: user.UserID, Model: in.Model,
		Status: "error", HTTPStatus: http.StatusNotImplemented,
		ErrorMessage: "no embeddings-capable provider", LatencyMS: elapsedMS(start)})
	writeError(w, http.StatusNotImplemented, "unsupported", "no embeddings-capable provider for "+in.Model)
}

func (s *server) handleRerank(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := newRequestID()
	w.Header().Set("X-Request-Id", reqID)

	ctxTrace, span := tracing.Start(r.Context(), "rerank")
	defer span.End()
	r = r.WithContext(ctxTrace)
	w.Header().Set("X-Trace-Id", span.TraceID)

	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusInternalServerError, "no_user_context", "missing user policy")
		return
	}

	var in provider.RerankRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20))
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(in.Query) == "" || len(in.Documents) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "query and documents are required")
		return
	}

	// Prefer a provider rerank API when the model is configured with one.
	if in.Model != "" {
		for _, c := range s.cfg.candidates[in.Model] {
			p := s.snapshot().providers[c.Channel.ID]
			rr, ok := p.(provider.Reranker)
			if !ok {
				continue
			}
			ctx, cancel := context.WithTimeout(r.Context(), s.cfg.requestTimeout)
			resp, err := rr.Rerank(ctx, &in)
			cancel()
			if err != nil {
				slog.Warn("rerank_failed", "provider", c.Channel.Provider, "err", err, "request_id", reqID)
				continue
			}
			s.finishRerank(w, user, reqID, in.Model, c.Channel.Provider, resp, start)
			return
		}
	}

	// Local BM25 fallback: deterministic, no external model required.
	scored := s.reranker.Rerank(in.Query, in.Documents, in.TopN)
	results := make([]provider.RerankResult, 0, len(scored))
	for _, d := range scored {
		results = append(results, provider.RerankResult{
			Index: d.Index, RelevanceScore: d.Score, Document: d.Text,
		})
	}
	modelName := in.Model
	if modelName == "" {
		modelName = "local-bm25"
	}
	resp := &provider.RerankResponse{
		Model: modelName, Results: results,
		Usage: map[string]int{"documents": len(in.Documents)},
	}
	s.finishRerank(w, user, reqID, modelName, "local", resp, start)
}

func (s *server) finishRerank(w http.ResponseWriter, user *middleware.UserConfig, reqID, modelName, providerName string,
	resp *provider.RerankResponse, start time.Time) {
	s.metrics.Inc("gateway_rerank_total", map[string]string{"model": modelName})
	s.record(audit.Entry{
		RequestID: reqID, UserID: user.UserID, Model: modelName,
		Provider: providerName, RouteReason: "rerank",
		Status: "success", HTTPStatus: http.StatusOK,
		LatencyMS: elapsedMS(start),
		Metadata:  map[string]any{"documents": len(resp.Results)},
	})
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	var (
		adminSvc *admin.Service
		keyBox   *secretbox.Box
	)

	cfg, err := loadConfig()
	if err != nil {
		slog.Error("config_invalid", "err", err)
		os.Exit(1)
	}

	shutdownTracing, err := tracing.Init(context.Background(), tracing.Config{
		ServiceName:  "super-llm-gateway",
		Version:      version,
		Exporter:     env("OTEL_EXPORTER", "none"),
		OTLPEndpoint: env("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		SampleRatio:  envFloat("OTEL_SAMPLE_RATIO", 1),
	})
	if err != nil {
		slog.Warn("tracing_init_failed", "err", err)
		shutdownTracing = func(context.Context) error { return nil }
	}
	var prices billing.PriceTable = billing.NewStaticPriceTable(1, cfg.prices)
	appCtx, appCancel := context.WithCancel(context.Background())
	defer appCancel()

	var (
		db            *sql.DB
		ledger        *billing.Ledger
		ledgerBackend            = "memory"
		sink          audit.Sink = audit.NopSink{}
	)
	if cfg.dbDSN != "" {
		db, err = sql.Open("pgx", cfg.dbDSN)
		if err != nil {
			slog.Error("db_open_failed", "err", err)
			os.Exit(1)
		}
		db.SetMaxOpenConns(envInt("DB_MAX_OPEN_CONNS", 20))
		db.SetMaxIdleConns(envInt("DB_MAX_IDLE_CONNS", 10))
		db.SetConnMaxLifetime(30 * time.Minute)

		pingCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		err = db.PingContext(pingCtx)
		cancel()
		if err != nil {
			slog.Error("db_unreachable", "err", err)
			os.Exit(1) // fail fast: never silently downgrade billing
		}

		pstore := billing.NewPostgresStore(db)

		// Prices live in price_versions; the env values are only a bootstrap
		// baseline. Operator edits take effect on the refresh ticker.
		priceSeedCtx, priceSeedCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := billing.SeedPrices(priceSeedCtx, db, cfg.prices); err != nil {
			slog.Warn("price_seed_failed", "err", err)
		}
		priceSeedCancel()
		dyn := billing.NewDynamicPriceTable(appCtx, billing.NewPostgresPriceSource(db), cfg.prices)
		dyn.StartRefresh(appCtx, time.Duration(envInt("PRICE_REFRESH_SECONDS", 30))*time.Second)
		prices = dyn

		ledger = billing.NewLedger(prices, billing.WithStore(pstore))

		seedCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		for _, kp := range cfg.keys {
			if err := pstore.EnsureUser(seedCtx, kp.cfg.UserID,
				fmt.Sprintf("gateway-user-%d", kp.cfg.UserID), sha256Hex(kp.key)); err != nil {
				slog.Error("seed_user_failed", "user", kp.cfg.UserID, "err", err)
				os.Exit(1)
			}
			if _, err := ledger.OpenAccount(seedCtx, kp.cfg.UserID, cfg.balance); err != nil {
				slog.Error("open_account_failed", "user", kp.cfg.UserID, "err", err)
				os.Exit(1)
			}
		}
		cancel()
		ledgerBackend = "postgres"
		sink = audit.NewPostgresSink(db, envInt("AUDIT_BUFFER", 8192), envInt("AUDIT_BATCH", 200))

		// Control plane: encrypted upstream keys + JWT console accounts.
		box, berr := secretbox.New(os.Getenv("MASTER_KEY"))
		keyBox = box
		if berr != nil {
			slog.Error("master_key_required", "err", berr, "hint", "openssl rand -hex 32")
			os.Exit(1)
		}
		adminSvc = admin.New(db, box, env("JWT_SECRET", ""))
		seedCtx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		if err := adminSvc.SeedAdmin(seedCtx2, os.Getenv("ADMIN_EMAIL"), os.Getenv("ADMIN_PASSWORD")); err != nil {
			slog.Warn("admin_seed_failed", "err", err)
		}
		cancel2()
	} else {
		ledger = billing.NewLedger(prices)
		for _, kp := range cfg.keys {
			if _, err := ledger.OpenAccount(context.Background(), kp.cfg.UserID, cfg.balance); err != nil {
				slog.Error("open_account_failed", "user", kp.cfg.UserID, "err", err)
				os.Exit(1)
			}
		}
	}

	// --- cache backend: go-redis probe, in-memory fallback that is logged ---
	var store cache.Store
	storeBackend := "memory"
	if cfg.redisDSN != "" {
		addr, password, dbIndex := parseRedisDSN(cfg.redisDSN)
		rs := cache.NewRedisStore(cache.RedisOptions{
			Addr:     addr,
			Password: password,
			DB:       dbIndex,
			PoolSize: envInt("REDIS_POOL_SIZE", 64),
		})
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := rs.Ping(ctx)
		cancel()
		if err != nil {
			slog.Warn("redis_unavailable", "addr", addr, "err", err, "fallback", "memory")
			store = cache.NewMemoryStore(8192)
		} else {
			store = rs
			storeBackend = "redis"
		}
	} else {
		slog.Warn("redis_dsn_empty", "fallback", "memory")
		store = cache.NewMemoryStore(8192)
	}

	resolver := middleware.PolicyResolverFunc(func(ctx context.Context, apiKey string) (*middleware.UserConfig, error) {
		for _, kp := range cfg.keys {
			if subtleEqual(kp.key, apiKey) {
				return kp.cfg, nil
			}
		}
		// Tokens issued from the console (sk-nexus-*) live in PostgreSQL.
		if adminSvc != nil {
			if uid, _, ok := adminSvc.ResolveAPIToken(ctx, apiKey); ok {
				return &middleware.UserConfig{UserID: uid, RateLimit: 600}, nil
			}
		}
		return nil, errors.New("unknown api key")
	})

	limiter := middleware.NewLimiter(middleware.LimiterConfig{
		GlobalLimit: envInt("GLOBAL_RATE_LIMIT", 10000),
		IPLimit:     envInt("IP_RATE_LIMIT", 600),
		ModelLimit:  envInt("MODEL_RATE_LIMIT", 300),
		Window:      time.Minute,
		IPWhitelist: splitCSV(env("IP_WHITELIST", "")),
		TrustProxy:  env("TRUST_PROXY", "false") == "true",
	}, resolver)
	defer limiter.Close()

	metrics := observability.New(map[string]string{
		"version": version, "ledger": ledgerBackend, "cache": storeBackend,
	})

	// Goroutine watchdog: exposes a gauge and logs growth so leaks are visible.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-appCtx.Done():
				return
			case <-ticker.C:
				n := runtime.NumGoroutine()
				metrics.SetGauge("gateway_goroutines", float64(n))
				slog.Info("goroutine_watch", "count", n)
			}
		}
	}()

	// --- admin RBAC store ---
	var adminStore authz.Store
	if db != nil {
		pg := authz.NewPostgresStore(db)
		seedCtx, seedCancel := context.WithTimeout(context.Background(), 10*time.Second)
		for _, ak := range cfg.adminKeys {
			if _, err := db.ExecContext(seedCtx,
				`INSERT INTO tenants (id, name) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`,
				ak.tenantID, fmt.Sprintf("tenant-%d", ak.tenantID)); err != nil {
				slog.Error("seed_tenant_failed", "tenant", ak.tenantID, "err", err)
				os.Exit(1)
			}
			if err := pg.EnsureAdminKey(seedCtx, authz.HashKey(ak.key), ak.tenantID, ak.role); err != nil {
				slog.Error("seed_admin_key_failed", "tenant", ak.tenantID, "err", err)
				os.Exit(1)
			}
			if _, err := ledger.OpenAccount(seedCtx, ak.tenantID, cfg.balance); err != nil {
				slog.Error("open_tenant_account_failed", "tenant", ak.tenantID, "err", err)
				os.Exit(1)
			}
		}
		seedCancel()
		adminStore = pg
	} else {
		mem := authz.NewMemoryStore()
		for _, ak := range cfg.adminKeys {
			_ = mem.EnsureAdminKey(context.Background(), authz.HashKey(ak.key), ak.tenantID, ak.role)
			if _, err := ledger.OpenAccount(context.Background(), ak.tenantID, cfg.balance); err != nil {
				slog.Error("open_tenant_account_failed", "tenant", ak.tenantID, "err", err)
				os.Exit(1)
			}
		}
		adminStore = mem
	}

	// Killer feature #2: real embedding-backed semantic cache. Disabled unless
	// EMBEDDING_API_URL is configured; tenant scoping lives inside the cache.
	var semantic cache.SemanticBackend
	if embURL := strings.TrimSpace(os.Getenv("EMBEDDING_API_URL")); embURL != "" {
		emb := cache.NewHTTPEmbedder(embURL,
			os.Getenv("EMBEDDING_API_KEY"),
			env("EMBEDDING_MODEL", "text-embedding-3-small"),
			envInt("EMBEDDING_DIMENSIONS", 0))
		// Defence in depth: the embedding endpoint is operator-configured, but
		// still goes through the SSRF policy when private ranges are allowed.
		if cfg.policy.AllowPrivateIPs && cfg.outboundClient != nil {
			emb.Client = cfg.outboundClient
		}
		semantic = cache.NewSemantic(store, emb, envFloat("SEMANTIC_THRESHOLD", 0.93))
		slog.Info("semantic_cache_enabled", "embedder", emb.Name(), "threshold", envFloat("SEMANTIC_THRESHOLD", 0.93))
	}

	srv := newServer(cfg, store, limiter, ledger, metrics, sink)
	settingsStore := settings.New(db)
	if db != nil {
		loadSettingsCtx, cancelSettings := context.WithTimeout(context.Background(), 10*time.Second)
		if err := settingsStore.Load(loadSettingsCtx); err != nil {
			slog.Warn("settings_load_failed", "err", err, "note", "using defaults; run migrations/004_settings_shadow.sql")
		}
		cancelSettings()
	}
	srv.settings = settingsStore
	if semantic != nil {
		srv.cache = cache.New(store, cache.Config{
			Semantic: semantic,
			SemanticGate: func() bool {
				return srv.settings.Bool("enable_semantic_cache", true)
			},
		})
	}
	srv.storeBackend = storeBackend
	srv.ledgerBackend = ledgerBackend
	srv.admin = adminStore
	srv.db = db
	srv.box = keyBox
	if adminSvc != nil {
		srv.adminSvc = adminSvc
	}
	if db != nil {
		loadCtx, loadCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := srv.loadCatalogFromDB(loadCtx); err != nil {
			slog.Warn("catalog_load_failed", "err", err)
		}
		loadCancel()
		go func() {
			ticker := time.NewTicker(time.Duration(envInt("CATALOG_REFRESH_SECONDS", 30)) * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-appCtx.Done():
					return
				case <-ticker.C:
					refreshCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					if err := srv.loadCatalogFromDB(refreshCtx); err != nil {
						slog.Warn("catalog_refresh_failed", "err", err)
					}
					cancel()
				}
			}
		}()
	}
	if rs, ok := store.(*cache.RedisStore); ok {
		srv.router = router.NewRouterWithShared(srv.tracker, router.NewRedisBreaker(rs.Client()))
		slog.Info("shared_breaker_enabled", "backend", "redis")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", limiter.Middleware(srv.handleChat))
	mux.HandleFunc("GET /v1/models", limiter.Middleware(srv.handleModels))
	mux.HandleFunc("POST /v1/embeddings", limiter.Middleware(srv.handleEmbeddings))
	mux.HandleFunc("POST /v1/rerank", limiter.Middleware(srv.handleRerank))
	mux.HandleFunc("POST /admin/api/login", srv.adminLogin)
	mux.HandleFunc("GET /admin/api/dashboard", srv.adminAuthV2(authz.ActionOrdersRead, srv.adminDashboard))
	mux.HandleFunc("GET /admin/api/channels", srv.adminAuthV2(authz.ActionOrdersRead, srv.adminChannels))
	mux.HandleFunc("POST /admin/api/channels", srv.adminAuthV2(authz.ActionOrdersWrite, srv.adminChannels))
	mux.HandleFunc("PATCH /admin/api/channels/{id}", srv.adminAuthV2(authz.ActionOrdersWrite, srv.adminChannelByID))
	mux.HandleFunc("DELETE /admin/api/channels/{id}", srv.adminAuthV2(authz.ActionOrdersWrite, srv.adminChannelByID))
	mux.HandleFunc("GET /admin/api/settings", srv.adminAuthV2(authz.ActionOrdersRead, srv.adminSettingsGet))
	mux.HandleFunc("PUT /admin/api/settings", srv.adminAuthV2(authz.ActionOrdersWrite, srv.adminSettingsPut))
	mux.HandleFunc("GET /admin/api/history", srv.adminAuthV2(authz.ActionOrdersRead, srv.adminHistory))
	mux.HandleFunc("GET /admin/api/shadow", srv.adminAuthV2(authz.ActionOrdersRead, srv.adminShadow))
	mux.HandleFunc("POST /admin/api/replay", srv.adminAuthV2(authz.ActionOrdersWrite, srv.adminReplay))
	mux.HandleFunc("GET /admin/api/tokens", srv.adminAuthV2(authz.ActionOrdersRead, srv.adminTokens))
	mux.HandleFunc("POST /admin/api/tokens", srv.adminAuthV2(authz.ActionOrdersWrite, srv.adminTokens))
	mux.HandleFunc("DELETE /admin/api/tokens/{id}", srv.adminAuthV2(authz.ActionOrdersWrite, srv.adminTokenByID))
	mux.HandleFunc("POST /admin/api/orders", adminAuth(adminStore, authz.ActionOrdersWrite, srv.adminCreateOrder))
	mux.HandleFunc("GET /admin/api/orders/{id}", adminAuth(adminStore, authz.ActionOrdersRead, srv.adminGetOrder))
	mux.HandleFunc("POST /admin/api/orders/{id}/pay", adminAuth(adminStore, authz.ActionOrdersWrite, srv.adminPayOrder))
	mux.HandleFunc("POST /admin/api/redeem", adminAuth(adminStore, authz.ActionRedeemCreate, srv.adminCreateRedeem))
	mux.HandleFunc("POST /admin/api/redeem/claim", adminAuth(adminStore, authz.ActionRedeemClaim, srv.adminClaimRedeem))
	adminUI, subErr := fs.Sub(web.AdminFS, "admin")
	if subErr != nil {
		slog.Warn("admin_ui_disabled", "err", subErr)
	} else {
		ui := http.FileServer(http.FS(adminUI))
		mux.Handle("GET /admin/ui/", http.StripPrefix("/admin/ui/", ui))
		mux.HandleFunc("GET /admin/ui", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/admin/ui/", http.StatusMovedPermanently)
		})
		mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/admin/ui/", http.StatusMovedPermanently)
		})
	}
	mux.HandleFunc("GET /health", srv.handleHealth)
	mux.Handle("GET /metrics", metrics.Handler())

	httpServer := &http.Server{
		Addr:              env("BIND_ADDR", "") + ":" + cfg.port,
		Handler:           recoverMiddleware(metrics.Middleware(mux)),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
		// WriteTimeout stays 0: streams set a per-write deadline via
		// http.ResponseController so slow clients cannot pin goroutines.
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("gateway_started", "addr", httpServer.Addr, "version", version,
			"models", len(cfg.candidates), "providers", len(cfg.providers),
			"cache", storeBackend, "ledger", ledgerBackend)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listen_failed", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutdown_started")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown_failed", "err", err)
	}
	cancel()

	if err := sink.Close(shutdownCtx); err != nil {
		slog.Warn("audit_close_failed", "err", err)
	}
	if err := store.Close(); err != nil {
		slog.Warn("store_close_failed", "err", err)
	}
	if db != nil {
		if err := db.Close(); err != nil {
			slog.Warn("db_close_failed", "err", err)
		}
	}
	traceCtx, traceCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := shutdownTracing(traceCtx); err != nil {
		slog.Warn("tracing_shutdown_failed", "err", err)
	}
	traceCancel()
	slog.Info("shutdown_complete")
}

// subtleEqual compares API keys without early exit.
func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// parseRedisDSN accepts redis://[:pass@]host:port[/db] and rediss:// for TLS.
func parseRedisDSN(dsn string) (addr, password string, db int) {
	rest := dsn
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		userinfo := rest[:at]
		rest = rest[at+1:]
		if colon := strings.Index(userinfo, ":"); colon >= 0 {
			password = userinfo[colon+1:]
		}
	}
	if slash := strings.Index(rest, "/"); slash >= 0 {
		if n, err := strconv.Atoi(rest[slash+1:]); err == nil {
			db = n
		}
		rest = rest[:slash]
	}
	if rest == "" {
		rest = "127.0.0.1:6379"
	}
	return rest, password, db
}
