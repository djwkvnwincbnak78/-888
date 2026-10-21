package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/djwkvnwincbnak78/nexus/internal/model"
	"github.com/djwkvnwincbnak78/nexus/internal/tracing"
)

// OpenAI speaks the /v1/chat/completions protocol and is the canonical shape
// used inside the gateway.
type OpenAI struct {
	cfg     Config
	client  *http.Client
	baseURL string
}

// NewOpenAI builds an OpenAI adapter. BaseURL defaults to the public API.
func NewOpenAI(cfg Config) *OpenAI {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	return &OpenAI{cfg: cfg, client: cfg.httpClient(), baseURL: base}
}

// Name implements model.Provider.
func (o *OpenAI) Name() string { return "openai" }

func (o *OpenAI) newRequest(ctx context.Context, path string, stream bool, body any) (*http.Request, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openai: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("openai: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.cfg.APIKey)
	req.Header.Set("User-Agent", o.cfg.userAgent())
	if span, ok := tracing.FromContext(ctx); ok {
		req.Header.Set("traceparent", span.Traceparent())
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	return req, nil
}

func (o *OpenAI) decodeError(resp *http.Response) error {
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if readErr != nil {
		slog.Warn("provider_error_body_read_failed", "provider", "openai", "err", readErr)
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		slog.Warn("provider_error_body_parse_failed", "provider", "openai", "err", err)
	}
	code := envelope.Error.Code
	if code == "" {
		code = envelope.Error.Type
	}
	return &APIError{
		Provider:   "openai",
		StatusCode: resp.StatusCode,
		Code:       code,
		Message:    envelope.Error.Message,
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		Raw:        string(raw),
	}
}

// Chat implements model.Provider.
func (o *OpenAI) Chat(ctx context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("openai: nil request")
	}
	body := *req
	body.Stream = false

	httpReq, err := o.newRequest(ctx, "/chat/completions", false, &body)
	if err != nil {
		return nil, err
	}
	resp, err := o.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai: transport: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, o.decodeError(resp)
	}
	var out model.ChatResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("openai: decode response: %w", err)
	}
	return &out, nil
}

// Stream implements model.Provider. The chunk channel is closed on completion;
// the error channel receives at most one error and is then closed.
func (o *OpenAI) Stream(ctx context.Context, req *model.ChatRequest) (<-chan *model.ChatChunk, <-chan error, error) {
	if req == nil {
		return nil, nil, fmt.Errorf("openai: nil request")
	}
	body := *req
	body.Stream = true
	payload := struct {
		*model.ChatRequest
		StreamOptions map[string]bool `json:"stream_options,omitempty"`
	}{ChatRequest: &body, StreamOptions: map[string]bool{"include_usage": true}}

	httpReq, err := o.newRequest(ctx, "/chat/completions", true, payload)
	if err != nil {
		return nil, nil, err
	}
	resp, err := o.client.Do(httpReq)
	if err != nil {
		return nil, nil, fmt.Errorf("openai: transport: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, nil, o.decodeError(resp)
	}

	chunks := make(chan *model.ChatChunk, 16)
	errs := make(chan error, 1)

	go func() {
		defer close(chunks)
		defer close(errs)
		defer resp.Body.Close()

		err := readSSE(resp.Body, func(ev sseEvent) error {
			data := strings.TrimSpace(ev.Data)
			if data == "" {
				return nil
			}
			if data == "[DONE]" {
				return io.EOF
			}
			var chunk model.ChatChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				return fmt.Errorf("openai: decode chunk: %w", err)
			}
			select {
			case chunks <- &chunk:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		if err != nil {
			errs <- err
		}
	}()

	return chunks, errs, nil
}

// ListModels implements ModelLister via GET /models.
func (o *OpenAI) ListModels(ctx context.Context) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.baseURL+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("openai: build models request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+o.cfg.APIKey)
	req.Header.Set("User-Agent", o.cfg.userAgent())

	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai: transport: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, o.decodeError(resp)
	}
	var out struct {
		Data []ModelInfo `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("openai: decode models: %w", err)
	}
	for i := range out.Data {
		out.Data[i].Object = "model"
		out.Data[i].OwnedBy = "openai"
	}
	return out.Data, nil
}

// Embeddings implements EmbeddingsProvider via POST /embeddings.
func (o *OpenAI) Embeddings(ctx context.Context, in *EmbeddingRequest) (*EmbeddingResponse, error) {
	if in == nil || len(in.Input) == 0 {
		return nil, fmt.Errorf("openai: empty embedding input")
	}
	httpReq, err := o.newRequest(ctx, "/embeddings", false, in)
	if err != nil {
		return nil, err
	}
	resp, err := o.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai: transport: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, o.decodeError(resp)
	}
	var out EmbeddingResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("openai: decode embeddings: %w", err)
	}
	out.Object = "list"
	return &out, nil
}
