package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djwkvnwincbnak78/nexus/internal/audit"
	"github.com/djwkvnwincbnak78/nexus/internal/authz"
	"github.com/djwkvnwincbnak78/nexus/internal/billing"
	"github.com/djwkvnwincbnak78/nexus/internal/cache"
	"github.com/djwkvnwincbnak78/nexus/internal/middleware"
	"github.com/djwkvnwincbnak78/nexus/internal/model"
	"github.com/djwkvnwincbnak78/nexus/internal/observability"
	"github.com/djwkvnwincbnak78/nexus/internal/provider"
	"github.com/djwkvnwincbnak78/nexus/internal/router"
	"github.com/djwkvnwincbnak78/nexus/web"
)

// ---------------------------------------------------------------------------
// Harness: real handler chain + in-memory ledger + mock upstream
// ---------------------------------------------------------------------------

type testEnv struct {
	srv          *server
	handler      http.Handler
	ledger       *billing.Ledger
	sink         *audit.MemorySink
	limiter      *middleware.Limiter
	upstream     *httptest.Server
	upstreamHits *atomic.Int64
	lastBody     *atomic.Value
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	hits := &atomic.Int64{}
	lastBody := &atomic.Value{}
	lastBody.Store("")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)

		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		switch r.URL.Path {
		case "/embeddings":
			_, _ = io.WriteString(w, `{"object":"list","model":"text-embedding-3-small",
				"data":[{"object":"embedding","index":0,"embedding":[0.1,0.2,0.3]}],
				"usage":{"prompt_tokens":4,"total_tokens":4}}`)
			return
		case "/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"gpt-4o","created":1700000000,"owned_by":"openai"}]}`)
			return
		}

		lastBody.Store(string(raw))
		var body model.ChatRequest
		_ = json.Unmarshal(raw, &body)

		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			f := w.(http.Flusher)
			for _, piece := range []string{"str", "eam"} {
				chunk := model.ChatChunk{
					ID: "chunk-1", Object: "chat.completion.chunk", Model: body.Model,
					Choices: []model.ChunkChoice{{Index: 0, Delta: model.ChunkDelta{Content: piece}}},
				}
				raw, _ := json.Marshal(chunk)
				_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
				f.Flush()
			}
			final, _ := json.Marshal(model.ChatChunk{
				ID: "chunk-1", Object: "chat.completion.chunk", Model: body.Model,
				Usage: &model.Usage{PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15},
			})
			_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", final)
			f.Flush()
			return
		}

		resp := model.ChatResponse{
			ID: "chatcmpl-test", Object: "chat.completion", Model: body.Model,
			Choices: []model.Choice{{
				Index: 0, Message: model.Message{Role: "assistant", Content: "ok"}, FinishReason: "stop",
			}},
			Usage: &model.Usage{PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(upstream.Close)

	price := model.FromFloat(10.0)
	channel := &model.Channel{ID: 1, Name: "openai-test", Provider: "openai", Weight: 1, Models: []string{"gpt-4o"}}
	cfg := &config{
		prices:         map[string]model.NUMERIC{"gpt-4o": price},
		balance:        model.FromFloat(100),
		requestTimeout: 5 * time.Second,
		streamTimeout:  10 * time.Second,
		providers: map[string]model.Provider{
			"openai": provider.NewOpenAI(provider.Config{BaseURL: upstream.URL, APIKey: "test-key"}),
		},
		candidates: map[string][]router.Candidate{
			"gpt-4o": {{
				Model:   &model.ModelChannel{ModelName: "gpt-4o", ChannelID: 1, PricePer1M: price},
				Channel: channel, Healthy: true,
			}},
		},
	}

	keys := []middleware.UserConfig{
		{UserID: 1, RateLimit: 1000},
		{UserID: 2, RateLimit: 1000},
		{UserID: 3, RateLimit: 1},    // for the 429 test
		{UserID: 4, RateLimit: 1000}, // zero balance, for the 402 test
	}
	cfg.keys = []keyPolicy{
		{key: "sk-good", cfg: &keys[0]},
		{key: "sk-other", cfg: &keys[1]},
		{key: "sk-limited", cfg: &keys[2]},
		{key: "sk-broke", cfg: &keys[3]},
	}

	prices := billing.NewStaticPriceTable(1, cfg.prices)
	ledger := billing.NewLedger(prices)
	ctx := context.Background()
	for _, kp := range cfg.keys {
		initial := model.FromFloat(100)
		if kp.cfg.UserID == 4 {
			initial = 0
		}
		if _, err := ledger.OpenAccount(ctx, kp.cfg.UserID, initial); err != nil {
			t.Fatalf("open account: %v", err)
		}
	}

	resolver := middleware.PolicyResolverFunc(func(_ context.Context, key string) (*middleware.UserConfig, error) {
		for _, kp := range cfg.keys {
			if subtleEqual(kp.key, key) {
				return kp.cfg, nil
			}
		}
		return nil, fmt.Errorf("unknown api key")
	})

	limiter := middleware.NewLimiter(middleware.LimiterConfig{
		GlobalLimit: 10000, IPLimit: 10000, ModelLimit: 10000, Window: time.Minute,
	}, resolver)
	t.Cleanup(limiter.Close)

	metrics := observability.New(map[string]string{"version": version})
	sink := &audit.MemorySink{}
	adminStore := authz.NewMemoryStore()
	_ = adminStore.EnsureAdminKey(ctx, authz.HashKey("adm-key"), 1, authz.RoleAdmin)
	_ = adminStore.EnsureAdminKey(ctx, authz.HashKey("view-key"), 1, authz.RoleViewer)

	srv := newServer(cfg, cache.NewMemoryStore(64), limiter, ledger, metrics, sink)
	srv.storeBackend = "memory"
	srv.ledgerBackend = "memory"
	srv.admin = adminStore

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", limiter.Middleware(srv.handleChat))
	mux.HandleFunc("GET /v1/models", limiter.Middleware(srv.handleModels))
	mux.HandleFunc("POST /v1/embeddings", limiter.Middleware(srv.handleEmbeddings))
	mux.HandleFunc("POST /v1/rerank", limiter.Middleware(srv.handleRerank))
	mux.HandleFunc("POST /admin/api/orders", adminAuth(adminStore, authz.ActionOrdersWrite, srv.adminCreateOrder))
	mux.HandleFunc("GET /admin/api/orders/{id}", adminAuth(adminStore, authz.ActionOrdersRead, srv.adminGetOrder))
	mux.HandleFunc("POST /admin/api/orders/{id}/pay", adminAuth(adminStore, authz.ActionOrdersWrite, srv.adminPayOrder))
	mux.HandleFunc("POST /admin/api/redeem", adminAuth(adminStore, authz.ActionRedeemCreate, srv.adminCreateRedeem))
	mux.HandleFunc("POST /admin/api/redeem/claim", adminAuth(adminStore, authz.ActionRedeemClaim, srv.adminClaimRedeem))
	adminUI, _ := fs.Sub(web.AdminFS, "admin")
	mux.Handle("GET /admin/ui/", http.StripPrefix("/admin/ui/", http.FileServer(http.FS(adminUI))))
	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/ui/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /health", srv.handleHealth)
	mux.Handle("GET /metrics", metrics.Handler())

	return &testEnv{
		srv:          srv,
		handler:      metrics.Middleware(mux),
		ledger:       ledger,
		sink:         sink,
		limiter:      limiter,
		upstream:     upstream,
		upstreamHits: hits,
		lastBody:     lastBody,
	}
}

func (e *testEnv) doAdmin(t *testing.T, method, path, adminKey, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if adminKey != "" {
		r.Header.Set("X-Admin-Key", adminKey)
	}
	r.RemoteAddr = "203.0.113.9:4242"
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, r)
	return rec
}

