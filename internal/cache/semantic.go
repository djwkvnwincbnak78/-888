package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrCacheMiss is returned by helpers when a key is absent.
var ErrCacheMiss = errors.New("cache: miss")

// ---------------------------------------------------------------------------
// Store — byte-level KV backend.
//   MemoryStore  in-process LRU (tests, single-node demo)
//   RedisStore   go-redis v9 client (pooled, cluster/sentinel capable)
// ---------------------------------------------------------------------------

// Store is the KV contract used by the exact cache and hit counters.
type Store interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, val []byte, ttl time.Duration) error
	Del(ctx context.Context, key string) error
	Incr(ctx context.Context, key string, ttl time.Duration) (int64, error)
	Close() error
}

// ---------------------------------------------------------------------------
// MemoryStore: LRU bounded by entry count.
// ---------------------------------------------------------------------------

type memItem struct {
	key     string
	val     []byte
	expires time.Time
	prev    *memItem
	next    *memItem
}

// MemoryStore is safe for concurrent use.
type MemoryStore struct {
	mu       sync.Mutex
	capacity int
	items    map[string]*memItem
	head     *memItem // most recently used
	tail     *memItem // least recently used
	now      func() time.Time
}

// NewMemoryStore builds an LRU with the given entry capacity.
func NewMemoryStore(capacity int) *MemoryStore {
	if capacity <= 0 {
		capacity = 4096
	}
	return &MemoryStore{capacity: capacity, items: make(map[string]*memItem), now: time.Now}
}

func (m *MemoryStore) unlink(it *memItem) {
	if it.prev != nil {
		it.prev.next = it.next
	} else {
		m.head = it.next
	}
	if it.next != nil {
		it.next.prev = it.prev
	} else {
		m.tail = it.prev
	}
	it.prev, it.next = nil, nil
}

func (m *MemoryStore) pushFront(it *memItem) {
	it.next = m.head
	if m.head != nil {
		m.head.prev = it
	}
	m.head = it
	if m.tail == nil {
		m.tail = it
	}
}

// Get implements Store.
func (m *MemoryStore) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[key]
	if !ok {
		return nil, ErrCacheMiss
	}
	if m.now().After(it.expires) {
		m.unlink(it)
		delete(m.items, key)
		return nil, ErrCacheMiss
	}
	m.unlink(it)
	m.pushFront(it)
	out := make([]byte, len(it.val))
	copy(out, it.val)
	return out, nil
}

// Set implements Store.
func (m *MemoryStore) Set(_ context.Context, key string, val []byte, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = time.Minute
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if it, ok := m.items[key]; ok {
		m.unlink(it)
		delete(m.items, key)
	}
	it := &memItem{key: key, val: append([]byte(nil), val...), expires: m.now().Add(ttl)}
	m.items[key] = it
	m.pushFront(it)

	for len(m.items) > m.capacity && m.tail != nil {
		old := m.tail
		m.unlink(old)
		delete(m.items, old.key)
	}
	return nil
}

// Del implements Store.
func (m *MemoryStore) Del(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if it, ok := m.items[key]; ok {
		m.unlink(it)
		delete(m.items, key)
	}
	return nil
}

// Incr implements Store using the stored bytes as a decimal counter.
func (m *MemoryStore) Incr(_ context.Context, key string, ttl time.Duration) (int64, error) {
	if ttl <= 0 {
		ttl = time.Minute
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	it, ok := m.items[key]
	if ok && m.now().After(it.expires) {
		m.unlink(it)
		delete(m.items, key)
		ok = false
	}
	var n int64
	if ok {
		n, _ = strconv.ParseInt(string(it.val), 10, 64)
	} else {
		it = &memItem{key: key, expires: m.now().Add(ttl)}
		m.items[key] = it
		m.pushFront(it)
	}
	n++
	it.val = []byte(strconv.FormatInt(n, 10))
	return n, nil
}

// Close implements Store.
func (m *MemoryStore) Close() error { return nil }

// Len reports the live entry count (tests / metrics).
func (m *MemoryStore) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.items)
}

// ---------------------------------------------------------------------------
// RedisStore — go-redis v9. The client owns a connection pool, retries,
// timeouts, Sentinel/Cluster support and TLS; none of that is reimplemented.
// ---------------------------------------------------------------------------

// RedisOptions configures the Redis backend.
type RedisOptions struct {
	Addr         string
	Password     string
	DB           int
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	PoolSize     int
	TLS          bool
}

