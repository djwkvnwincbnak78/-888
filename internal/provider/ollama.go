package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/djwkvnwincbnak78/nexus/internal/model"
	"github.com/djwkvnwincbnak78/nexus/internal/tracing"
)

// Ollama talks to a local Ollama daemon (/api/chat, /api/tags, /api/embed).
// Its protocol is NDJSON, not SSE, and tool arguments are JSON objects rather
// than strings — both are converted here so callers only see the gateway model.
type Ollama struct {
	cfg     Config
	client  *http.Client
	baseURL string
	retry   RetryPolicy
}

// NewOllama builds an Ollama adapter (default http://127.0.0.1:11434).
func NewOllama(cfg Config) *Ollama {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = "http://127.0.0.1:11434"
	}
	retry := cfg.Retry
	if retry.Attempts <= 0 {
		retry.Attempts = 2 // local daemons fail fast; two attempts is enough
	}
	return &Ollama{cfg: cfg, client: cfg.httpClient(), baseURL: base, retry: retry}
}

// Name implements model.Provider.
func (o *Ollama) Name() string { return "ollama" }

type ollamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
	ToolName  string           `json:"tool_name,omitempty"`
}

type ollamaToolCall struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type ollamaChatRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Stream   bool            `json:"stream"`
	Tools    []model.Tool    `json:"tools,omitempty"`
	Format   json.RawMessage `json:"format,omitempty"`
	Options  map[string]any  `json:"options,omitempty"`
}

type ollamaChatResponse struct {
	Model           string        `json:"model"`
	Message         ollamaMessage `json:"message"`
	Done            bool          `json:"done"`
	PromptEvalCount int           `json:"prompt_eval_count"`
	EvalCount       int           `json:"eval_count"`
}

func toOllamaMessages(msgs []model.Message) []ollamaMessage {
	out := make([]ollamaMessage, 0, len(msgs))
	for _, m := range msgs {
		om := ollamaMessage{Role: m.Role, Content: m.Content, ToolName: m.Name}
		for _, tc := range m.ToolCalls {
			var call ollamaToolCall
			call.Function.Name = tc.Function.Name
			args := strings.TrimSpace(tc.Function.Arguments)
			if args == "" {
				args = "{}"
			}
			call.Function.Arguments = json.RawMessage(args)
			om.ToolCalls = append(om.ToolCalls, call)
		}
		out = append(out, om)
	}
	return out
}

func fromOllamaMessage(m ollamaMessage) model.Message {
	out := model.Message{Role: m.Role, Content: m.Content, Name: m.ToolName}
	for _, tc := range m.ToolCalls {
		args := string(tc.Function.Arguments)
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		out.ToolCalls = append(out.ToolCalls, model.ToolCall{
			ID: "call_" + tc.Function.Name, Type: "function",
			Function: model.ToolCallFunction{Name: tc.Function.Name, Arguments: args},
		})
	}
	return out
}

func (o *Ollama) buildRequest(ctx context.Context, path string, body any) (*http.Request, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("ollama: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("ollama: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", o.cfg.userAgent())
	if span, ok := tracing.FromContext(ctx); ok {
		req.Header.Set("traceparent", span.Traceparent())
	}
	return req, nil
}

// do performs a POST with retries for transport errors, 429 and 5xx.
func (o *Ollama) do(ctx context.Context, path string, body any) (*http.Response, error) {
	var resp *http.Response
	err := withRetry(ctx, o.retry, func() error {
		req, err := o.buildRequest(ctx, path, body)
		if err != nil {
			return err
		}
		r, err := o.client.Do(req)
		if err != nil {
			return fmt.Errorf("ollama: transport: %w", err)
		}
		if r.StatusCode == http.StatusTooManyRequests || r.StatusCode >= 500 {
			raw, _ := io.ReadAll(io.LimitReader(r.Body, maxErrorBody))
			r.Body.Close()
			return &APIError{Provider: "ollama", StatusCode: r.StatusCode, Raw: string(raw)}
		}
		resp = r
		return nil
	})
	return resp, err
}

func (o *Ollama) decodeError(resp *http.Response) error {
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if readErr != nil {
		slog.Warn("provider_error_body_read_failed", "provider", "ollama", "err", readErr)
	}
	var envelope struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		slog.Warn("provider_error_body_parse_failed", "provider", "ollama", "err", err)
	}
	return &APIError{
		Provider:   "ollama",
		StatusCode: resp.StatusCode,
		Message:    envelope.Error,
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		Raw:        string(raw),
	}
}

