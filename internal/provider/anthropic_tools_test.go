package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/djwkvnwincbnak78/nexus/internal/model"
)

func TestToAnthropicForwardsToolsAndChoice(t *testing.T) {
	req := &model.ChatRequest{
		Model: "claude-3",
		Messages: []model.Message{
			{Role: "user", Content: "weather in Paris?"},
		},
		Tools: []model.Tool{{
			Type: "function",
			Function: model.ToolFunction{
				Name:        "get_weather",
				Description: "look up weather",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
			},
		}},
		ToolChoice: json.RawMessage(`{"type":"function","function":{"name":"get_weather"}}`),
	}
	out := ToAnthropic(req)
	if len(out.Tools) != 1 {
		t.Fatalf("tools=%d", len(out.Tools))
	}
	if out.Tools[0].Name != "get_weather" || !strings.Contains(string(out.Tools[0].InputSchema), `"city"`) {
		t.Fatalf("tool=%+v", out.Tools[0])
	}
	if out.ToolChoice == nil || out.ToolChoice.Type != "tool" || out.ToolChoice.Name != "get_weather" {
		t.Fatalf("choice=%+v", out.ToolChoice)
	}
}

func TestToAnthropicMapsToolTurns(t *testing.T) {
	req := &model.ChatRequest{
		Model: "claude-3",
		Messages: []model.Message{
			{Role: "user", Content: "weather?"},
			{Role: "assistant", ToolCalls: []model.ToolCall{{
				ID: "toolu_1", Type: "function",
				Function: model.ToolCallFunction{Name: "get_weather", Arguments: `{"city":"Paris"}`},
			}}},
			{Role: "tool", ToolCallID: "toolu_1", Content: `{"temp":21}`},
		},
	}
	out := ToAnthropic(req)

	var assistantBlocks []model.AnthropicContent
	for _, m := range out.Messages {
		if m.Role == "assistant" {
			blocks, ok := m.Content.([]model.AnthropicContent)
			if !ok {
				t.Fatalf("assistant content type=%T", m.Content)
			}
			assistantBlocks = blocks
		}
	}
	if len(assistantBlocks) != 1 || assistantBlocks[0].Type != "tool_use" {
		t.Fatalf("assistant blocks=%+v", assistantBlocks)
	}
	if assistantBlocks[0].ID != "toolu_1" || string(assistantBlocks[0].Input) != `{"city":"Paris"}` {
		t.Fatalf("tool_use block=%+v", assistantBlocks[0])
	}

	last := out.Messages[len(out.Messages)-1]
	blocks, ok := last.Content.([]model.AnthropicContent)
	if !ok || last.Role != "user" || blocks[0].Type != "tool_result" || blocks[0].ToolUseID != "toolu_1" {
		t.Fatalf("tool result message=%+v", last)
	}
}

func TestAnthropicResponseMapsToolUse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{
			"id":"msg_1","type":"message","role":"assistant","model":"claude-3",
			"content":[
				{"type":"text","text":"checking"},
				{"type":"tool_use","id":"toolu_9","name":"get_weather","input":{"city":"Paris"}}
			],
			"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":6}
		}`)
	}))
	defer srv.Close()

	p := NewAnthropic(Config{BaseURL: srv.URL, APIKey: "k"})
	resp, err := p.Chat(context.Background(), &model.ChatRequest{
		Model: "claude-3", Messages: []model.Message{{Role: "user", Content: "weather?"}},
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	msg := resp.Choices[0].Message
	if msg.Content != "checking" {
		t.Fatalf("content=%q", msg.Content)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("tool calls=%+v", msg.ToolCalls)
	}
	if !strings.Contains(msg.ToolCalls[0].Function.Arguments, `"Paris"`) {
		t.Fatalf("arguments=%q", msg.ToolCalls[0].Function.Arguments)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish=%q", resp.Choices[0].FinishReason)
	}
}

func TestAnthropicToolChoiceMapping(t *testing.T) {
	cases := map[string]string{
		`{"type":"auto"}`:     "auto",
		`{"type":"none"}`:     "none",
		`{"type":"required"}`: "any",
	}
	for raw, want := range cases {
		got := toAnthropicToolChoice(json.RawMessage(raw))
		if got == nil || got.Type != want {
			t.Fatalf("%s -> %+v want %s", raw, got, want)
		}
	}
	if got := toAnthropicToolChoice(nil); got != nil {
		t.Fatalf("nil choice -> %+v", got)
	}
}
