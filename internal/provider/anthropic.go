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

// Anthropic adapts /v1/messages to the gateway's OpenAI-shaped model.Provider.
type Anthropic struct {
	cfg     Config
	client  *http.Client
	baseURL string
	version string
}

// NewAnthropic builds an Anthropic adapter.
func NewAnthropic(cfg Config) *Anthropic {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = "https://api.anthropic.com/v1"
	}
	version := cfg.APIVersion
	if version == "" {
		version = "2023-06-01"
	}
	return &Anthropic{cfg: cfg, client: cfg.httpClient(), baseURL: base, version: version}
}

// Name implements model.Provider.
func (a *Anthropic) Name() string { return "anthropic" }

// ToAnthropic converts an OpenAI-shaped request into the Messages API shape.
// A "developer" or "system" message becomes the top-level system prompt; the
// remaining turns keep their order.
func ToAnthropic(req *model.ChatRequest) *model.AnthropicRequest {
	out := &model.AnthropicRequest{
		Model:       req.Model,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.Stop,
		Stream:      req.Stream,
	}
	if out.MaxTokens <= 0 {
		out.MaxTokens = 4096 // Messages API requires max_tokens
	}
	var system []string
	for _, m := range req.Messages {
		switch m.Role {
		case "system", "developer":
			if strings.TrimSpace(m.Content) != "" {
				system = append(system, m.Content)
			}
		case "tool":
			// Tool results are user-role content blocks in the Messages API.
			out.Messages = append(out.Messages, model.AnthropicMessage{
				Role: "user",
				Content: []model.AnthropicContent{{
					Type:      "tool_result",
					ToolUseID: m.ToolCallID,
					Content:   m.Content,
				}},
			})
		case "assistant":
			if len(m.ToolCalls) == 0 {
				out.Messages = append(out.Messages, model.AnthropicMessage{Role: "assistant", Content: m.Content})
				continue
			}
			blocks := make([]model.AnthropicContent, 0, len(m.ToolCalls)+1)
			if strings.TrimSpace(m.Content) != "" {
				blocks = append(blocks, model.AnthropicContent{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				args := tc.Function.Arguments
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				blocks = append(blocks, model.AnthropicContent{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: json.RawMessage(args),
				})
			}
			out.Messages = append(out.Messages, model.AnthropicMessage{Role: "assistant", Content: blocks})
		default:
			out.Messages = append(out.Messages, model.AnthropicMessage{Role: m.Role, Content: m.Content})
		}
	}
	out.System = strings.Join(system, "\n\n")

	// Tools: OpenAI {type:function, function:{name,description,parameters}} ->
	// Anthropic {name, description, input_schema}. Nothing is dropped.
	for _, t := range req.Tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		schema := t.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, model.AnthropicTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: schema,
		})
	}
	out.ToolChoice = toAnthropicToolChoice(req.ToolChoice)
	return out
}

// toAnthropicToolChoice maps OpenAI tool_choice onto the Messages API enum.
func toAnthropicToolChoice(raw json.RawMessage) *model.AnthropicToolChoice {
	if len(raw) == 0 {
		return nil
	}
	var v struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	switch v.Type {
	case "auto":
		return &model.AnthropicToolChoice{Type: "auto"}
	case "none":
		return &model.AnthropicToolChoice{Type: "none"}
	case "required":
		return &model.AnthropicToolChoice{Type: "any"}
	case "function":
		return &model.AnthropicToolChoice{Type: "tool", Name: v.Function.Name}
	default:
		return nil
	}
}

// FromAnthropic converts a Messages response into an OpenAI-shaped response.
func FromAnthropic(resp *model.AnthropicResponse) *model.ChatResponse {
	out := &model.ChatResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: 0,
		Model:   resp.Model,
	}
	var text strings.Builder
	var calls []model.ToolCall
	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			args := string(block.Input)
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			calls = append(calls, model.ToolCall{
				ID: block.ID, Type: "function",
				Function: model.ToolCallFunction{Name: block.Name, Arguments: args},
			})
		}
	}
	finish := mapStopReason(resp.StopReason)
	out.Choices = []model.Choice{{
		Index: 0,
		Message: model.Message{
			Role: "assistant", Content: text.String(), ToolCalls: calls,
		},
		FinishReason: finish,
	}}
	if resp.Usage != nil {
		out.Usage = &model.Usage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		}
	}
	return out
}

func mapStopReason(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "":
		return ""
	default:
		return reason
	}
}

func (a *Anthropic) newRequest(ctx context.Context, path string, stream bool, body any) (*http.Request, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", a.cfg.APIKey)
	req.Header.Set("anthropic-version", a.version)
	req.Header.Set("User-Agent", a.cfg.userAgent())
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

func (a *Anthropic) decodeError(resp *http.Response) error {
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if readErr != nil {
		slog.Warn("provider_error_body_read_failed", "provider", "anthropic", "err", readErr)
	}
	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		slog.Warn("provider_error_body_parse_failed", "provider", "anthropic", "err", err)
	}
	return &APIError{
		Provider:   "anthropic",
		StatusCode: resp.StatusCode,
		Code:       envelope.Error.Type,
		Message:    envelope.Error.Message,
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		Raw:        string(raw),
	}
}

