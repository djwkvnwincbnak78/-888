package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Embedder turns texts into vectors.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Name() string
}

// HTTPEmbedder calls any OpenAI-compatible /embeddings endpoint
// (OpenAI, Ollama, vLLM, TEI, BGE-M3 server, ...).
type HTTPEmbedder struct {
	URL        string
	APIKey     string
	Model      string
	Dimensions int
	Client     *http.Client
}

// NewHTTPEmbedder builds a client; dims<=0 omits the parameter.
func NewHTTPEmbedder(url, apiKey, model string, dims int) *HTTPEmbedder {
	if strings.TrimSpace(model) == "" {
		model = "text-embedding-3-small"
	}
	client := &http.Client{Timeout: 15 * time.Second}
	return &HTTPEmbedder{URL: url, APIKey: apiKey, Model: model, Dimensions: dims, Client: client}
}

// Name implements Embedder.
func (e *HTTPEmbedder) Name() string { return "http:" + e.Model }

// Embed implements Embedder.
func (e *HTTPEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	payload := map[string]any{"model": e.Model, "input": texts}
	if e.Dimensions > 0 {
		payload["dimensions"] = e.Dimensions
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.URL, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.APIKey)
	}
	resp, err := e.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding: transport: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("embedding: http %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("embedding: decode: %w", err)
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("embedding: got %d vectors for %d inputs", len(out.Data), len(texts))
	}
	sort.Slice(out.Data, func(i, j int) bool { return out.Data[i].Index < out.Data[j].Index })
	vecs := make([][]float32, len(out.Data))
	for i := range out.Data {
		vecs[i] = out.Data[i].Embedding
	}
	return vecs, nil
}

// ---------------------------------------------------------------------------
// SemanticCache: embedding-backed, tenant-scoped, threshold-gated.
// ---------------------------------------------------------------------------

// SemanticCache implements SemanticBackend. Entries are namespaced by tenant,
// model and sampling parameters, so a vector search can never cross tenants.
type SemanticCache struct {
	store     Store
	embedder  Embedder
	threshold float64
	ttl       time.Duration
	perScope  int

	mu      sync.RWMutex
	entries map[string][]semEntry
	now     func() time.Time
}

type semEntry struct {
	key     string
	bodyKey string
	vec     []float32
	expires time.Time
}

// NewSemantic wires the cache. threshold<=0 defaults to 0.93.
func NewSemantic(store Store, emb Embedder, threshold float64) *SemanticCache {
	if threshold <= 0 || threshold > 1 {
		threshold = 0.93
	}
	return &SemanticCache{
		store:     store,
		embedder:  emb,
		threshold: threshold,
		ttl:       30 * time.Minute,
		perScope:  512,
		entries:   make(map[string][]semEntry),
		now:       time.Now,
	}
}

// Name implements SemanticBackend.
func (s *SemanticCache) Name() string {
	if s.embedder == nil {
		return "semantic:none"
	}
	return "semantic:" + s.embedder.Name()
}

// scope isolates entries by tenant + model + sampling parameters.
func (s *SemanticCache) scope(k Key) string {
	stop := append([]string(nil), k.StopWords...)
	sort.Strings(stop)
	raw := strings.Join([]string{
		k.TenantID, k.Model, NormalizePrompt(k.System),
		fmt.Sprintf("%.6f", k.Temperature), fmt.Sprint(k.MaxTokens),
		strings.Join(stop, "\x00"), k.ToolsDigest,
	}, "\x1f")
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:16])
}

// Lookup implements SemanticBackend.
func (s *SemanticCache) Lookup(ctx context.Context, k Key) ([]byte, bool, error) {
	if s.embedder == nil {
		return nil, false, nil
	}
	text := NormalizePrompt(k.System + "\n" + k.Prompt)
	vecs, err := s.embedder.Embed(ctx, []string{text})
	if err != nil || len(vecs) != 1 {
		return nil, false, err
	}
	scope := s.scope(k)
	now := s.now()

	s.mu.RLock()
	list := s.entries[scope]
	bestIdx, bestScore := -1, -1.0
	for i := range list {
		if now.After(list[i].expires) {
			continue
		}
		if score := cosine(vecs[0], list[i].vec); score > bestScore {
			bestIdx, bestScore = i, score
		}
	}
	var bodyKey string
	if bestIdx >= 0 && bestScore >= s.threshold {
		bodyKey = list[bestIdx].bodyKey
	}
	s.mu.RUnlock()

	if bodyKey == "" {
		return nil, false, nil
	}
	body, err := s.store.Get(ctx, bodyKey)
	if err != nil {
		return nil, false, nil // expired body: treat as miss, never error the request
	}
	return body, true, nil
}

// Put implements SemanticBackend.
func (s *SemanticCache) Put(ctx context.Context, k Key, body []byte, ttl time.Duration) error {
	if s.embedder == nil || len(body) == 0 {
		return nil
	}
	if ttl <= 0 {
		ttl = s.ttl
	}
	text := NormalizePrompt(k.System + "\n" + k.Prompt)
	vecs, err := s.embedder.Embed(ctx, []string{text})
	if err != nil || len(vecs) != 1 {
		return err
	}
	scope := s.scope(k)
	bodyKey := "sem:" + scope + ":" + k.Hash()
	if err := s.store.Set(ctx, bodyKey, body, ttl); err != nil {
		return err
	}

	now := s.now()
	entry := semEntry{key: k.Hash(), bodyKey: bodyKey, vec: vecs[0], expires: now.Add(ttl)}

	s.mu.Lock()
	defer s.mu.Unlock()
	list := append(s.entries[scope], entry)
	live := list[:0]
	for _, e := range list {
		if now.Before(e.expires) {
			live = append(live, e)
		}
	}
	if len(live) > s.perScope {
		live = live[len(live)-s.perScope:]
	}
	s.entries[scope] = live
	return nil
}

// Size reports tracked vectors (metrics/tests).
func (s *SemanticCache) Size() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, list := range s.entries {
		n += len(list)
	}
	return n
}

func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