func (e *testEnv) do(t *testing.T, method, path, apiKey string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if apiKey != "" {
		r.Header.Set("Authorization", "Bearer "+apiKey)
	}
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = "203.0.113.9:4242"
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, r)
	return rec
}

func chatBody(modelName, content string, temperature float64, stream bool) string {
	payload := map[string]any{
		"model": modelName,
		"messages": []map[string]string{
			{"role": "user", "content": content},
		},
		"temperature": temperature,
	}
	if stream {
		payload["stream"] = true
	}
	raw, _ := json.Marshal(payload)
	return string(raw)
}

// ---------------------------------------------------------------------------
// Core paths
// ---------------------------------------------------------------------------

func TestAuthRejectsMissingAndUnknownKey(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(t, http.MethodPost, "/v1/chat/completions", "", chatBody("gpt-4o", "hi", 0, false))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing key status=%d want 401", rec.Code)
	}

	rec = env.do(t, http.MethodPost, "/v1/chat/completions", "sk-does-not-exist", chatBody("gpt-4o", "hi", 0, false))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown key status=%d want 401", rec.Code)
	}
	if env.upstreamHits.Load() != 0 {
		t.Fatal("unauthenticated request reached upstream")
	}
}

func TestRateLimitReturns429(t *testing.T) {
	env := newTestEnv(t)

	if rec := env.do(t, http.MethodPost, "/v1/chat/completions", "sk-limited",
		chatBody("gpt-4o", "first prompt", 0, false)); rec.Code != http.StatusOK {
		t.Fatalf("first request status=%d want 200", rec.Code)
	}

	rec := env.do(t, http.MethodPost, "/v1/chat/completions", "sk-limited",
		chatBody("gpt-4o", "second prompt", 0, false))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status=%d want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 must carry Retry-After")
	}
	if rec.Header().Get("X-RateLimit-Scope") != "user" {
		t.Fatalf("scope=%q want user", rec.Header().Get("X-RateLimit-Scope"))
	}
}

