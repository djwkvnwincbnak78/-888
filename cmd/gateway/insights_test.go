package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/djwkvnwincbnak78/nexus/internal/audit"
	"github.com/djwkvnwincbnak78/nexus/internal/middleware"
	"github.com/djwkvnwincbnak78/nexus/internal/model"
	"github.com/djwkvnwincbnak78/nexus/internal/observability"
	"github.com/djwkvnwincbnak78/nexus/internal/router"
	"github.com/djwkvnwincbnak78/nexus/internal/settings"
)

type fakeProvider struct {
	reply string
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Chat(_ context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
	return &model.ChatResponse{
		ID: "fake-1", Object: "chat.completion", Model: req.Model,
		Choices: []model.Choice{{Index: 0, Message: model.Message{Role: "assistant", Content: f.reply}}},
		Usage:   &model.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10},
	}, nil
}

func (f *fakeProvider) Stream(context.Context, *model.ChatRequest) (<-chan *model.ChatChunk, <-chan error, error) {
	return nil, nil, errors.New("not implemented")
}

func testServer(t *testing.T) *server {
	t.Helper()
	mk := func(name string, channelID uint, price float64) ([]router.Candidate, string) {
		ch := &model.Channel{ID: channelID, Name: name, Provider: "fake", Models: []string{name}, Weight: 1}
		cands := []router.Candidate{{
			Model:   &model.ModelChannel{ModelName: name, ChannelID: channelID, PricePer1M: model.FromFloat(price)},
			Channel: ch, Healthy: true,
		}}
		return cands, name
	}
	mainCands, _ := mk("main-model", 1, 10)
	shadowCands, _ := mk("shadow-model", 2, 2)
	cfg := &config{
		candidates:     map[string][]router.Candidate{"main-model": mainCands, "shadow-model": shadowCands},
		providers:      map[string]model.Provider{"fake": &fakeProvider{reply: "pong"}},
		requestTimeout: 5 * time.Second, streamTimeout: 5 * time.Second,
	}
	srv := newServer(cfg, nil, nil, nil, observability.New(map[string]string{"version": "test"}), audit.NopSink{})
	srv.settings = settings.New(nil)
	return srv
}

func TestMaybeShadowWritesComparisonWithoutBlocking(t *testing.T) {
	srv := testServer(t)
	got := make(chan shadowRecord, 1)
	srv.shadowSink = func(r shadowRecord) error { got <- r; return nil }
	if err := srv.settings.Update(context.Background(), map[string]string{
		"enable_shadow_test": "true", "shadow_model": "shadow-model", "shadow_sample_rate": "100",
	}); err != nil {
		t.Fatal(err)
	}

	req := &model.ChatRequest{Model: "main-model", Messages: []model.Message{{Role: "user", Content: "ping"}}}
	decision := &router.Decision{
		Channel: &model.Channel{ID: 1}, Model: &model.ModelChannel{PricePer1M: model.FromFloat(10)},
	}
	mainResp := &model.ChatResponse{Usage: &model.Usage{PromptTokens: 10, CompletionTokens: 5}}

	started := time.Now()
	srv.maybeShadow("trace-1", 42, req, decision, mainResp, []byte(`{"model":"main-model"}`), 12)
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("maybeShadow blocked for %v", elapsed)
	}

	select {
	case rec := <-got:
		if rec.MainModel != "main-model" || rec.ShadowModel != "shadow-model" {
			t.Fatalf("models = %s / %s", rec.MainModel, rec.ShadowModel)
		}
		if rec.ShadowTokens != 10 {
			t.Fatalf("shadow tokens = %d, want 10", rec.ShadowTokens)
		}
		if rec.Status != "ok" {
			t.Fatalf("status = %s (%s)", rec.Status, rec.ErrorMessage)
		}
		if rec.TraceID != "trace-1" || rec.MainLatency != 12 {
			t.Fatalf("trace/latency mismatch: %+v", rec)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shadow goroutine did not persist a record")
	}
}

func TestMaybeShadowDisabledIsNoop(t *testing.T) {
	srv := testServer(t)
	srv.shadowSink = func(shadowRecord) error {
		t.Fatal("sink must not be called when shadow testing is disabled")
		return nil
	}
	req := &model.ChatRequest{Model: "main-model", Messages: []model.Message{{Role: "user", Content: "x"}}}
	srv.maybeShadow("t", 1, req, &router.Decision{Channel: &model.Channel{ID: 1}, Model: &model.ModelChannel{}}, nil, nil, 1)
	time.Sleep(50 * time.Millisecond)
}

func TestReplayReturnsCompletion(t *testing.T) {
	srv := testServer(t)
	body := `{"model":"shadow-model","messages":[{"role":"user","content":"ping"}],"max_tokens":8}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/replay", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.adminReplay(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Model    string             `json:"model"`
		Response model.ChatResponse `json:"response"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Model != "shadow-model" || out.Response.Choices[0].Message.Content != "pong" {
		t.Fatalf("unexpected replay body: %s", rec.Body.String())
	}
}

func TestReplayRejectsMalformedJSON(t *testing.T) {
	srv := testServer(t)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/replay", strings.NewReader(`{"model":`))
	rec := httptest.NewRecorder()
	srv.adminReplay(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "goroutine") {
		t.Fatalf("response leaked stack trace: %s", rec.Body.String())
	}
}

func TestChatRejectsOversizedPrompt(t *testing.T) {
	srv := testServer(t)
	huge := `{"model":"main-model","messages":[{"role":"user","content":"` + strings.Repeat("A", 10<<20) + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(huge))
	ctx := middleware.WithUserConfig(req.Context(), &middleware.UserConfig{UserID: 1, RateLimit: 100})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.handleChat(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body capped at 4MB)", rec.Code)
	}
}
