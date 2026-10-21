package router

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

type fakeShared struct {
	mu        sync.Mutex
	open      map[uint]bool
	failures  map[uint]int
	threshold int
}

func (f *fakeShared) Open(_ context.Context, id uint) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.open[id], nil
}
func (f *fakeShared) RecordFailure(_ context.Context, id uint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[id]++
	if f.failures[id] >= f.threshold {
		f.open[id] = true
	}
	return nil
}
func (f *fakeShared) RecordSuccess(_ context.Context, id uint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.open, id)
	f.failures[id] = 0
	return nil
}

func TestSharedBreakerExcludesOpenChannel(t *testing.T) {
	shared := &fakeShared{open: map[uint]bool{}, failures: map[uint]int{}, threshold: 2}
	r := NewRouterWithShared(nil, shared)

	a := cand(1, "m", 1.0, 1, false)
	b := cand(2, "m", 1.0, 1, false)

	r.Report(1, OutcomeServerError)
	if d, err := r.Select(context.Background(), "m", []Candidate{a, b}, StrategyWeighted); err != nil || d == nil {
		t.Fatalf("after one failure: d=%v err=%v", d, err)
	}
	r.Report(1, OutcomeServerError)

	for i := 0; i < 20; i++ {
		d, err := r.Select(context.Background(), "m", []Candidate{a, b}, StrategyWeighted)
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		if d.Channel.ID == 1 {
			t.Fatal("shared-breaker-open channel 1 was selected")
		}
	}
	// Recovery propagates from any replica.
	r.Report(1, OutcomeSuccess)
	if d, err := r.Select(context.Background(), "m", []Candidate{a}, StrategyWeighted); err != nil || d.Channel.ID != 1 {
		t.Fatalf("channel did not recover: d=%v err=%v", d, err)
	}
}

func TestRedisBreakerSharesStateAcrossInstances(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR not set; skipping Redis shared-breaker test")
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	defer client.Close()
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	_ = client.Del(ctx, "circuit:channel:77:failures", "circuit:channel:77:open").Err()

	b1 := NewRedisBreaker(client)
	b2 := NewRedisBreaker(client)
	b1.memoTTL = 0
	b2.memoTTL = 0

	for i := 0; i < 5; i++ {
		if err := b1.RecordFailure(ctx, 77); err != nil {
			t.Fatalf("record failure: %v", err)
		}
	}
	open, err := b2.Open(ctx, 77)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !open {
		t.Fatal("second instance did not observe the open circuit")
	}
	if ttl, err := client.TTL(ctx, "circuit:channel:77:open").Result(); err != nil || ttl <= 0 {
		t.Fatalf("open key TTL=%v err=%v", ttl, err)
	}

	if err := b2.RecordSuccess(ctx, 77); err != nil {
		t.Fatalf("record success: %v", err)
	}
	if open, _ := b1.Open(ctx, 77); open {
		t.Fatal("circuit did not close after success")
	}
	_ = client.Del(ctx, "circuit:channel:77:failures", "circuit:channel:77:open").Err()
}

func TestRedisBreakerMemoTTL(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR not set")
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	defer client.Close()
	ctx := context.Background()
	_ = client.Del(ctx, "circuit:channel:88:open").Err()

	b := NewRedisBreaker(client)
	if open, _ := b.Open(ctx, 88); open {
		t.Fatal("fresh circuit should be closed")
	}
	// Another instance opens the circuit; memo delays visibility by <= memoTTL.
	if err := client.Set(ctx, "circuit:channel:88:open", "1", time.Minute).Err(); err != nil {
		t.Fatalf("seed open: %v", err)
	}
	if open, _ := b.Open(ctx, 88); open {
		t.Fatal("memo should still report closed within TTL")
	}
	b.memoTTL = 0
	if open, _ := b.Open(ctx, 88); !open {
		t.Fatal("after memo expiry the shared state must be visible")
	}
	_ = client.Del(ctx, "circuit:channel:88:open").Err()
}
