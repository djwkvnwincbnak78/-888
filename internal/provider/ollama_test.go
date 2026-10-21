package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djwkvnwincbnak78/nexus/internal/model"
)

func ollamaServer(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		switch r.URL.Path {
		case "/api/chat":
			var body ollamaChatRequest
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Stream {
				w.Header().Set("Content-Type", "application/x-ndjson")
				lines := []string{
					`{"model":"llama3","message":{"role":"assistant","content":"Hel"},"done":false}`,
					`{"model":"llama3","message":{"role":"assistant","content":"lo"},"done":false}`,
					`{"model":"llama3","message":{"role":"assistant","content":""},"done":true,"prompt_eval_count":9,"eval_count":2}`,
				}
				for _, l := range lines {
					_, _ = io.WriteString(w, l+"\n")
					w.(http.Flusher).Flush()
				}
				return
			}
			if len(body.Tools) > 0 {
				_, _ = io.WriteString(w, `{
					"model":"llama3","done":true,
					"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"get_weather","arguments":{"city":"Paris"}}}]},
					"prompt_eval_count":11,"eval_count":4}`)
				return
			}
			_, _ = io.WriteString(w, `{"model":"llama3","done":true,
				"message":{"role":"assistant","content":"local-ok"},
				"prompt_eval_count":7,"eval_count":3}`)
		case "/api/tags":
			_, _ = io.WriteString(w, `{"models":[{"name":"llama3:latest","model":"llama3:latest","modified_at":"2026-01-02T03:04:05Z"}]}`)
		case "/api/embed":
			_, _ = io.WriteString(w, `{"model":"bge-m3","embeddings":[[0.1,0.2,0.3],[0.4,0.5,0.6]]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestOllamaChatAndUsage(t *testing.T) {
	srv := ollamaServer(t, nil)
	defer srv.Close()

	p := NewOllama(Config{BaseURL: srv.URL})
	resp, err := p.Chat(context.Background(), &model.ChatRequest{
		Model: "llama3", Messages: []model.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Choices[0].Message.Content != "local-ok" {
		t.Fatalf("content=%q", resp.Choices[0].Message.Content)
	}
	if resp.Usage.PromptTokens != 7 || resp.Usage.CompletionTokens != 3 {
		t.Fatalf("usage=%+v", resp.Usage)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish=%q", resp.Choices[0].FinishReason)
	}
}

func TestOllamaToolCallMapping(t *testing.T) {
	srv := ollamaServer(t, nil)
	defer srv.Close()

	p := NewOllama(Config{BaseURL: srv.URL})
	resp, err := p.Chat(context.Background(), &model.ChatRequest{
		Model:    "llama3",
		Messages: []model.Message{{Role: "user", Content: "weather?"}},
		Tools: []model.Tool{{
			Type: "function",
			Function: model.ToolFunction{
				Name:       "get_weather",
				Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
			},
		}},
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	msg := resp.Choices[0].Message
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("tool calls=%d", len(msg.ToolCalls))
	}
	if msg.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("name=%q", msg.ToolCalls[0].Function.Name)
	}
	if !strings.Contains(msg.ToolCalls[0].Function.Arguments, `"city":"Paris"`) {
		t.Fatalf("args=%q", msg.ToolCalls[0].Function.Arguments)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish=%q", resp.Choices[0].FinishReason)
	}
}

func TestOllamaStreamNDJSON(t *testing.T) {
	srv := ollamaServer(t, nil)
	defer srv.Close()

	p := NewOllama(Config{BaseURL: srv.URL})
	chunks, errs, err := p.Stream(context.Background(), &model.ChatRequest{
		Model: "llama3", Stream: true,
		Messages: []model.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	var text strings.Builder
	var usage *model.Usage
	for c := range chunks {
		for _, ch := range c.Choices {
			text.WriteString(ch.Delta.Content)
		}
		if c.Usage != nil {
			usage = c.Usage
		}
	}
	for e := range errs {
		if e != nil {
			t.Fatalf("stream err: %v", e)
		}
	}
	if text.String() != "Hello" {
		t.Fatalf("assembled=%q", text.String())
	}
	if usage == nil || usage.TotalTokens != 11 {
		t.Fatalf("usage=%+v", usage)
	}
}

func TestOllamaListModelsAndEmbeddings(t *testing.T) {
	srv := ollamaServer(t, nil)
	defer srv.Close()

	p := NewOllama(Config{BaseURL: srv.URL})
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	if len(models) != 1 || models[0].ID != "llama3:latest" || models[0].OwnedBy != "ollama" {
		t.Fatalf("models=%+v", models)
	}

	emb, err := p.Embeddings(context.Background(), &EmbeddingRequest{
		Model: "bge-m3", Input: []string{"a", "b"},
	})
	if err != nil {
		t.Fatalf("embeddings: %v", err)
	}
	if len(emb.Data) != 2 || len(emb.Data[1].Embedding) != 3 {
		t.Fatalf("embeddings=%+v", emb)
	}
	if emb.Data[1].Embedding[2] != 0.6 {
		t.Fatalf("vector=%v", emb.Data[1].Embedding)
	}
}

func TestOllamaRetriesThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":"warming up"}`)
			return
		}
		_, _ = io.WriteString(w, `{"model":"llama3","done":true,"message":{"role":"assistant","content":"finally"}}`)
	}))
	defer srv.Close()

	p := NewOllama(Config{
		BaseURL: srv.URL,
		Retry:   RetryPolicy{Attempts: 3, Base: time.Millisecond, Max: 5 * time.Millisecond},
	})
	resp, err := p.Chat(context.Background(), &model.ChatRequest{
		Model: "llama3", Messages: []model.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Choices[0].Message.Content != "finally" {
		t.Fatalf("content=%q", resp.Choices[0].Message.Content)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls=%d want 3", calls.Load())
	}
}

func TestOllamaRerankUnsupported(t *testing.T) {
	p := NewOllama(Config{BaseURL: "http://127.0.0.1:1"})
	if _, err := p.Rerank(context.Background(), &RerankRequest{Query: "q", Documents: []string{"d"}}); err == nil {
		t.Fatal("expected ErrUnsupported")
	}
}

func TestOllamaStreamHonoursCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		for i := 0; i < 100; i++ {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			_, _ = fmt.Fprintf(w, `{"model":"m","message":{"role":"assistant","content":"x"},"done":false}`+"\n")
			w.(http.Flusher).Flush()
			time.Sleep(5 * time.Millisecond)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	p := NewOllama(Config{BaseURL: srv.URL})
	chunks, errs, err := p.Stream(ctx, &model.ChatRequest{
		Model: "m", Stream: true, Messages: []model.Message{{Role: "user", Content: "x"}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	<-chunks
	cancel()

	done := make(chan struct{})
	go func() {
		for range chunks {
		}
		for range errs {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream goroutine did not exit after cancel")
	}
}
