package model

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Persistence-facing entities. Money is NUMERIC(20,10) in PostgreSQL; the Go
// side keeps the same 10-decimal scale and never uses float for arithmetic
// on balances (see billing.PriceTable for the conversion boundary).
// ---------------------------------------------------------------------------

// User represents a gateway tenant.
type User struct {
	ID        uint      `json:"id"`
	Username  string    `json:"username"`
	APIKey    string    `json:"-"`
	RateLimit int       `json:"rate_limit"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Channel is an upstream provider credential set.
type Channel struct {
	ID        uint      `json:"id"`
	Name      string    `json:"name"`
	Provider  string    `json:"provider"` // openai | anthropic
	APIKey    string    `json:"-"`
	BaseURL   string    `json:"base_url"`
	Models    []string  `json:"models"`
	RateLimit int       `json:"rate_limit"`
	Weight    int       `json:"weight"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ModelChannel binds a model name to a channel and prices it.
type ModelChannel struct {
	ID           uint      `json:"id"`
	ModelName    string    `json:"model_name"`
	ChannelID    uint      `json:"channel_id"`
	Channel      *Channel  `json:"channel,omitempty"`
	PricePer1M   NUMERIC   `json:"price_per_1m"` // NUMERIC(20,10) in DDL
	ContextLimit int       `json:"context_limit"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// NUMERIC is a decimal with 10 fractional digits, matching NUMERIC(20,10).
type NUMERIC int64

// Scale is the number of fractional decimal digits.
const Scale = 10

// ScaleFactor is 10^Scale.
const ScaleFactor int64 = 10000000000

// FromFloat converts a float into a NUMERIC at the DB scale, rounding half
// away from zero. Plain truncation loses a unit on values such as 0.00015
// (1.5e6 scaled), which shows up as a one-unit billing drift.
func FromFloat(v float64) NUMERIC {
	scaled := v * float64(ScaleFactor)
	if scaled >= 0 {
		return NUMERIC(int64(scaled + 0.5))
	}
	return NUMERIC(int64(scaled - 0.5))
}

// Float returns the decimal as float64 for reporting only, never for
// balance arithmetic.
func (n NUMERIC) Float() float64 {
	return float64(n) / float64(ScaleFactor)
}

// String renders the decimal with fixed 10-digit scale, trimming trailing
// zeros down to a minimum of two fraction digits.
func (n NUMERIC) String() string {
	neg := n < 0
	v := int64(n)
	if neg {
		v = -v
	}
	whole := v / ScaleFactor
	frac := v % ScaleFactor
	s := fmt.Sprintf("%d.%010d", whole, frac)
	dot := strings.IndexByte(s, '.')
	for strings.HasSuffix(s, "0") && len(s)-dot-1 > 2 {
		s = s[:len(s)-1]
	}
	if neg {
		return "-" + s
	}
	return s
}

// MarshalJSON renders the decimal as a JSON number with full scale.
func (n NUMERIC) MarshalJSON() ([]byte, error) {
	return json.Marshal(n.Float())
}

// UnmarshalJSON accepts a JSON number and rescales it.
func (n *NUMERIC) UnmarshalJSON(b []byte) error {
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	*n = FromFloat(f)
	return nil
}

// Value implements driver.Valuer: PostgreSQL receives the exact decimal text.
func (n NUMERIC) Value() (driver.Value, error) {
	return n.String(), nil
}

// Scan implements sql.Scanner for NUMERIC(20,10) columns.
func (n *NUMERIC) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*n = 0
		return nil
	case string:
		parsed, err := ParseNUMERIC(v)
		if err != nil {
			return err
		}
		*n = parsed
		return nil
	case []byte:
		parsed, err := ParseNUMERIC(string(v))
		if err != nil {
			return err
		}
		*n = parsed
		return nil
	case int64:
		// NUMERIC without fraction.
		if v > math.MaxInt64/ScaleFactor || v < math.MinInt64/ScaleFactor {
			return fmt.Errorf("model: integer %d overflows NUMERIC scale %d", v, Scale)
		}
		*n = NUMERIC(v) * NUMERIC(ScaleFactor)
		return nil
	case float64:
		*n = FromFloat(v)
		return nil
	default:
		return fmt.Errorf("model: cannot scan %T into NUMERIC", src)
	}
}

// ParseNUMERIC parses a PostgreSQL numeric literal ("-12.345600") into the
// fixed-point representation, rejecting exponents and overflow.
func ParseNUMERIC(s string) (NUMERIC, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("model: empty numeric")
	}
	neg := false
	switch s[0] {
	case '-':
		neg, s = true, s[1:]
	case '+':
		s = s[1:]
	}
	if s == "" {
		return 0, fmt.Errorf("model: malformed numeric")
	}
	intPart, fracPart, hasFrac := strings.Cut(s, ".")
	if intPart == "" && fracPart == "" {
		return 0, fmt.Errorf("model: malformed numeric")
	}
	for _, r := range intPart {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("model: malformed numeric %q", s)
		}
	}
	for _, r := range fracPart {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("model: malformed numeric %q", s)
		}
	}
	if hasFrac && len(fracPart) > Scale {
		// Truncate beyond the column scale, matching a numeric cast.
		fracPart = fracPart[:Scale]
	}
	for len(fracPart) < Scale {
		fracPart += "0"
	}
	if intPart == "" {
		intPart = "0"
	}
	digits := strings.TrimLeft(intPart+fracPart, "0")
	if digits == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("model: numeric %q overflows int64 scale %d", s, Scale)
	}
	if neg {
		v = -v
	}
	return NUMERIC(v), nil
}

// ---------------------------------------------------------------------------
// Wire types. Hand-written: no third-party SDK is imported anywhere.
// ---------------------------------------------------------------------------

// Message is a chat message in OpenAI shape.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

// Tool describes a callable function exposed to the model.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction is the OpenAI function schema.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      bool            `json:"strict,omitempty"`
}

// ToolCall is a model-issued function invocation.
type ToolCall struct {
	Index    int              `json:"index,omitempty"`
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction carries the tool name and raw JSON arguments.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatRequest is the OpenAI-compatible chat completion request.
type ChatRequest struct {
	Model          string          `json:"model"`
	Messages       []Message       `json:"messages"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	Temperature    float64         `json:"temperature,omitempty"`
	TopP           float64         `json:"top_p,omitempty"`
	Stream         bool            `json:"stream,omitempty"`
	Stop           []string        `json:"stop,omitempty"`
	User           string          `json:"user,omitempty"`
	Tools          []Tool          `json:"tools,omitempty"`
	ToolChoice     json.RawMessage `json:"tool_choice,omitempty"`
	ResponseFormat json.RawMessage `json:"response_format,omitempty"`
}

// Usage is the token accounting block.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Choice is a single completion choice.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// ChatResponse is the OpenAI-compatible chat completion response.
type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// ChunkDelta is a streaming delta.
type ChunkDelta struct {
	Role      string     `json:"role,omitempty"`
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// ChunkChoice is a streaming choice.
type ChunkChoice struct {
	Index        int        `json:"index"`
	Delta        ChunkDelta `json:"delta"`
	FinishReason string     `json:"finish_reason,omitempty"`
}

// ChatChunk is the OpenAI-compatible SSE payload.
type ChatChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

// AnthropicContent is one block of Anthropic message content.
type AnthropicContent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
}

// AnthropicTool is the Messages API tool declaration.
type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// AnthropicToolChoice mirrors the Messages API tool_choice.
type AnthropicToolChoice struct {
	Type string `json:"type"`           // auto | any | tool | none
	Name string `json:"name,omitempty"` // required when type=tool
}

// AnthropicMessage is one turn in Anthropic shape. Content is either a string
// or a list of content blocks (tool_use / tool_result).
type AnthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// AnthropicRequest is the Anthropic /v1/messages request.
type AnthropicRequest struct {
	Model       string               `json:"model"`
	System      string               `json:"system,omitempty"`
	Messages    []AnthropicMessage   `json:"messages"`
	MaxTokens   int                  `json:"max_tokens"`
	Temperature float64              `json:"temperature,omitempty"`
	TopP        float64              `json:"top_p,omitempty"`
	Stop        []string             `json:"stop_sequences,omitempty"`
	Stream      bool                 `json:"stream,omitempty"`
	Tools       []AnthropicTool      `json:"tools,omitempty"`
	ToolChoice  *AnthropicToolChoice `json:"tool_choice,omitempty"`
}

// AnthropicUsage is Anthropic's token accounting.
type AnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// AnthropicResponse is the Anthropic /v1/messages response.
type AnthropicResponse struct {
	ID         string             `json:"id"`
	Type       string             `json:"type"`
	Role       string             `json:"role"`
	Content    []AnthropicContent `json:"content"`
	Model      string             `json:"model"`
	StopReason string             `json:"stop_reason"`
	Usage      *AnthropicUsage    `json:"usage,omitempty"`
}

// AnthropicStreamEvent covers the SSE event family (message_start,
// content_block_delta, message_delta, message_stop, ping, error).
type AnthropicStreamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index,omitempty"`
	Message *struct {
		ID    string          `json:"id"`
		Model string          `json:"model"`
		Role  string          `json:"role"`
		Usage *AnthropicUsage `json:"usage,omitempty"`
	} `json:"message,omitempty"`
	ContentBlock *struct {
		Type  string          `json:"type"`
		ID    string          `json:"id,omitempty"`
		Name  string          `json:"name,omitempty"`
		Input json.RawMessage `json:"input,omitempty"`
	} `json:"content_block,omitempty"`
	Delta *struct {
		Type        string `json:"type,omitempty"`
		Text        string `json:"text,omitempty"`
		PartialJSON string `json:"partial_json,omitempty"`
		StopReason  string `json:"stop_reason,omitempty"`
	} `json:"delta,omitempty"`
	Usage *AnthropicUsage `json:"usage,omitempty"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Provider is the uniform upstream interface implemented by every adapter.
type Provider interface {
	// Name returns the provider identifier ("openai", "anthropic").
	Name() string
	// Chat performs a non-streaming completion.
	Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error)
	// Stream performs a streaming completion; the channel is closed by the
	// producer and must be drained to avoid goroutine leaks.
	Stream(ctx context.Context, req *ChatRequest) (<-chan *ChatChunk, <-chan error, error)
}
