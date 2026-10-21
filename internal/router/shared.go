package router

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// SharedBreaker keeps circuit state outside the process so every gateway
// replica sees the same open/closed decision.
type SharedBreaker interface {
	Open(ctx context.Context, channelID uint) (bool, error)
	RecordFailure(ctx context.Context, channelID uint) error
	RecordSuccess(ctx context.Context, channelID uint) error
}

// RedisBreaker implements SharedBreaker with two keys per channel:
//
//	circuit:channel:{id}:failures  INCR with a sliding window TTL
//	circuit:channel:{id}:open      SETEX, present while the circuit is open
//
// A local memo (default 500ms) avoids one Redis round trip per candidate on
// every request while still converging across replicas quickly.
type RedisBreaker struct {
	client    redis.UniversalClient
	threshold int64
	window    time.Duration
	openFor   time.Duration
	prefix    string

	mu      sync.Mutex
	memo    map[uint]memoEntry
	memoTTL time.Duration
	now     func() time.Time
}

type memoEntry struct {
	open    bool
	checked time.Time
}

// NewRedisBreaker builds a breaker. Defaults: 5 failures / 60s window, open 30s.
func NewRedisBreaker(client redis.UniversalClient) *RedisBreaker {
	return &RedisBreaker{
		client:    client,
		threshold: 5,
		window:    60 * time.Second,
		openFor:   30 * time.Second,
		prefix:    "circuit:channel:",
		memo:      make(map[uint]memoEntry),
		memoTTL:   500 * time.Millisecond,
		now:       time.Now,
	}
}

func (b *RedisBreaker) failuresKey(id uint) string {
	return fmt.Sprintf("%s%d:failures", b.prefix, id)
}

func (b *RedisBreaker) openKey(id uint) string {
	return fmt.Sprintf("%s%d:open", b.prefix, id)
}

// Open implements SharedBreaker.
func (b *RedisBreaker) Open(ctx context.Context, channelID uint) (bool, error) {
	now := b.now()
	b.mu.Lock()
	if e, ok := b.memo[channelID]; ok && now.Sub(e.checked) < b.memoTTL {
		b.mu.Unlock()
		return e.open, nil
	}
	b.mu.Unlock()

	n, err := b.client.Exists(ctx, b.openKey(channelID)).Result()
	if err != nil {
		return false, err
	}
	open := n > 0
	b.mu.Lock()
	b.memo[channelID] = memoEntry{open: open, checked: now}
	b.mu.Unlock()
	return open, nil
}

// RecordFailure implements SharedBreaker.
func (b *RedisBreaker) RecordFailure(ctx context.Context, channelID uint) error {
	key := b.failuresKey(channelID)
	pipe := b.client.Pipeline()
	incr := pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, b.window)
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}
	if incr.Val() >= b.threshold {
		if err := b.client.Set(ctx, b.openKey(channelID), "1", b.openFor).Err(); err != nil {
			return err
		}
		_ = b.client.Del(ctx, key).Err()
		b.setMemo(channelID, true)
	}
	return nil
}

// RecordSuccess implements SharedBreaker.
func (b *RedisBreaker) RecordSuccess(ctx context.Context, channelID uint) error {
	pipe := b.client.Pipeline()
	pipe.Del(ctx, b.failuresKey(channelID))
	pipe.Del(ctx, b.openKey(channelID))
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}
	b.setMemo(channelID, false)
	return nil
}

func (b *RedisBreaker) setMemo(channelID uint, open bool) {
	b.mu.Lock()
	b.memo[channelID] = memoEntry{open: open, checked: b.now()}
	b.mu.Unlock()
}