// RedisStore implements Store on top of go-redis.
type RedisStore struct {
	client *redis.Client
}

// NewRedisStore builds a client; options are optional (zero value is sane).
func NewRedisStore(opts RedisOptions) *RedisStore {
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:6379"
	}
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = 2 * time.Second
	}
	if opts.ReadTimeout <= 0 {
		opts.ReadTimeout = time.Second
	}
	if opts.WriteTimeout <= 0 {
		opts.WriteTimeout = time.Second
	}
	if opts.PoolSize <= 0 {
		opts.PoolSize = 64
	}
	ro := &redis.Options{
		Addr:         opts.Addr,
		Password:     opts.Password,
		DB:           opts.DB,
		DialTimeout:  opts.DialTimeout,
		ReadTimeout:  opts.ReadTimeout,
		WriteTimeout: opts.WriteTimeout,
		PoolSize:     opts.PoolSize,
		PoolTimeout:  2 * time.Second,
		MaxRetries:   2,
		MinIdleConns: 4,
	}
	if opts.TLS {
		ro.TLSConfig = nil // enabled via URL scheme redis:// vs rediss:// by caller
	}
	return &RedisStore{client: redis.NewClient(ro)}
}

// Ping verifies connectivity (startup probe).
func (s *RedisStore) Ping(ctx context.Context) error { return s.client.Ping(ctx).Err() }

// Get implements Store.
func (s *RedisStore) Get(ctx context.Context, key string) ([]byte, error) {
	b, err := s.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrCacheMiss
	}
	return b, err
}

// Set implements Store.
func (s *RedisStore) Set(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = time.Minute
	}
	return s.client.Set(ctx, key, val, ttl).Err()
}

// Del implements Store.
func (s *RedisStore) Del(ctx context.Context, key string) error {
	return s.client.Del(ctx, key).Err()
}

// Incr implements Store; INCR + EXPIRE runs in a pipeline (one round trip).
func (s *RedisStore) Incr(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	if ttl <= 0 {
		ttl = time.Minute
	}
	pipe := s.client.Pipeline()
	incr := pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	return incr.Val(), nil
}

// Close implements Store.
func (s *RedisStore) Close() error { return s.client.Close() }

// Client exposes the underlying client for health checks / metrics.
func (s *RedisStore) Client() *redis.Client { return s.client }

// ---------------------------------------------------------------------------
// Cache key: tenant-scoped and parameter-complete. Two requests share a cache
// entry only if every field below is identical.
// ---------------------------------------------------------------------------

// Key is the full identity of a cacheable request.
type Key struct {
	TenantID    string   `json:"t"`
	Model       string   `json:"m"`
	System      string   `json:"s"`
	Prompt      string   `json:"p"`
	Temperature float64  `json:"temp"`
	MaxTokens   int      `json:"max"`
	StopWords   []string `json:"stop,omitempty"`
	ToolsDigest string   `json:"tools,omitempty"`
}

func (k Key) canonical() []byte {
	sorted := append([]string(nil), k.StopWords...)
	sort.Strings(sorted)
	k.StopWords = sorted
	raw, err := json.Marshal(k)
	if err != nil {
		// Key only holds strings/numbers; marshalling cannot fail in practice.
		return []byte(fmt.Sprintf("%#v", k))
	}
	return raw
}

// Hash returns the SHA-256 of the canonical key.
func (k Key) Hash() string {
	sum := sha256.Sum256(k.canonical())
	return hex.EncodeToString(sum[:])
}

