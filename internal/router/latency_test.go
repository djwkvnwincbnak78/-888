package router

import (
	"context"
	"testing"

	"github.com/djwkvnwincbnak78/nexus/internal/model"
)

func TestLatencyTrackerFeedsLatencyStrategy(t *testing.T) {
	tr := NewLatencyTracker()
	r := NewRouter(tr)

	fast := cand(1, "m", 10.0, 1, false)
	slow := cand(2, "m", 10.0, 1, false)

	// Below min samples the tracker reports nothing: strategy degrades to cost.
	if _, ok := tr.LatencyMS(context.Background(), 1); ok {
		t.Fatal("tracker must not report before min samples")
	}

	for i := 0; i < 5; i++ {
		tr.Observe(1, 40)
		tr.Observe(2, 400)
	}
	ms, ok := tr.LatencyMS(context.Background(), 1)
	if !ok || ms < 30 || ms > 50 {
		t.Fatalf("latency1=%v ok=%v", ms, ok)
	}

	d, err := r.Select(context.Background(), "m", []Candidate{slow, fast}, StrategyLatency)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if d.Channel.ID != 1 {
		t.Fatalf("expected fastest channel 1, got %d (%s)", d.Channel.ID, d.Reason)
	}
	if d.Reason != ReasonLatencyPreferred {
		t.Fatalf("reason=%s want %s", d.Reason, ReasonLatencyPreferred)
	}
}

func TestLatencyTrackerSnapshot(t *testing.T) {
	tr := NewLatencyTracker()
	for i := 0; i < 3; i++ {
		tr.Observe(7, 12.5)
	}
	snap := tr.Snapshot()
	if _, ok := snap[7]; !ok {
		t.Fatalf("snapshot missing channel 7: %v", snap)
	}
}

func TestLatencyTrackerIgnoresZeroChannel(t *testing.T) {
	tr := NewLatencyTracker()
	tr.Observe(0, 10)
	if len(tr.Snapshot()) != 0 {
		t.Fatal("channel 0 must be ignored")
	}
}

var _ = model.Channel{}