func TestNonStreamingSuccessChargesAndAudits(t *testing.T) {
	env := newTestEnv(t)

	before, err := env.ledger.Balance(context.Background(), 1)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}

	rec := env.do(t, http.MethodPost, "/v1/chat/completions", "sk-good",
		chatBody("gpt-4o", "hello gateway", 0.2, false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Route-Reason") == "" {
		t.Fatal("missing X-Route-Reason")
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Fatal("missing X-Request-Id")
	}

	var body model.ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Choices) == 0 || body.Choices[0].Message.Content != "ok" {
		t.Fatalf("unexpected response: %s", rec.Body.String())
	}

	after, _ := env.ledger.Balance(context.Background(), 1)
	wantCost := model.FromFloat(0.00015) // 12 prompt + 3 completion tokens at 10 per 1M
	if after.Spent != wantCost {
		t.Fatalf("spent=%s want %s", after.Spent.String(), wantCost.String())
	}
	if after.Balance != before.Balance-wantCost {
		t.Fatalf("balance=%s want %s", after.Balance.String(), (before.Balance - wantCost).String())
	}
	if after.Reserved != 0 {
		t.Fatalf("reservation leak: reserved=%s", after.Reserved.String())
	}

	if len(env.sink.Entries) == 0 {
		t.Fatal("no audit entry written")
	}
	last := env.sink.Entries[len(env.sink.Entries)-1]
	if last.Status != "success" || last.HTTPStatus != 200 || last.PriceVersion != 1 {
		t.Fatalf("audit entry=%+v", last)
	}
	if last.Cost != wantCost {
		t.Fatalf("audit cost=%s want %s", last.Cost.String(), wantCost.String())
	}
	if last.RequestID == "" || last.Model != "gpt-4o" || last.Provider != "openai" {
		t.Fatalf("audit identity fields missing: %+v", last)
	}
}

func TestCacheHitSkipsUpstreamAndBilling(t *testing.T) {
	env := newTestEnv(t)
	body := chatBody("gpt-4o", "cache me", 0.2, false)

	if rec := env.do(t, http.MethodPost, "/v1/chat/completions", "sk-good", body); rec.Code != http.StatusOK {
		t.Fatalf("first status=%d", rec.Code)
	}
	first, _ := env.ledger.Balance(context.Background(), 1)

	rec := env.do(t, http.MethodPost, "/v1/chat/completions", "sk-good", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("second status=%d", rec.Code)
	}
	if got := rec.Header().Get("X-Cache"); got != "exact" {
		t.Fatalf("X-Cache=%q want exact", got)
	}
	if env.upstreamHits.Load() != 1 {
		t.Fatalf("upstream hits=%d want 1", env.upstreamHits.Load())
	}
	second, _ := env.ledger.Balance(context.Background(), 1)
	if second.Balance != first.Balance || second.Spent != first.Spent {
		t.Fatal("cache hit must not charge the account")
	}

	last := env.sink.Entries[len(env.sink.Entries)-1]
	if !last.Cached || last.Status != "cache_hit" {
		t.Fatalf("audit entry=%+v", last)
	}
}