// NormalizePrompt collapses runs of whitespace and trims. It is deliberately
// conservative: no lowercasing, no punctuation stripping.
func NormalizePrompt(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// ---------------------------------------------------------------------------
// Cache
// ---------------------------------------------------------------------------

// Lookup describes how a hit was produced.
type Lookup string

const (
	LookupExact    Lookup = "exact"
	LookupSemantic Lookup = "semantic"
	LookupMiss     Lookup = "miss"
)

// CachedResponse is the value stored for a request.
type CachedResponse struct {
	Body        []byte
	TokensSaved int
}

// SemanticBackend is the seam for a real embedding-based cache (BGE-M3,
// text-embedding-3-*). It is nil by default and MUST stay nil until a real
// embedding service with tenant isolation is wired: the previous lexical
// hashing "semantic" matcher produced false hits on negations and has been
// removed entirely.
type SemanticBackend interface {
	Lookup(ctx context.Context, k Key) ([]byte, bool, error)
	Put(ctx context.Context, k Key, body []byte, ttl time.Duration) error
	Name() string
}

// Config tunes the exact cache.
type Config struct {
	ExactTTL time.Duration
	// Semantic is optional. When nil (the default) the cache is exact-only.
	Semantic SemanticBackend
	// SemanticTTL is only used when Semantic is set.
	SemanticTTL time.Duration
	// SemanticGate is evaluated per request so operators can hot-toggle the
	// semantic layer from the console without rebuilding the cache.
	SemanticGate func() bool
}

// Cache is an exact-match cache with an optional, explicitly injected
// semantic layer. It never guesses.
type Cache struct {
	exact    Store
	semantic SemanticBackend
	cfg      Config
	now      func() time.Time

	statsMu   sync.Mutex
	hitsExact int64
	hitsSem   int64
	misses    int64
}

// New builds the cache. A nil Store falls back to an in-memory LRU.
func New(exact Store, cfg Config) *Cache {
	if exact == nil {
		exact = NewMemoryStore(8192)
	}
	if cfg.ExactTTL <= 0 {
		cfg.ExactTTL = time.Hour
	}
	if cfg.SemanticTTL <= 0 {
		cfg.SemanticTTL = 30 * time.Minute
	}
	return &Cache{exact: exact, semantic: cfg.Semantic, cfg: cfg, now: time.Now}
}

// SemanticEnabled reports whether a semantic backend is wired.
func (c *Cache) SemanticEnabled() bool { return c.semantic != nil }

// Get performs an exact lookup, then (only if configured) a semantic lookup.
func (c *Cache) Get(ctx context.Context, k Key) (CachedResponse, Lookup, error) {
	k.Prompt = NormalizePrompt(k.Prompt)
	k.System = NormalizePrompt(k.System)
	key := k.Hash()

	raw, err := c.exact.Get(ctx, key)
	if err == nil {
		c.bump(&c.hitsExact)
		return CachedResponse{Body: raw, TokensSaved: estimateTokens(k.Prompt)}, LookupExact, nil
	}
	if !errors.Is(err, ErrCacheMiss) {
		return CachedResponse{}, LookupMiss, err
	}

	if c.semanticOn() {
		body, ok, serr := c.semantic.Lookup(ctx, k)
		if serr != nil {
			return CachedResponse{}, LookupMiss, serr
		}
		if ok {
			c.bump(&c.hitsSem)
			return CachedResponse{Body: body, TokensSaved: estimateTokens(k.Prompt)}, LookupSemantic, nil
		}
	}

	c.bump(&c.misses)
	return CachedResponse{}, LookupMiss, nil
}

// Put stores a response under the exact key and, if enabled, the semantic layer.
func (c *Cache) Put(ctx context.Context, k Key, body []byte, tokensSaved int) error {
	if len(body) == 0 {
		return nil
	}
	k.Prompt = NormalizePrompt(k.Prompt)
	k.System = NormalizePrompt(k.System)
	if err := c.exact.Set(ctx, k.Hash(), body, c.cfg.ExactTTL); err != nil {
		return err
	}
	if c.semanticOn() {
		return c.semantic.Put(ctx, k, body, c.cfg.SemanticTTL)
	}
	return nil
}

// Invalidate drops one entry.
func (c *Cache) Invalidate(ctx context.Context, k Key) error {
	k.Prompt = NormalizePrompt(k.Prompt)
	k.System = NormalizePrompt(k.System)
	return c.exact.Del(ctx, k.Hash())
}

// Stats returns a consistent snapshot of hit counters.
func (c *Cache) Stats() (exact, semantic, miss int64) {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	return c.hitsExact, c.hitsSem, c.misses
}

func (c *Cache) semanticOn() bool {
	if c.semantic == nil {
		return false
	}
	return c.cfg.SemanticGate == nil || c.cfg.SemanticGate()
}

func (c *Cache) bump(p *int64) {
	c.statsMu.Lock()
	*p++
	c.statsMu.Unlock()
}

// estimateTokens is a coarse token estimate (~4 chars/token) used only for
// reporting saved work, never for billing.
func estimateTokens(s string) int {
	n := len(s) / 4
	if n == 0 && len(s) > 0 {
		n = 1
	}
	return n
}
