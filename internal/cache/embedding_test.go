package cache

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeEmbedder maps words into 8 buckets; identical word sets produce
// identical vectors, which is enough to exercise the cache contracts.
type fakeEmbedder struct {
	err   error
	calls int
}

func (f *fakeEmbedder) Name() string { return "fake" }

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, 8)
		for _, w := range strings.Fields(strings.ToLower(t)) {
			w = strings.Trim(w, "!,.;:?!\"'()[]{}")
			if w == "" {
				continue
			}
			v[len(w)%8] += 1
		}
		out[i] = v
	}
	return out, nil
}

func semanticKey(tenant, prompt string) Key {
	return Key{TenantID: tenant, Model: "gpt-4o", System: "be terse", Prompt: prompt, Temperature: 0.2, MaxTokens: 256}
}

func TestSemanticCacheHitsNearDuplicate(t *testing.T) {
	store := NewMemoryStore(64)
	emb := &fakeEmbedder{}
	sem := NewSemantic(store, emb, 0.99)
	c := New(store, Config{Semantic: sem})

	ctx := context.Background()
	k := semanticKey("t1", "alpha beta gamma")
	if err := c.Put(ctx, k, []byte(`{"answer":42}`), 60); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, kind, err := c.Get(ctx, semanticKey("t1", "alpha beta gamma!!"))
	if err != nil || kind != LookupSemantic {
		t.Fatalf("kind=%s err=%v", kind, err)
	}
	if string(got.Body) != `{"answer":42}` {
		t.Fatalf("body=%s", got.Body)
	}
}

func TestSemanticCacheIsolatesTenantsAndModels(t *testing.T) {
	store := NewMemoryStore(64)
	sem := NewSemantic(store, &fakeEmbedder{}, 0.99)
	c := New(store, Config{Semantic: sem})
	ctx := context.Background()

	_ = c.Put(ctx, semanticKey("t1", "alpha beta"), []byte("t1"), 60)

	if _, kind, _ := c.Get(ctx, semanticKey("t2", "alpha beta")); kind != LookupMiss {
		t.Fatal("cross-tenant semantic leak")
	}
	other := semanticKey("t1", "alpha beta")
	other.Model = "gpt-4o-mini"
	if _, kind, _ := c.Get(ctx, other); kind != LookupMiss {
		t.Fatal("cross-model semantic leak")
	}
}

func TestSemanticCacheMissBelowThreshold(t *testing.T) {
	store := NewMemoryStore(64)
	sem := NewSemantic(store, &fakeEmbedder{}, 0.99)
	c := New(store, Config{Semantic: sem})
	ctx := context.Background()

	_ = c.Put(ctx, semanticKey("t1", "alpha beta"), []byte("x"), 60)
	if _, kind, _ := c.Get(ctx, semanticKey("t1", "zeta omega delta")); kind != LookupMiss {
		t.Fatalf("expected miss, got %s", kind)
	}
}

func TestSemanticCacheDegradesOnEmbedderFailure(t *testing.T) {
	store := NewMemoryStore(64)
	emb := &fakeEmbedder{err: context.DeadlineExceeded}
	sem := NewSemantic(store, emb, 0.9)
	c := New(store, Config{Semantic: sem})

	_, kind, err := c.Get(context.Background(), semanticKey("t1", "anything"))
	if kind != LookupMiss {
		t.Fatalf("kind=%s", kind)
	}
	if err == nil {
		t.Fatal("embedder error should surface to the caller for logging")
	}
}

func TestHTTPEmbedder(t *testing.T) {
	var gotAuth, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		if len(body.Input) != 2 {
			t.Errorf("inputs=%d", len(body.Input))
		}
		_, _ = io.WriteString(w, `{"data":[
			{"index":1,"embedding":[0.3,0.4]},
			{"index":0,"embedding":[0.1,0.2]}
		]}`)
	}))
	defer srv.Close()

	e := NewHTTPEmbedder(srv.URL, "sk-test", "bge-m3", 0)
	vecs, err := e.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if gotAuth != "Bearer sk-test" || gotModel != "bge-m3" {
		t.Fatalf("auth=%q model=%q", gotAuth, gotModel)
	}
	if len(vecs) != 2 || vecs[0][0] != 0.1 || vecs[1][0] != 0.3 {
		t.Fatalf("index order not restored: %+v", vecs)
	}
}

func TestHTTPEmbedderErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, "rate limited")
	}))
	defer srv.Close()

	e := NewHTTPEmbedder(srv.URL, "", "m", 0)
	if _, err := e.Embed(context.Background(), []string{"x"}); err == nil {
		t.Fatal("expected error on 429")
	}

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[0.1]}]}`)
	}))
	defer srv2.Close()
	e2 := NewHTTPEmbedder(srv2.URL, "", "m", 0)
	if _, err := e2.Embed(context.Background(), []string{"a", "b"}); err == nil {
		t.Fatal("expect count mismatch error")
	}
}
