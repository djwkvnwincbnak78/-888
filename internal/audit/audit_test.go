package audit

import (
	"context"
	"testing"
)

func TestNopSink(t *testing.T) {
	var s Sink = NopSink{}
	if err := s.Write(context.Background(), Entry{RequestID: "r"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if s.Dropped() != 0 {
		t.Fatal("nop sink must not drop")
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestMemorySinkKeepsEntries(t *testing.T) {
	s := &MemorySink{}
	_ = s.Write(context.Background(), Entry{RequestID: "a", Status: "success"})
	_ = s.Write(context.Background(), Entry{RequestID: "b", Status: "error"})
	if len(s.Entries) != 2 {
		t.Fatalf("entries=%d", len(s.Entries))
	}
	if s.Entries[1].Status != "error" {
		t.Fatalf("order/status wrong: %+v", s.Entries)
	}
}

func TestPostgresSinkDropsWhenBufferFull(t *testing.T) {
	// No worker healthy (nil DB would panic in insert), so instead exercise the
	// non-blocking contract with a sink whose channel is pre-filled.
	s := &PostgresSink{ch: make(chan Entry, 1), done: make(chan struct{})}
	s.ch <- Entry{RequestID: "first"}
	if err := s.Write(context.Background(), Entry{RequestID: "second"}); err != nil {
		t.Fatalf("write must not error: %v", err)
	}
	if s.Dropped() != 1 {
		t.Fatalf("dropped=%d want 1", s.Dropped())
	}
}
