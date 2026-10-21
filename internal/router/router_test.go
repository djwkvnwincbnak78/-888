package router

import (
	"context"
	"testing"

	"github.com/djwkvnwincbnak78/nexus/internal/model"
)

func cand(id uint, modelName string, price float64, weight int, pinned bool) Candidate {
	return Candidate{
		Model: &model.ModelChannel{
			ModelName:  modelName,
			ChannelID:  id,
			PricePer1M: model.FromFloat(price),
		},
		Channel: &model.Channel{ID: id, Provider: "openai", Weight: weight},
		Pinned:  pinned,
		Healthy: true,
	}
}

func TestCostOptimizedPicksCheapest(t *testing.T) {
	r := NewRouter(nil)
	cands := []Candidate{
		cand(1, "gpt-4", 30.0, 10, false),
		cand(2, "gpt-4", 5.0, 1, false),
		cand(3, "gpt-4", 12.0, 5, false),
	}
	d, err := r.Select(context.Background(), "gpt-4", cands, StrategyCost)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if d.Channel.ID != 2 {
		t.Fatalf("expected cheapest channel 2, got %d", d.Channel.ID)
	}
	if d.Reason != ReasonCostOptimized {
		t.Fatalf("expected reason %s, got %s", ReasonCostOptimized, d.Reason)
	}
}

func TestFixedPrefersPinned(t *testing.T) {
	r := NewRouter(nil)
	cands := []Candidate{
		cand(1, "gpt-4", 30.0, 100, false),
		cand(7, "gpt-4", 99.0, 1, true),
	}
	d, err := r.Select(context.Background(), "gpt-4", cands, StrategyFixed)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if d.Channel.ID != 7 || d.Reason != ReasonFixedPinned {
		t.Fatalf("expected pinned channel 7, got %d (%s)", d.Channel.ID, d.Reason)
	}
}

func TestWeightedDistributionFollowsWeights(t *testing.T) {
	r := NewRouter(nil)
	cands := []Candidate{
		cand(1, "m", 1.0, 1, false),
		cand(2, "m", 1.0, 3, false),
	}
	counts := map[uint]int{}
	const draws = 20000
	for i := 0; i < draws; i++ {
		d, err := r.Select(context.Background(), "m", cands, StrategyWeighted)
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		counts[d.Channel.ID]++
	}
	got := float64(counts[2]) / float64(draws)
	if got < 0.72 || got > 0.78 {
		t.Fatalf("weight 3:1 produced %.3f for channel 2, want ~0.75", got)
	}
}

func TestBreakerOpensAfterServerErrors(t *testing.T) {
	r := NewRouter(nil)
	cands := []Candidate{
		cand(1, "m", 1.0, 1, false),
		cand(2, "m", 1.0, 1, false),
	}
	for i := 0; i < 5; i++ {
		r.Report(1, OutcomeServerError)
	}
	seen := 0
	for i := 0; i < 50; i++ {
		d, err := r.Select(context.Background(), "m", cands, StrategyWeighted)
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		if d.Channel.ID == 1 {
			seen++
		}
	}
	if seen != 0 {
		t.Fatalf("circuit-broken channel 1 was selected %d times", seen)
	}
	r.Report(1, OutcomeSuccess) // half-open reset
	if b := r.breakerFor(1); !b.allow(r.now()) {
		t.Fatal("breaker did not reset after success")
	}
}

func TestSelectRejectsUnknownModelAndEmptyPool(t *testing.T) {
	r := NewRouter(nil)
	if _, err := r.Select(context.Background(), "m", nil, StrategyCost); err != ErrNoCandidate {
		t.Fatalf("want ErrNoCandidate, got %v", err)
	}
	cands := []Candidate{cand(1, "other", 1.0, 1, false)}
	if _, err := r.Select(context.Background(), "m", cands, StrategyCost); err != ErrAllBreakersOpen {
		t.Fatalf("want ErrAllBreakersOpen, got %v", err)
	}
}

func TestClassify(t *testing.T) {
	cases := map[int]Outcome{
		200: OutcomeSuccess, 429: OutcomeRateLimited,
		500: OutcomeServerError, 503: OutcomeServerError, 400: OutcomeClientError,
	}
	for code, want := range cases {
		if got := Classify(code); got != want {
			t.Fatalf("Classify(%d)=%v want %v", code, got, want)
		}
	}
}