// Chat implements model.Provider.
func (o *Ollama) Chat(ctx context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("ollama: nil request")
	}
	body := ollamaChatRequest{
		Model:    req.Model,
		Messages: toOllamaMessages(req.Messages),
		Stream:   false,
		Tools:    req.Tools,
		Format:   req.ResponseFormat,
		Options:  ollamaOptions(req),
	}
	resp, err := o.do(ctx, "/api/chat", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, o.decodeError(resp)
	}

	var out ollamaChatResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("ollama: decode response: %w", err)
	}
	finish := "stop"
	msg := fromOllamaMessage(out.Message)
	if len(msg.ToolCalls) > 0 {
		finish = "tool_calls"
	}
	return &model.ChatResponse{
		ID: "ollama-" + out.Model, Object: "chat.completion", Model: out.Model,
		Choices: []model.Choice{{Index: 0, Message: msg, FinishReason: finish}},
		Usage: &model.Usage{
			PromptTokens:     out.PromptEvalCount,
			CompletionTokens: out.EvalCount,
			TotalTokens:      out.PromptEvalCount + out.EvalCount,
		},
	}, nil
}

func ollamaOptions(req *model.ChatRequest) map[string]any {
	opts := map[string]any{}
	if req.Temperature > 0 {
		opts["temperature"] = req.Temperature
	}
	if req.TopP > 0 {
		opts["top_p"] = req.TopP
	}
	if req.MaxTokens > 0 {
		opts["num_predict"] = req.MaxTokens
	}
	if len(req.Stop) > 0 {
		opts["stop"] = req.Stop
	}
	if len(opts) == 0 {
		return nil
	}
	return opts
}

// Stream implements model.Provider over Ollama's NDJSON stream.
func (o *Ollama) Stream(ctx context.Context, req *model.ChatRequest) (<-chan *model.ChatChunk, <-chan error, error) {
	if req == nil {
		return nil, nil, fmt.Errorf("ollama: nil request")
	}
	body := ollamaChatRequest{
		Model:    req.Model,
		Messages: toOllamaMessages(req.Messages),
		Stream:   true,
		Tools:    req.Tools,
		Options:  ollamaOptions(req),
	}
	resp, err := o.do(ctx, "/api/chat", body)
	if err != nil {
		return nil, nil, err
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

		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64*1024), 4<<20)

		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var event ollamaChatResponse
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				errs <- fmt.Errorf("ollama: decode stream line: %w", err)
				return
			}
			msg := fromOllamaMessage(event.Message)
			chunk := &model.ChatChunk{
				ID: "ollama-" + event.Model, Object: "chat.completion.chunk", Model: event.Model,
				Choices: []model.ChunkChoice{{Index: 0, Delta: model.ChunkDelta{
					Role: msg.Role, Content: msg.Content, ToolCalls: msg.ToolCalls,
				}}},
			}
			if event.Done {
				chunk.Usage = &model.Usage{
					PromptTokens:     event.PromptEvalCount,
					CompletionTokens: event.EvalCount,
					TotalTokens:      event.PromptEvalCount + event.EvalCount,
				}
			}
			select {
			case chunks <- chunk:
			case <-ctx.Done():
				return
			}
			if event.Done {
				return
			}
		}
		if err := sc.Err(); err != nil {
			errs <- fmt.Errorf("ollama: stream read: %w", err)
		}
	}()

	return chunks, errs, nil
}

// ListModels implements ModelLister via /api/tags.
func (o *Ollama) ListModels(ctx context.Context) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.baseURL+"/api/tags", nil)
	if err != nil {
		return nil, fmt.Errorf("ollama: build tags request: %w", err)
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama: transport: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, o.decodeError(resp)
	}
	var out struct {
		Models []struct {
			Name       string    `json:"name"`
			Model      string    `json:"model"`
			ModifiedAt time.Time `json:"modified_at"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("ollama: decode tags: %w", err)
	}
	models := make([]ModelInfo, 0, len(out.Models))
	for _, m := range out.Models {
		id := m.Name
		if id == "" {
			id = m.Model
		}
		models = append(models, ModelInfo{
			ID: id, Object: "model", Created: m.ModifiedAt.Unix(), OwnedBy: "ollama",
		})
	}
	return models, nil
}

// Embeddings implements EmbeddingsProvider via /api/embed.
func (o *Ollama) Embeddings(ctx context.Context, in *EmbeddingRequest) (*EmbeddingResponse, error) {
	if in == nil || len(in.Input) == 0 {
		return nil, fmt.Errorf("ollama: empty embedding input")
	}
	body := map[string]any{"model": in.Model, "input": in.Input}
	resp, err := o.do(ctx, "/api/embed", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, o.decodeError(resp)
	}
	var out struct {
		Model      string      `json:"model"`
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("ollama: decode embeddings: %w", err)
	}
	res := &EmbeddingResponse{Object: "list", Model: out.Model}
	for i, vec := range out.Embeddings {
		res.Data = append(res.Data, Embedding{Object: "embedding", Index: i, Embedding: vec})
	}
	return res, nil
}

// Rerank is not part of the Ollama API.
func (o *Ollama) Rerank(context.Context, *RerankRequest) (*RerankResponse, error) {
	return nil, fmt.Errorf("ollama: %w", ErrUnsupported)
}
