package provider

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"strconv"
	"time"
)

// APIError is a structured upstream failure. The router classifies failures by
// StatusCode: 429 -> queue/retry, 5xx -> trip breaker and fail over.
type APIError struct {
	Provider   string
	StatusCode int
	Code       string
	Message    string
	RetryAfter time.Duration
	Raw        string
}

// Error implements error.
func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%s: %d %s: %s", e.Provider, e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("%s: http %d", e.Provider, e.StatusCode)
}

// StatusOf extracts an HTTP status from an error chain.
func StatusOf(err error) (int, bool) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode, true
	}
	return 0, false
}

// RetryAfterOf extracts a server-provided retry hint.
func RetryAfterOf(err error) (time.Duration, bool) {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
		return apiErr.RetryAfter, true
	}
	return 0, false
}

// Config configures an upstream adapter.
type Config struct {
	BaseURL    string
	APIKey     string
	APIVersion string        // anthropic-version, ignored by OpenAI
	Timeout    time.Duration // per-request timeout
	Client     *http.Client  // inject an SSRF-guarded client; nil -> guarded default
	UserAgent  string
	Retry      RetryPolicy // adapter-level retries (transport / 429 / 5xx)
}

func (c Config) httpClient() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &http.Client{Timeout: timeout}
}

func (c Config) userAgent() string {
	if c.UserAgent != "" {
		return c.UserAgent
	}
	return "super-llm-gateway/0.1"
}

func parseRetryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// ErrUnsupported is returned by adapters that cannot serve a capability.
var ErrUnsupported = errors.New("provider: capability not supported")

// ModelInfo is the OpenAI-shaped model descriptor.
type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ModelLister lists upstream models.
type ModelLister interface {
	ListModels(ctx context.Context) ([]ModelInfo, error)
}

// EmbeddingRequest is the OpenAI-shaped embeddings request.
type EmbeddingRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	Dimensions     int      `json:"dimensions,omitempty"`
	EncodingFormat string   `json:"encoding_format,omitempty"`
}

// Embedding is one vector in the response.
type Embedding struct {
	Object    string    `json:"object"`
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

// EmbeddingUsage reports token consumption.
type EmbeddingUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// EmbeddingResponse is the OpenAI-shaped embeddings response.
type EmbeddingResponse struct {
	Object string          `json:"object"`
	Data   []Embedding     `json:"data"`
	Model  string          `json:"model"`
	Usage  *EmbeddingUsage `json:"usage,omitempty"`
}

// EmbeddingsProvider computes vectors.
type EmbeddingsProvider interface {
	Embeddings(ctx context.Context, req *EmbeddingRequest) (*EmbeddingResponse, error)
}

// RerankRequest asks a model to score documents against a query.
type RerankRequest struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      int      `json:"top_n,omitempty"`
}

// RerankResult is one scored document.
type RerankResult struct {
	Index          int     `json:"index"`
	RelevanceScore float64 `json:"relevance_score"`
	Document       string  `json:"document,omitempty"`
}

// RerankResponse is the rerank result set.
type RerankResponse struct {
	Model   string         `json:"model"`
	Results []RerankResult `json:"results"`
	Usage   map[string]int `json:"usage,omitempty"`
}

// Reranker scores documents. Implemented by providers that expose a rerank API.
type Reranker interface {
	Rerank(ctx context.Context, req *RerankRequest) (*RerankResponse, error)
}

// RetryPolicy controls adapter-level retries for transport/5xx failures.
type RetryPolicy struct {
	Attempts int           // total attempts, default 3
	Base     time.Duration // base backoff, default 200ms
	Max      time.Duration // cap, default 3s
}

func (p RetryPolicy) attempts() int {
	if p.Attempts <= 0 {
		return 3
	}
	return p.Attempts
}

func (p RetryPolicy) base() time.Duration {
	if p.Base <= 0 {
		return 200 * time.Millisecond
	}
	return p.Base
}

func (p RetryPolicy) max() time.Duration {
	if p.Max <= 0 {
		return 3 * time.Second
	}
	return p.Max
}

// backoff returns an exponential delay with full jitter.
func (p RetryPolicy) backoff(attempt int) time.Duration {
	d := p.base() << (attempt - 1)
	if d > p.max() || d <= 0 {
		d = p.max()
	}
	// full jitter in [d/2, d]
	half := d / 2
	return half + time.Duration(rand.Int63n(int64(half)+1))
}

// retryable reports whether an error deserves another attempt.
func retryable(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == 429 || apiErr.StatusCode >= 500
	}
	// Transport-level errors are retryable.
	return true
}

// withRetry runs fn with exponential backoff, honouring ctx.
func withRetry(ctx context.Context, p RetryPolicy, fn func() error) error {
	var err error
	for attempt := 1; attempt <= p.attempts(); attempt++ {
		if err = fn(); err == nil {
			return nil
		}
		if !retryable(err) || attempt == p.attempts() {
			return err
		}
		delay := p.backoff(attempt)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}