func TestCacheIsParameterAndTenantScoped(t *testing.T) {
	env := newTestEnv(t)
	warm := chatBody("gpt-4o", "shared prompt", 0.2, false)
	if rec := env.do(t, http.MethodPost, "/v1/chat/completions", "sk-good", warm); rec.Code != http.StatusOK {
		t.Fatalf("warm status=%d", rec.Code)
	}

	// Different temperature: must miss.
	hot := chatBody("gpt-4o", "shared prompt", 0.9, false)
	if rec := env.do(t, http.MethodPost, "/v1/chat/completions", "sk-good", hot); rec.Header().Get("X-Cache") != "" {
		t.Fatal("temperature change must not hit the cache")
	}

	// Different tenant: must miss.
	if rec := env.do(t, http.MethodPost, "/v1/chat/completions", "sk-other", warm); rec.Header().Get("X-Cache") != "" {
		t.Fatal("cross-tenant cache hit")
	}

	if env.upstreamHits.Load() != 3 {
		t.Fatalf("upstream hits=%d want 3", env.upstreamHits.Load())
	}
}

func TestStreamingRelaysSSEAndSettles(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(t, http.MethodPost, "/v1/chat/completions", "sk-good",
		chatBody("gpt-4o", "stream please", 0.2, true))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type=%q", ct)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"content":"str"`) || !strings.Contains(out, `"content":"eam"`) {
		t.Fatalf("stream content missing: %s", out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
		t.Fatalf("stream not terminated: %s", out)
	}

	acct, _ := env.ledger.Balance(context.Background(), 1)
	if acct.Reserved != 0 {
		t.Fatalf("stream reservation leaked: %s", acct.Reserved.String())
	}
	if acct.Spent == 0 {
		t.Fatal("stream was not charged")
	}

	last := env.sink.Entries[len(env.sink.Entries)-1]
	if last.Status != "success" || last.CompletionTokens != 3 || last.Metadata["stream"] != true {
		t.Fatalf("audit entry=%+v", last)
	}
}