// Chat implements model.Provider.
func (a *Anthropic) Chat(ctx context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("anthropic: nil request")
	}
	body := ToAnthropic(req)
	body.Stream = false

	httpReq, err := a.newRequest(ctx, "/messages", false, body)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: transport: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, a.decodeError(resp)
	}
	var out model.AnthropicResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("anthropic: decode response: %w", err)
	}
	return FromAnthropic(&out), nil
}

// Stream implements model.Provider, translating Anthropic events into
// OpenAI-shaped chunks. Anthropic reports input tokens once at message_start
// and output tokens at message_delta; both are mapped onto the final chunk.
func (a *Anthropic) Stream(ctx context.Context, req *model.ChatRequest) (<-chan *model.ChatChunk, <-chan error, error) {
	if req == nil {
		return nil, nil, fmt.Errorf("anthropic: nil request")
	}
	body := ToAnthropic(req)
	body.Stream = true

	httpReq, err := a.newRequest(ctx, "/messages", true, body)
	if err != nil {
		return nil, nil, err
	}
	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, nil, fmt.Errorf("anthropic: transport: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, nil, a.decodeError(resp)
	}

	chunks := make(chan *model.ChatChunk, 16)
	errs := make(chan error, 1)

	go func() {
		defer close(chunks)
		defer close(errs)
		defer resp.Body.Close()

		var (
			id          string
			modelName   string
			inputTokens int
			outputTok   int
			finish      string
		)

		emit := func(c *model.ChatChunk) bool {
			select {
			case chunks <- c:
				return true
			case <-ctx.Done():
				return false
			}
		}

		err := readSSE(resp.Body, func(ev sseEvent) error {
			data := strings.TrimSpace(ev.Data)
			if data == "" {
				return nil
			}
			var event model.AnthropicStreamEvent
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				return fmt.Errorf("anthropic: decode event: %w", err)
			}
			switch event.Type {
			case "message_start":
				if event.Message != nil {
					id = event.Message.ID
					modelName = event.Message.Model
					if event.Message.Usage != nil {
						inputTokens = event.Message.Usage.InputTokens
					}
					if !emit(&model.ChatChunk{
						ID: id, Object: "chat.completion.chunk", Model: modelName,
						Choices: []model.ChunkChoice{{Index: 0, Delta: model.ChunkDelta{Role: "assistant"}}},
					}) {
						return ctx.Err()
					}
				}
			case "content_block_start":
				if event.ContentBlock != nil && event.ContentBlock.Type == "tool_use" {
					if !emit(&model.ChatChunk{
						ID: id, Object: "chat.completion.chunk", Model: modelName,
						Choices: []model.ChunkChoice{{Index: 0, Delta: model.ChunkDelta{
							ToolCalls: []model.ToolCall{{
								Index: event.Index,
								ID:    event.ContentBlock.ID,
								Type:  "function",
								Function: model.ToolCallFunction{
									Name: event.ContentBlock.Name,
								},
							}},
						}}},
					}) {
						return ctx.Err()
					}
				}
			case "content_block_delta":
				if event.Delta != nil && event.Delta.PartialJSON != "" {
					if !emit(&model.ChatChunk{
						ID: id, Object: "chat.completion.chunk", Model: modelName,
						Choices: []model.ChunkChoice{{Index: 0, Delta: model.ChunkDelta{
							ToolCalls: []model.ToolCall{{
								Index:    event.Index,
								Type:     "function",
								Function: model.ToolCallFunction{Arguments: event.Delta.PartialJSON},
							}},
						}}},
					}) {
						return ctx.Err()
					}
				}
				if event.Delta != nil && event.Delta.Text != "" {
					if !emit(&model.ChatChunk{
						ID: id, Object: "chat.completion.chunk", Model: modelName,
						Choices: []model.ChunkChoice{{Index: 0, Delta: model.ChunkDelta{Content: event.Delta.Text}}},
					}) {
						return ctx.Err()
					}
				}
			case "message_delta":
				if event.Delta != nil && event.Delta.StopReason != "" {
					finish = mapStopReason(event.Delta.StopReason)
				}
				if event.Usage != nil {
					outputTok = event.Usage.OutputTokens
				}
			case "message_stop":
				final := &model.ChatChunk{
					ID: id, Object: "chat.completion.chunk", Model: modelName,
					Choices: []model.ChunkChoice{{Index: 0, Delta: model.ChunkDelta{}, FinishReason: finish}},
					Usage: &model.Usage{
						PromptTokens:     inputTokens,
						CompletionTokens: outputTok,
						TotalTokens:      inputTokens + outputTok,
					},
				}
				if !emit(final) {
					return ctx.Err()
				}
				return io.EOF
			case "error":
				if event.Error != nil {
					return fmt.Errorf("anthropic stream error: %s: %s", event.Error.Type, event.Error.Message)
				}
				return fmt.Errorf("anthropic stream error")
			case "ping":
				// keepalive
			}
			return nil
		})
		if err != nil {
			errs <- err
		}
	}()

	return chunks, errs, nil
}
