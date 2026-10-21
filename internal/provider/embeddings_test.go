package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/djwkvnwincbnak78/nexus/internal/model"
)

func TestOpenAIListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" || r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("missing auth header")
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"gpt-4o","created":1700000000,"owned_by":"openai"}]}`)
	}))
	defer srv.Close()

	p := NewOpenAI(Config{BaseURL: srv.URL, APIKey: "k"})
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(models) != 1 || models[0].ID != "gpt-4o" || models[0].Object != "model" {
		t.Fatalf("models=%+v", models)
	}
}

func TestOpenAIEmbeddings(t *testing.T) {
	var got EmbeddingRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("path=%s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = io.WriteString(w, `{"object":"list","model":"text-embedding-3-small",
			"data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],
			"usage":{"prompt_tokens":4,"total_tokens":4}}`)
	}))
	defer srv.Close()

	p := NewOpenAI(Config{BaseURL: srv.URL, APIKey: "k"})
	resp, err := p.Embeddings(context.Background(), &EmbeddingRequest{
		Model: "text-embedding-3-small", Input: []string{"hello"}, Dimensions: 2,
	})
	if err != nil {
		t.Fatalf("embeddings: %v", err)
	}
	if got.Model != "text-embedding-3-small" || len(got.Input) != 1 || got.Dimensions != 2 {
		t.Fatalf("request not forwarded: %+v", got)
	}
	if len(resp.Data) != 1 || resp.Data[0].Embedding[1] != 0.2 {
		t.Fatalf("response=%+v", resp)
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 4 {
		t.Fatalf("usage=%+v", resp.Usage)
	}
}

func TestOpenAIEmbeddingsRejectsEmptyInput(t *testing.T) {
	p := NewOpenAI(Config{BaseURL: "http://127.0.0.1:1", APIKey: "k"})
	if _, err := p.Embeddings(context.Background(), &EmbeddingRequest{Model: "m"}); err == nil {
		t.Fatal("empty input must fail before any network call")
	}
}

var _ = model.ChatRequest{}