func TestBadRequestsAndBillingGuards(t *testing.T) {
	env := newTestEnv(t)

	if rec := env.do(t, http.MethodPost, "/v1/chat/completions", "sk-good", "{not json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body status=%d want 400", rec.Code)
	}
	if rec := env.do(t, http.MethodPost, "/v1/chat/completions", "sk-good",
		`{"model":"gpt-4o","messages":[]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty messages status=%d want 400", rec.Code)
	}
	if rec := env.do(t, http.MethodPost, "/v1/chat/completions", "sk-good",
		chatBody("unknown-model", "hi", 0, false)); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown model status=%d want 404", rec.Code)
	}
	if rec := env.do(t, http.MethodPost, "/v1/chat/completions", "sk-broke",
		chatBody("gpt-4o", "no money", 0, false)); rec.Code != http.StatusPaymentRequired {
		t.Fatalf("broke user status=%d want 402", rec.Code)
	}
	if env.upstreamHits.Load() != 0 {
		t.Fatal("rejected requests must not reach upstream")
	}
}

func TestHealthAndMetricsEndpoints(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(t, http.MethodGet, "/health", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("health status=%d", rec.Code)
	}
	var health map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil {
		t.Fatalf("health decode: %v", err)
	}
	if health["status"] != "ok" || health["semantic_cache"] != false {
		t.Fatalf("health=%v", health)
	}

	rec = env.do(t, http.MethodGet, "/metrics", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "gateway_http_requests_total") {
		t.Fatalf("metrics body missing counters: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Config / helper coverage
// ---------------------------------------------------------------------------

func TestParseKeys(t *testing.T) {
	keys, err := parseKeys("sk-a:1:60,sk-b:2:120:gpt-4o|claude-3")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(keys) != 2 || keys[0].cfg.UserID != 1 || keys[0].cfg.RateLimit != 60 {
		t.Fatalf("keys=%+v", keys)
	}
	if len(keys[1].cfg.ModelWhitelist) != 2 || keys[1].cfg.ModelWhitelist[1] != "claude-3" {
		t.Fatalf("whitelist=%v", keys[1].cfg.ModelWhitelist)
	}

	for _, bad := range []string{"", "sk-a", "sk-a:x:1", "sk-a:1:-2"} {
		if _, err := parseKeys(bad); err == nil {
			t.Fatalf("parseKeys(%q) should fail", bad)
		}
	}
}

func TestLoadConfigOpenAIAndAnthropic(t *testing.T) {
	t.Setenv("GATEWAY_API_KEYS", "sk-a:1:60:gpt-4o")
	t.Setenv("OPENAI_API_KEY", "oa-key")
	t.Setenv("OPENAI_MODELS", "gpt-4o,gpt-4o-mini")
	t.Setenv("ANTHROPIC_API_KEY", "an-key")
	t.Setenv("ANTHROPIC_MODELS", "claude-3-5-sonnet")
	t.Setenv("PORT", "9999")
	t.Setenv("INITIAL_BALANCE", "42")
	t.Setenv("REQUEST_TIMEOUT_SECONDS", "7")
	t.Setenv("STREAM_TIMEOUT_SECONDS", "11")
	t.Setenv("OUTBOUND_ALLOW_PRIVATE", "true")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.port != "9999" {
		t.Fatalf("port=%s", cfg.port)
	}
	if len(cfg.providers) != 2 || len(cfg.candidates) != 3 {
		t.Fatalf("providers=%d candidates=%d", len(cfg.providers), len(cfg.candidates))
	}
	if cfg.requestTimeout != 7*time.Second || cfg.streamTimeout != 11*time.Second {
		t.Fatalf("timeouts=%v/%v", cfg.requestTimeout, cfg.streamTimeout)
	}
	if !cfg.policy.AllowPrivateIPs {
		t.Fatal("OUTBOUND_ALLOW_PRIVATE not applied")
	}
	if cfg.balance != model.FromFloat(42) {
		t.Fatalf("balance=%s", cfg.balance.String())
	}
	if got := cfg.keys[0].cfg.ModelWhitelist; len(got) != 1 || got[0] != "gpt-4o" {
		t.Fatalf("whitelist=%v", got)
	}

	// Structural errors.
	t.Setenv("GATEWAY_API_KEYS", "")
	if _, err := loadConfig(); err == nil {
		t.Fatal("missing keys must fail")
	}
	t.Setenv("GATEWAY_API_KEYS", "sk-a:1:60")
	t.Setenv("DB_DSN", "mysql://nope")
	if _, err := loadConfig(); err == nil {
		t.Fatal("non-postgres DSN must fail")
	}
	t.Setenv("DB_DSN", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	if _, err := loadConfig(); err == nil {
		t.Fatal("no provider keys must fail")
	}
}

func TestParseRedisDSN(t *testing.T) {
	cases := []struct {
		in       string
		addr     string
		password string
		db       int
	}{
		{"redis://127.0.0.1:6379/3", "127.0.0.1:6379", "", 3},
		{"redis://:secret@redis:6380/0", "redis:6380", "secret", 0},
		{"rediss://user:pass@host:6381/2", "host:6381", "pass", 2},
		{"redis://", "127.0.0.1:6379", "", 0},
	}
	for _, tc := range cases {
		addr, pw, db := parseRedisDSN(tc.in)
		if addr != tc.addr || pw != tc.password || db != tc.db {
			t.Fatalf("parseRedisDSN(%q)=(%q,%q,%d) want (%q,%q,%d)",
				tc.in, addr, pw, db, tc.addr, tc.password, tc.db)
		}
	}
}

func TestSubtleEqual(t *testing.T) {
	if !subtleEqual("abc", "abc") {
		t.Fatal("equal strings must match")
	}
	if subtleEqual("abc", "abd") || subtleEqual("abc", "ab") {
		t.Fatal("different strings must not match")
	}
}

func TestHelpers(t *testing.T) {
	req := &model.ChatRequest{
		Messages: []model.Message{
			{Role: "system", Content: "be terse"},
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi"},
		},
	}
	if got := systemPrompt(req); !strings.Contains(got, "be terse") || strings.Contains(got, "hello") {
		t.Fatalf("systemPrompt=%q", got)
	}
	if got := promptText(req); !strings.Contains(got, "user:hello") || strings.Contains(got, "be terse") {
		t.Fatalf("promptText=%q", got)
	}
	if promptTokens(req) <= 0 {
		t.Fatal("promptTokens must be positive")
	}
	if promptTokens(&model.ChatRequest{}) == 0 {
		t.Fatal("empty request must still reserve at least one token")
	}
	if estimateTokens("") != 1 {
		t.Fatalf("estimateTokens(empty)=%d", estimateTokens(""))
	}
	if newRequestID() == newRequestID() {
		t.Fatal("request IDs must be unique")
	}
	if providerName(nil) != "" || providerName([]router.Candidate{{Channel: &model.Channel{Provider: "openai"}}}) != "openai" {
		t.Fatal("providerName wrong")
	}
	if cfg := cacheKey(7, req); cfg.TenantID != "7" || cfg.Model != "" || cfg.System == "" {
		t.Fatalf("cacheKey=%+v", cfg)
	}
}

func TestModelsEndpoint(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(t, http.MethodGet, "/v1/models", "sk-good", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Object != "list" || len(payload.Data) != 1 || payload.Data[0].ID != "gpt-4o" {
		t.Fatalf("payload=%+v", payload)
	}
	if payload.Data[0].OwnedBy != "openai" {
		t.Fatalf("owned_by=%q", payload.Data[0].OwnedBy)
	}

	if rec := env.do(t, http.MethodGet, "/v1/models", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated models status=%d want 401", rec.Code)
	}
}

func TestModelsRefreshQueriesUpstream(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(t, http.MethodGet, "/v1/models?refresh=1", "sk-good", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if env.upstreamHits.Load() == 0 {
		t.Fatal("refresh=1 must call the upstream model list")
	}
	if !strings.Contains(rec.Body.String(), "gpt-4o") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestEmbeddingsEndpoint(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(t, http.MethodPost, "/v1/embeddings", "sk-good",
		`{"model":"gpt-4o","input":["hello"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(payload.Data) != 1 || len(payload.Data[0].Embedding) != 3 {
		t.Fatalf("payload=%+v", payload)
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Fatal("missing request id")
	}
	last := env.sink.Entries[len(env.sink.Entries)-1]
	if last.RouteReason != "embeddings" || last.Status != "success" {
		t.Fatalf("audit=%+v", last)
	}

	// Unknown model → 404 without touching the upstream.
	before := env.upstreamHits.Load()
	rec = env.do(t, http.MethodPost, "/v1/embeddings", "sk-good", `{"model":"nope","input":["x"]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown model status=%d want 404", rec.Code)
	}
	if env.upstreamHits.Load() != before {
		t.Fatal("unknown model must not reach upstream")
	}

	// Empty input → 400.
	if rec := env.do(t, http.MethodPost, "/v1/embeddings", "sk-good", `{"model":"gpt-4o","input":[]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty input status=%d want 400", rec.Code)
	}
}

func TestRerankLocalBM25(t *testing.T) {
	env := newTestEnv(t)

	body := `{"query":"apple pie","documents":["quantum physics","apple pie recipe","apple orchard"],"top_n":2}`
	rec := env.do(t, http.MethodPost, "/v1/rerank", "sk-good", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Model   string `json:"model"`
		Results []struct {
			Index int     `json:"index"`
			Score float64 `json:"relevance_score"`
			Doc   string  `json:"document"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Model != "local-bm25" {
		t.Fatalf("model=%q", payload.Model)
	}
	if len(payload.Results) != 2 {
		t.Fatalf("top_n ignored: %+v", payload.Results)
	}
	if payload.Results[0].Index != 1 {
		t.Fatalf("top result=%+v want document index 1", payload.Results[0])
	}
	if payload.Results[0].Score <= payload.Results[1].Score {
		t.Fatalf("scores not descending: %+v", payload.Results)
	}

	last := env.sink.Entries[len(env.sink.Entries)-1]
	if last.RouteReason != "rerank" {
		t.Fatalf("audit=%+v", last)
	}

	if rec := env.do(t, http.MethodPost, "/v1/rerank", "sk-good", `{"query":"","documents":["a"]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty query status=%d want 400", rec.Code)
	}
}

func TestToolsAreForwardedToUpstream(t *testing.T) {
	env := newTestEnv(t)
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"weather?"}],
		"tools":[{"type":"function","function":{"name":"get_weather","description":"lookup",
		"parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],
		"tool_choice":{"type":"auto"}}`

	rec := env.do(t, http.MethodPost, "/v1/chat/completions", "sk-good", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	sent, _ := env.lastBody.Load().(string)
	if !strings.Contains(sent, `"tools"`) || !strings.Contains(sent, "get_weather") {
		t.Fatalf("tools dropped in translation: %s", sent)
	}
	if !strings.Contains(sent, `"tool_choice"`) || !strings.Contains(sent, `"auto"`) {
		t.Fatalf("tool_choice dropped in translation: %s", sent)
	}
}

// stubProvider implements only model.Provider: it is used to exercise the
// "capability not supported" branches of embeddings/rerank.
type stubProvider struct{ rerank bool }

func (stubProvider) Name() string { return "stub" }
func (stubProvider) Chat(context.Context, *model.ChatRequest) (*model.ChatResponse, error) {
	return nil, fmt.Errorf("stub: chat unsupported")
}
func (stubProvider) Stream(context.Context, *model.ChatRequest) (<-chan *model.ChatChunk, <-chan error, error) {
	return nil, nil, fmt.Errorf("stub: stream unsupported")
}

type stubReranker struct{ stubProvider }

func (s stubReranker) Rerank(context.Context, *provider.RerankRequest) (*provider.RerankResponse, error) {
	return &provider.RerankResponse{
		Model:   "stub-rerank",
		Results: []provider.RerankResult{{Index: 0, RelevanceScore: 0.9, Document: "x"}},
	}, nil
}

func TestEmbeddingsUnsupportedProvider(t *testing.T) {
	env := newTestEnv(t)
	env.srv.cfg.candidates["no-embed"] = []router.Candidate{{
		Channel: &model.Channel{ID: 9, Provider: "stub"},
		Model:   &model.ModelChannel{ModelName: "no-embed", ChannelID: 9},
	}}
	env.srv.cfg.providers["stub"] = stubProvider{}

	rec := env.do(t, http.MethodPost, "/v1/embeddings", "sk-good", `{"model":"no-embed","input":["x"]}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status=%d want 501", rec.Code)
	}
}

func TestRerankUsesProviderWhenAvailable(t *testing.T) {
	env := newTestEnv(t)
	env.srv.cfg.candidates["stub-rerank"] = []router.Candidate{{
		Channel: &model.Channel{ID: 10, Provider: "stub"},
		Model:   &model.ModelChannel{ModelName: "stub-rerank", ChannelID: 10},
	}}
	env.srv.cfg.providers["stub"] = stubReranker{}
	// The running catalog keys adapters by channel ID; register the stub there
	// too so the handler exercises the provider path (not the BM25 fallback).
	env.srv.snapshot().providers[10] = stubReranker{}

	rec := env.do(t, http.MethodPost, "/v1/rerank", "sk-good",
		`{"model":"stub-rerank","query":"q","documents":["a","b"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "stub-rerank") {
		t.Fatalf("body=%s", rec.Body.String())
	}
	last := env.sink.Entries[len(env.sink.Entries)-1]
	if last.Provider != "stub" || last.RouteReason != "rerank" {
		t.Fatalf("audit=%+v", last)
	}
}

func TestAdminAuthRequiresKeyAndRole(t *testing.T) {
	env := newTestEnv(t)

	if rec := env.doAdmin(t, http.MethodPost, "/admin/api/orders", "", `{"amount":"5"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing key status=%d want 401", rec.Code)
	}
	if rec := env.doAdmin(t, http.MethodPost, "/admin/api/orders", "wrong", `{"amount":"5"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad key status=%d want 401", rec.Code)
	}
	if rec := env.doAdmin(t, http.MethodPost, "/admin/api/orders", "view-key", `{"amount":"5"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer write status=%d want 403", rec.Code)
	}
	// Viewers may read (404 proves the request passed authorization).
	if rec := env.doAdmin(t, http.MethodGet, "/admin/api/orders/ord_missing", "view-key", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("viewer read status=%d want 404", rec.Code)
	}
}

func TestAdminOrderLifecycleCreditsLedger(t *testing.T) {
	env := newTestEnv(t)
	before, _ := env.ledger.Balance(context.Background(), 1)

	rec := env.doAdmin(t, http.MethodPost, "/admin/api/orders", "adm-key", `{"amount":"12.5","currency":"usd"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var order struct {
		ID       string `json:"id"`
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
		Status   string `json:"status"`
	}
	// model.NUMERIC marshals as a JSON number; unmarshal generically first.
	var raw map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	order.ID, _ = raw["id"].(string)
	order.Currency, _ = raw["currency"].(string)
	order.Status, _ = raw["status"].(string)
	if order.ID == "" || order.Currency != "USD" || order.Status != "pending" {
		t.Fatalf("order=%v", raw)
	}

	if rec := env.doAdmin(t, http.MethodGet, "/admin/api/orders/"+order.ID, "view-key", ""); rec.Code != http.StatusOK {
		t.Fatalf("get status=%d", rec.Code)
	}
	if rec := env.doAdmin(t, http.MethodPost, "/admin/api/orders/"+order.ID+"/pay", "adm-key", ""); rec.Code != http.StatusOK {
		t.Fatalf("pay status=%d body=%s", rec.Code, rec.Body.String())
	}

	after, _ := env.ledger.Balance(context.Background(), 1)
	want := model.FromFloat(12.5)
	if after.Balance-before.Balance != want {
		t.Fatalf("credit delta=%s want %s", (after.Balance - before.Balance).String(), want.String())
	}

	if rec := env.doAdmin(t, http.MethodPost, "/admin/api/orders/"+order.ID+"/pay", "adm-key", ""); rec.Code != http.StatusConflict {
		t.Fatalf("double pay status=%d want 409", rec.Code)
	}
	if rec := env.doAdmin(t, http.MethodPost, "/admin/api/orders", "adm-key", `{"amount":"-1"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative amount status=%d want 400", rec.Code)
	}
}

func TestAdminRedeemFlow(t *testing.T) {
	env := newTestEnv(t)
	before, _ := env.ledger.Balance(context.Background(), 1)

	rec := env.doAdmin(t, http.MethodPost, "/admin/api/redeem", "adm-key", `{"amount":"3.25","count":2}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create codes status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Data []struct {
			Code string `json:"code"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(payload.Data) != 2 || payload.Data[0].Code == "" {
		t.Fatalf("codes=%+v", payload.Data)
	}

	code := payload.Data[0].Code
	if rec := env.doAdmin(t, http.MethodPost, "/admin/api/redeem/claim", "adm-key", `{"code":"`+code+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("claim status=%d body=%s", rec.Code, rec.Body.String())
	}
	after, _ := env.ledger.Balance(context.Background(), 1)
	want := model.FromFloat(3.25)
	if after.Balance-before.Balance != want {
		t.Fatalf("credit delta=%s want %s", (after.Balance - before.Balance).String(), want.String())
	}

	if rec := env.doAdmin(t, http.MethodPost, "/admin/api/redeem/claim", "adm-key", `{"code":"`+code+`"}`); rec.Code != http.StatusConflict {
		t.Fatalf("double claim status=%d want 409", rec.Code)
	}
	if rec := env.doAdmin(t, http.MethodPost, "/admin/api/redeem/claim", "adm-key", `{"code":"rc_missing"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown code status=%d want 404", rec.Code)
	}
}

func TestParseAdminKeys(t *testing.T) {
	keys, err := parseAdminKeys("k1:1:admin,k2:2:viewer")
	if err != nil || len(keys) != 2 || keys[0].role != authz.RoleAdmin || keys[1].tenantID != 2 {
		t.Fatalf("keys=%+v err=%v", keys, err)
	}
	for _, bad := range []string{"k1:1", "k1:x:admin", "k1:1:god"} {
		if _, err := parseAdminKeys(bad); err == nil {
			t.Fatalf("%q should fail", bad)
		}
	}
}

func TestAdminUIIsServed(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(t, http.MethodGet, "/admin/ui/", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin ui status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Nexus Console") {
		t.Fatalf("admin ui body missing title: %s", rec.Body.String()[:120])
	}

	rec = env.do(t, http.MethodGet, "/admin", "", "")
	if rec.Code != http.StatusMovedPermanently && rec.Code != http.StatusFound {
		t.Fatalf("/admin status=%d want redirect", rec.Code)
	}
}

func TestPromptCompressionSavesTokensAndRewritesUpstream(t *testing.T) {
	env := newTestEnv(t)
	env.srv.cfg.compress = true

	verbose := "Hello     world ,   this   is   verbose !!!!!"
	rec := env.do(t, http.MethodPost, "/v1/chat/completions", "sk-good",
		chatBody("gpt-4o", verbose, 0.2, false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if saved := rec.Header().Get("X-Tokens-Saved"); saved == "" || saved == "0" {
		t.Fatalf("X-Tokens-Saved=%q", saved)
	}
	sent, _ := env.lastBody.Load().(string)
	if !strings.Contains(sent, `"content":"Hello world, this is verbose!"`) {
		t.Fatalf("upstream prompt not compressed: %s", sent)
	}
}
