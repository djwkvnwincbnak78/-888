package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/djwkvnwincbnak78/nexus/internal/model"
)

func TestOpenAIChatRoundTrip(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody model.ChatRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"chatcmpl-1","object":"chat.completion","created":1700000000,"model":"gpt-4o",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hi there"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}
		}`)
	}))
	defer srv.Close()

	p := NewOpenAI(Config{BaseURL: srv.URL, APIKey: "sk-test"})
	resp, err := p.Chat(context.Background(), &model.ChatRequest{
		Model:    "gpt-4o",
		Messages: []model.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("auth header=%q", gotAuth)
	}
	if gotPath != "/chat/completions" {
		t.Fatalf("path=%q", gotPath)
	}
	if gotBody.Stream {
		t.Fatal("non-streaming call sent stream=true")
	}
	if resp.Choices[0].Message.Content != "hi there" {
		t.Fatalf("content=%q", resp.Choices[0].Message.Content)
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 9 {
		t.Fatalf("usage=%+v", resp.Usage)
	}
}

func TestOpenAIStreamSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, line := range []string{
			`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"Hel"}}]}`,
			`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"lo"}}]}`,
			`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
			`data: [DONE]`,
		} {
			_, _ = io.WriteString(w, line+"\n\n")
			flusher.Flush()
		}
	}))
	defer srv.Close()

	p := NewOpenAI(Config{BaseURL: srv.URL, APIKey: "k"})
	chunks, errs, err := p.Stream(context.Background(), &model.ChatRequest{
		Model: "gpt-4o", Messages: []model.Message{{Role: "user", Content: "hi"}}, Stream: true,
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	var text strings.Builder
	var usage *model.Usage
	count := 0
	for c := range chunks {
		count++
		for _, ch := range c.Choices {
			text.WriteString(ch.Delta.Content)
		}
		if c.Usage != nil {
			usage = c.Usage
		}
	}
	for e := range errs {
		if e != nil {
			t.Fatalf("stream err: %v", e)
		}
	}
	if text.String() != "Hello" {
		t.Fatalf("assembled=%q", text.String())
	}
	if count != 4 {
		t.Fatalf("chunks=%d want 4", count)
	}
	if usage == nil || usage.TotalTokens != 5 {
		t.Fatalf("usage=%+v", usage)
	}
}

func TestOpenAIErrorClassification(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited","type":"rate_limit_error","code":"rate_limit_exceeded"}}`)
	}))
	defer srv.Close()

	p := NewOpenAI(Config{BaseURL: srv.URL, APIKey: "k"})
	_, err := p.Chat(context.Background(), &model.ChatRequest{Model: "gpt-4o", Messages: []model.Message{{Role: "user", Content: "x"}}})
	if err == nil {
		t.Fatal("expected error")
	}
	status, ok := StatusOf(err)
	if !ok || status != http.StatusTooManyRequests {
		t.Fatalf("status=%d ok=%v", status, ok)
	}
	if d, ok := RetryAfterOf(err); !ok || d < time.Second {
		t.Fatalf("retry-after=%v ok=%v", d, ok)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "rate_limit_exceeded" {
		t.Fatalf("apiErr=%+v", apiErr)
	}
}

func TestToAnthropicExtractsSystemPrompt(t *testing.T) {
	req := &model.ChatRequest{
		Model:     "claude-3-5-sonnet",
		MaxTokens: 0,
		Messages: []model.Message{
			{Role: "system", Content: "be terse"},
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi"},
			{Role: "user", Content: "again"},
		},
	}
	out := ToAnthropic(req)
	if out.System != "be terse" {
		t.Fatalf("system=%q", out.System)
	}
	if len(out.Messages) != 3 {
		t.Fatalf("messages=%d want 3 (system must be hoisted)", len(out.Messages))
	}
	if out.MaxTokens != 4096 {
		t.Fatalf("max_tokens=%d want default 4096", out.MaxTokens)
	}
}

