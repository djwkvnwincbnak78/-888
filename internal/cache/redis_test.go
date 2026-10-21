package cache

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// newTestRedis returns a store bound to TEST_REDIS_ADDR, or skips.
func newTestRedis(t *testing.T) *RedisStore {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR not set; skipping Redis integration test")
	}
	s := NewRedisStore(RedisOptions{Addr: addr, Password: os.Getenv("TEST_REDIS_PASSWORD")})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRedisSetGetDel(t *testing.T) {
	s := newTestRedis(t)
	ctx := context.Background()
	key := "gw:test:setget"

	if err := s.Set(ctx, key, []byte("hello"), 30*time.Second); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("got %q", got)
	}
	if err := s.Del(ctx, key); err != nil {
		t.Fatalf("del: %v", err)
	}
	if _, err := s.Get(ctx, key); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("want ErrCacheMiss after del, got %v", err)
	}
}

func TestRedisTTLExpiry(t *testing.T) {
	s := newTestRedis(t)
	ctx := context.Background()
	key := "gw:test:ttl"

	if err := s.Set(ctx, key, []byte("v"), time.Second); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, err := s.Get(ctx, key); err != nil {
		t.Fatalf("should exist: %v", err)
	}
	time.Sleep(2100 * time.Millisecond)
	if _, err := s.Get(ctx, key); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("want expired, got %v", err)
	}
}

func TestRedisIncr(t *testing.T) {
	s := newTestRedis(t)
	ctx := context.Background()
	key := "gw:test:incr"
	_ = s.Del(ctx, key)

	for want := int64(1); want <= 3; want++ {
		got, err := s.Incr(ctx, key, time.Minute)
		if err != nil {
			t.Fatalf("incr: %v", err)
		}
		if got != want {
			t.Fatalf("incr=%d want %d", got, want)
		}
	}
	_ = s.Del(ctx, key)
}

func TestRedisBackedExactCache(t *testing.T) {
	s := newTestRedis(t)
	ctx := context.Background()
	c := New(s, Config{})

	k := baseKey()
	k.Prompt = "cache me via redis"
	_ = c.Invalidate(ctx, k)

	if err := c.Put(ctx, k, []byte(`{"ok":1}`), 5); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, kind, err := c.Get(ctx, k)
	if err != nil || kind != LookupExact {
		t.Fatalf("kind=%s err=%v", kind, err)
	}
	if string(got.Body) != `{"ok":1}` {
		t.Fatalf("body=%s", got.Body)
	}
	_ = c.Invalidate(ctx, k)
}
