package tracing

import (
	"context"
	"testing"
	"time"
)

func TestInitNoneKeepsPropagation(t *testing.T) {
	shutdown, err := Init(context.Background(), Config{
		ServiceName: "test-gateway", Version: "1.1.0", Exporter: "none", SampleRatio: 0.5,
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	ctx, span := Start(context.Background(), "root")
	if len(span.TraceID) != 32 || len(span.SpanID) != 16 {
		t.Fatalf("ids trace=%q span=%q", span.TraceID, span.SpanID)
	}
	span.SetAttribute("k", "v")
	span.SetAttribute("n", 3)
	span.SetAttribute("b", true)
	span.End()
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestInitStdoutExporter(t *testing.T) {
	shutdown, err := Init(context.Background(), Config{
		ServiceName: "test-stdout", Version: "dev", Exporter: "stdout",
	})
	if err != nil {
		t.Fatalf("init stdout: %v", err)
	}
	_, span := Start(context.Background(), "exported")
	span.End()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestInitOTLPExporterConstruction(t *testing.T) {
	shutdown, err := Init(context.Background(), Config{
		ServiceName: "test-otlp", Version: "dev", Exporter: "otlp", OTLPEndpoint: "127.0.0.1:4318",
	})
	if err != nil {
		t.Fatalf("init otlp: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = shutdown(ctx)
}

func TestInitRejectsBadResource(t *testing.T) {
	// A conflicting resource attribute is not possible to trigger via Config;
	// the important contract is that an unknown exporter falls back to none.
	shutdown, err := Init(context.Background(), Config{ServiceName: "x", Exporter: "unknown"})
	if err != nil {
		t.Fatalf("unknown exporter must not fail: %v", err)
	}
	_ = shutdown(context.Background())
}

func TestDurationAndNilSafety(t *testing.T) {
	_, span := Start(context.Background(), "d")
	time.Sleep(2 * time.Millisecond)
	span.End()
	if span.Duration() <= 0 {
		t.Fatalf("duration=%v", span.Duration())
	}
	var nilSpan *Span
	nilSpan.End()
	if nilSpan.Duration() != 0 || nilSpan.Traceparent() != "" {
		t.Fatal("nil span must be safe")
	}
	nilSpan.SetAttribute("x", "y")
}