func TestAnthropicChatAndConversion(t *testing.T) {
	var gotKey, gotVersion, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		_, _ = io.WriteString(w, `{
			"id":"msg_1","type":"message","role":"assistant","model":"claude-3-5-sonnet",
			"content":[{"type":"text","text":"Hello from Claude"}],
			"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":4}
		}`)
	}))
	defer srv.Close()

	p := NewAnthropic(Config{BaseURL: srv.URL, APIKey: "ak", APIVersion: "2023-06-01"})
	resp, err := p.Chat(context.Background(), &model.ChatRequest{
		Model:     "claude-3-5-sonnet",
		MaxTokens: 100,
		Messages: []model.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "hi"},
		},
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if gotKey != "ak" || gotVersion != "2023-06-01" || gotPath != "/messages" {
		t.Fatalf("headers key=%q version=%q path=%q", gotKey, gotVersion, gotPath)
	}
	if !strings.Contains(gotBody, `"system":"sys"`) {
		t.Fatalf("system prompt not hoisted: %s", gotBody)
	}
	if resp.Choices[0].Message.Content != "Hello from Claude" {
		t.Fatalf("content=%q", resp.Choices[0].Message.Content)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish=%q want stop", resp.Choices[0].FinishReason)
	}
	if resp.Usage == nil || resp.Usage.PromptTokens != 11 || resp.Usage.CompletionTokens != 4 {
		t.Fatalf("usage=%+v", resp.Usage)
	}
}

func TestAnthropicStreamTranslation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		events := []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-3\",\"role\":\"assistant\",\"usage\":{\"input_tokens\":9,\"output_tokens\":0}}}",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hel\"}}",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"lo\"}}",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}",
		}
		for _, ev := range events {
			_, _ = io.WriteString(w, ev+"\n\n")
			flusher.Flush()
		}
	}))
	defer srv.Close()

	p := NewAnthropic(Config{BaseURL: srv.URL, APIKey: "ak"})
	chunks, errs, err := p.Stream(context.Background(), &model.ChatRequest{
		Model: "claude-3", Messages: []model.Message{{Role: "user", Content: "hi"}}, Stream: true,
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	var text strings.Builder
	var last *model.ChatChunk
	n := 0
	for c := range chunks {
		n++
		last = c
		for _, ch := range c.Choices {
			text.WriteString(ch.Delta.Content)
		}
	}
	for e := range errs {
		if e != nil {
			t.Fatalf("stream err: %v", e)
		}
	}
	if text.String() != "Hello" {
		t.Fatalf("assembled=%q", text.String())
	}
	if n != 4 { // role chunk + 2 deltas + final
		t.Fatalf("chunks=%d want 4", n)
	}
	if last == nil || last.Usage == nil {
		t.Fatalf("final chunk missing usage: %+v", last)
	}
	if last.Usage.PromptTokens != 9 || last.Usage.CompletionTokens != 2 {
		t.Fatalf("usage=%+v", last.Usage)
	}
	if last.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish=%q", last.Choices[0].FinishReason)
	}
}

func TestReadSSEJoinsMultilineData(t *testing.T) {
	input := "event: x\ndata: a\ndata: b\n\n: ping\n\ndata: tail\n\n"
	var got []sseEvent
	err := readSSE(strings.NewReader(input), func(ev sseEvent) error {
		got = append(got, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("readSSE: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("events=%d want 2: %+v", len(got), got)
	}
	if got[0].Event != "x" || got[0].Data != "a\nb" {
		t.Fatalf("event0=%+v", got[0])
	}
	if got[1].Data != "tail" {
		t.Fatalf("event1=%+v", got[1])
	}
}

func TestStreamStopsOnContextCancel(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for i := 0; i < 100; i++ {
			select {
			case <-release:
				return
			case <-r.Context().Done():
				return
			default:
			}
			_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n")
			flusher.Flush()
			time.Sleep(5 * time.Millisecond)
		}
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	p := NewOpenAI(Config{BaseURL: srv.URL, APIKey: "k"})
	chunks, errs, err := p.Stream(ctx, &model.ChatRequest{Model: "m", Messages: []model.Message{{Role: "user", Content: "x"}}, Stream: true})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	<-chunks
	cancel()
	timeout := time.After(3 * time.Second)
	done := make(chan struct{})
	go func() {
		for range chunks {
		}
		for range errs {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-timeout:
		t.Fatal("stream goroutine did not exit after cancel")
	}
}
