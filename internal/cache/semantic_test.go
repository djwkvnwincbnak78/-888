package cache

import (
	"context"
	"errors"
	"testing"
	"time"
)

func baseKey() Key {
	return Key{
		TenantID:    "tenant-a",
		Model:       "gpt-4o",
		System:      "you are terse",
		Prompt:      "hello world",
		Temperature: 0.2,
		MaxTokens:   512,
		StopWords:   []string{"\n\n"},
	}
}

func TestExactHitOnlyForIdenticalKey(t *testing.T) {
	c := New(NewMemoryStore(16), Config{})
	ctx := context.Background()
	k := baseKey()

	if _, kind, _ := c.Get(ctx, k); kind != LookupMiss {
		t.Fatalf("expected miss, got %s", kind)
	}
	if err := c.Put(ctx, k, []byte(`{"ok":true}`), 10); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, kind, err := c.Get(ctx, k)
	if err != nil || kind != LookupExact {
		t.Fatalf("kind=%s err=%v", kind, err)
	}
	if string(got.Body) != `{"ok":true}` {
		t.Fatalf("body=%s", got.Body)
	}

	// whitespace normalisation is the only normalisation allowed
	k2 := k
	k2.Prompt = "hello   world"
	if _, kind, _ := c.Get(ctx, k2); kind != LookupExact {
		t.Fatalf("whitespace-normalised prompt should hit, got %s", kind)
	}
}

func TestSamplingParamsChangeTheEntry(t *testing.T) {
	c := New(NewMemoryStore(16), Config{})
	ctx := context.Background()
	k := baseKey()
	_ = c.Put(ctx, k, []byte("a"), 1)

	mutations := map[string]func(*Key){
		"temperature": func(x *Key) { x.Temperature = 0.9 },
		"max_tokens":  func(x *Key) { x.MaxTokens = 4096 },
		"stop_words":  func(x *Key) { x.StopWords = []string{"END"} },
		"system":      func(x *Key) { x.System = "you may ramble" },
		"model":       func(x *Key) { x.Model = "gpt-4o-mini" },
	}
	for name, mutate := range mutations {
		x := k
		mutate(&x)
		if _, kind, _ := c.Get(ctx, x); kind != LookupMiss {
			t.Fatalf("%s change must miss, got %s", name, kind)
		}
	}
}

func TestTenantIsolation(t *testing.T) {
	c := New(NewMemoryStore(16), Config{})
	ctx := context.Background()
	k := baseKey()
	if err := c.Put(ctx, k, []byte("tenant-a-secret"), 1); err != nil {
		t.Fatalf("put: %v", err)
	}

	other := k
	other.TenantID = "tenant-b"
	if _, kind, _ := c.Get(ctx, other); kind != LookupMiss {
		t.Fatalf("cross-tenant cache leak: %s", kind)
	}
}

func TestNoFuzzyHitWithoutSemanticBackend(t *testing.T) {
	c := New(NewMemoryStore(16), Config{})
	ctx := context.Background()
	k := baseKey()
	_ = c.Put(ctx, k, []byte("paris"), 1)

	// Near-identical wording must NOT hit: exact-only by design.
	near := k
	near.Prompt = "hello world!"
	if _, kind, _ := c.Get(ctx, near); kind != LookupMiss {
		t.Fatalf("fuzzy match leaked through: %s", kind)
	}
	if c.SemanticEnabled() {
		t.Fatal("semantic backend must be nil by default")
	}
}

// fakeSemantic proves the seam works when a real backend is injected.
type fakeSemantic struct{ hits bool }

func (f *fakeSemantic) Lookup(context.Context, Key) ([]byte, bool, error) {
	if f.hits {
		return []byte("semantic-body"), true, nil
	}
	return nil, false, nil
}
func (f *fakeSemantic) Put(context.Context, Key, []byte, time.Duration) error { return nil }
func (f *fakeSemantic) Name() string                                          { return "fake" }

func TestSemanticSeamWhenExplicitlyInjected(t *testing.T) {
	c := New(NewMemoryStore(16), Config{Semantic: &fakeSemantic{hits: true}})
	k := baseKey()
	k.Prompt = "never seen before"
	got, kind, err := c.Get(context.Background(), k)
	if err != nil || kind != LookupSemantic {
		t.Fatalf("kind=%s err=%v", kind, err)
	}
	if string(got.Body) != "semantic-body" {
		t.Fatalf("body=%s", got.Body)
	}
}

func TestMemoryStoreLRUEviction(t *testing.T) {
	m := NewMemoryStore(2)
	ctx := context.Background()
	_ = m.Set(ctx, "a", []byte("1"), time.Minute)
	_ = m.Set(ctx, "b", []byte("2"), time.Minute)
	_, _ = m.Get(ctx, "a") // touch a, b becomes LRU
	_ = m.Set(ctx, "c", []byte("3"), time.Minute)

	if _, err := m.Get(ctx, "b"); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("expected b evicted, err=%v", err)
	}
	if _, err := m.Get(ctx, "a"); err != nil {
		t.Fatalf("a should survive: %v", err)
	}
	if _, err := m.Get(ctx, "c"); err != nil {
		t.Fatalf("c should exist: %v", err)
	}
}

func TestMemoryStoreTTL(t *testing.T) {
	m := NewMemoryStore(4)
	now := time.Unix(1000, 0)
	m.now = func() time.Time { return now }
	ctx := context.Background()
	_ = m.Set(ctx, "k", []byte("v"), 10*time.Second)

	if _, err := m.Get(ctx, "k"); err != nil {
		t.Fatalf("should be alive: %v", err)
	}
	now = now.Add(11 * time.Second)
	if _, err := m.Get(ctx, "k"); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("should be expired, err=%v", err)
	}
}

func TestCacheStats(t *testing.T) {
	c := New(NewMemoryStore(8), Config{})
	ctx := context.Background()
	k := baseKey()
	_ = c.Put(ctx, k, []byte("x"), 1)
	c.Get(ctx, k)
	other := k
	other.TenantID = "tenant-z"
	c.Get(ctx, other)
	c.Get(ctx, other)

	exact, sem, miss := c.Stats()
	if exact != 1 || sem != 0 || miss != 2 {
		t.Fatalf("stats exact=%d sem=%d miss=%d", exact, sem, miss)
	}
}

func TestKeyHashOrderInsensitiveStopWords(t *testing.T) {
	a := baseKey()
	a.StopWords = []string{"A", "B"}
	b := baseKey()
	b.StopWords = []string{"B", "A"}
	if a.Hash() != b.Hash() {
		t.Fatal("stop word order must not change the key")
	}
}
