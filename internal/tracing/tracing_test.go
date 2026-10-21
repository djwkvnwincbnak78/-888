package tracing

import (
	"context"
	"testing"
)

func TestStartCreatesValidIDs(t *testing.T) {
	ctx, span := Start(context.Background(), "chat")
	if len(span.TraceID) != 32 || len(span.SpanID) != 16 {
		t.Fatalf("ids trace=%q span=%q", span.TraceID, span.SpanID)
	}
	got, ok := FromContext(ctx)
	if !ok || got != span {
		t.Fatal("span not stored in context")
	}
	traceID, spanID, ok := ParseTraceparent(span.Traceparent())
	if !ok || traceID != span.TraceID || spanID != span.SpanID {
		t.Fatalf("traceparent roundtrip failed: %s", span.Traceparent())
	}
}

func TestChildInheritsTraceID(t *testing.T) {
	ctx, parent := Start(context.Background(), "parent")
	_, child := Start(ctx, "child")
	if child.TraceID != parent.TraceID {
		t.Fatalf("child trace=%s parent=%s", child.TraceID, parent.TraceID)
	}
	if child.SpanID == parent.SpanID {
		t.Fatal("span ids must differ")
	}
}

func TestInboundTraceparentAdopted(t *testing.T) {
	const tp = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	ctx := WithTraceparent(context.Background(), tp)
	_, span := Start(ctx, "handler")
	if span.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("trace=%s", span.TraceID)
	}
}

func TestParseTraceparentRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "00-xyz-abc-01", "01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "00-4bf92f3577b34da6a3ce929d0e0e4736-01"} {
		if _, _, ok := ParseTraceparent(bad); ok {
			t.Fatalf("%q should be rejected", bad)
		}
	}
}

func TestSpanEnd(t *testing.T) {
	_, span := Start(context.Background(), "x")
	span.End()
	if span.Duration() < 0 || span.Ended.IsZero() {
		t.Fatalf("span not ended: %+v", span)
	}
}
