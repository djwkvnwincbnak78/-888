package middleware

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Context plumbing: per-user policy is injected by the auth layer, never
// hard-coded in the limiter.
// ---------------------------------------------------------------------------

type userConfigKey struct{}

// UserConfig is the per-caller policy resolved from the data layer.
type UserConfig struct {
	UserID         uint
	RateLimit      int      // requests per window for this user (<=0 disables)
	ModelWhitelist []string // empty means "all models allowed"
}

// WithUserConfig stores the resolved policy in the request context.
func WithUserConfig(ctx context.Context, cfg *UserConfig) context.Context {
	return context.WithValue(ctx, userConfigKey{}, cfg)
}

// UserConfigFrom extracts the policy previously stored by WithUserConfig.
func UserConfigFrom(ctx context.Context) (*UserConfig, bool) {
	cfg, ok := ctx.Value(userConfigKey{}).(*UserConfig)
	return cfg, ok && cfg != nil
}

// PolicyResolver is the seam to the real data layer (PostgreSQL / cache).
// The limiter never invents a whitelist; if the resolver fails we fail closed.
type PolicyResolver interface {
	Resolve(ctx context.Context, apiKey string) (*UserConfig, error)
}

// PolicyResolverFunc adapts a plain function to PolicyResolver.
type PolicyResolverFunc func(ctx context.Context, apiKey string) (*UserConfig, error)

// Resolve implements PolicyResolver.
func (f PolicyResolverFunc) Resolve(ctx context.Context, apiKey string) (*UserConfig, error) {
	return f(ctx, apiKey)
}

// ErrMissingAPIKey is returned when no credential is presented.
var ErrMissingAPIKey = errors.New("middleware: missing api key")

// ---------------------------------------------------------------------------
// Sliding-window counter (pure stdlib, no Redis). Each layer owns its own map.
// ---------------------------------------------------------------------------

type windowCounter struct {
	hits      int
	windowEnd time.Time
}

type fixedWindow struct {
	mu     sync.Mutex
	counts map[string]*windowCounter
	limit  int
	window time.Duration
	now    func() time.Time
}

func newFixedWindow(limit int, window time.Duration, now func() time.Time) *fixedWindow {
	return &fixedWindow{
		counts: make(map[string]*windowCounter),
		limit:  limit,
		window: window,
		now:    now,
	}
}

// allow increments the counter for key and reports whether the call is allowed.
// It also returns the remaining quota and the retry-after duration.
func (w *fixedWindow) allow(key string) (bool, int, time.Duration) {
	if w.limit <= 0 {
		return true, -1, 0
	}
	now := w.now()

	w.mu.Lock()
	defer w.mu.Unlock()

	c, ok := w.counts[key]
	if !ok || now.After(c.windowEnd) {
		c = &windowCounter{hits: 0, windowEnd: now.Add(w.window)}
		w.counts[key] = c
	}
	c.hits++

	if c.hits > w.limit {
		return false, 0, c.windowEnd.Sub(now)
	}
	return true, w.limit - c.hits, 0
}

// sweep drops expired buckets so the map cannot grow without bound.
func (w *fixedWindow) sweep() {
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	for k, c := range w.counts {
		if now.After(c.windowEnd) {
			delete(w.counts, k)
		}
	}
}

// ---------------------------------------------------------------------------
// Four-layer limiter
//   L1 global      – protects the whole gateway
//   L2 IP          – protects against a single noisy host
//   L3 user        – per API key quota (from UserConfig)
//   L4 model       – per (user, model) quota, prevents single-model stampede
// ---------------------------------------------------------------------------

// LimiterConfig configures the four layers.
type LimiterConfig struct {
	GlobalLimit int
	IPLimit     int
	ModelLimit  int
	Window      time.Duration
	// IPWhitelist is the trusted egress/ingress set (CIDR allowed).
	IPWhitelist []string
	// TrustProxy enables X-Forwarded-For parsing.
	TrustProxy bool
}

// Limiter is safe for concurrent use.
type Limiter struct {
	cfg      LimiterConfig
	resolver PolicyResolver

	global *fixedWindow
	ip     *fixedWindow
	model  *fixedWindow

	userMu sync.Mutex
	users  map[string]*fixedWindow

	allowNets []*net.IPNet

	stopOnce sync.Once
	stop     chan struct{}
}

// NewLimiter builds a limiter. window<=0 defaults to one minute.
func NewLimiter(cfg LimiterConfig, resolver PolicyResolver) *Limiter {
	if cfg.Window <= 0 {
		cfg.Window = time.Minute
	}
	if cfg.GlobalLimit == 0 {
		cfg.GlobalLimit = 10000
	}
	if cfg.IPLimit == 0 {
		cfg.IPLimit = 100
	}
	if cfg.ModelLimit == 0 {
		cfg.ModelLimit = 50
	}
	now := time.Now
	l := &Limiter{
		cfg:      cfg,
		resolver: resolver,
		global:   newFixedWindow(cfg.GlobalLimit, cfg.Window, now),
		ip:       newFixedWindow(cfg.IPLimit, cfg.Window, now),
		model:    newFixedWindow(cfg.ModelLimit, cfg.Window, now),
		users:    make(map[string]*fixedWindow),
		stop:     make(chan struct{}),
	}
	for _, cidr := range cfg.IPWhitelist {
		if _, n, err := net.ParseCIDR(cidr); err == nil {
			l.allowNets = append(l.allowNets, n)
		}
	}
	go l.sweeper()
	return l
}

// Close stops the background sweep goroutine.
func (l *Limiter) Close() {
	l.stopOnce.Do(func() { close(l.stop) })
}

func (l *Limiter) sweeper() {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			l.global.sweep()
			l.ip.sweep()
			l.model.sweep()
			l.userMu.Lock()
			for _, w := range l.users {
				w.sweep()
			}
			l.userMu.Unlock()
		}
	}
}

func (l *Limiter) userWindow(userID uint, limit int) *fixedWindow {
	key := strconv.FormatUint(uint64(userID), 10)
	l.userMu.Lock()
	defer l.userMu.Unlock()
	if w, ok := l.users[key]; ok {
		return w
	}
	w := newFixedWindow(limit, l.cfg.Window, time.Now)
	l.users[key] = w
	return w
}

// ClientIP resolves the peer address, honouring X-Forwarded-For when trusted.
func (l *Limiter) ClientIP(r *http.Request) string {
	if l.cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := strings.TrimSpace(parts[0]); net.ParseIP(ip) != nil {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// IPAllowed reports whether ip passes the whitelist. An empty whitelist means
// "no IP restriction configured" and allows everything.
func (l *Limiter) IPAllowed(ipStr string) bool {
	if len(l.allowNets) == 0 {
		return true
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, n := range l.allowNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func modelAllowed(list []string, model string) bool {
	if len(list) == 0 || model == "" {
		return true
	}
	for _, m := range list {
		if m == model {
			return true
		}
	}
	return false
}

func writeLimited(w http.ResponseWriter, scope string, retry time.Duration, remaining int) {
	if retry < time.Second {
		retry = time.Second
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())))
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	w.Header().Set("X-RateLimit-Scope", scope)
	http.Error(w, "rate limit exceeded: "+scope, http.StatusTooManyRequests)
}

// Middleware enforces the four layers in order: global -> IP -> user -> model.
func (l *Limiter) Middleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		apiKey := r.Header.Get("Authorization")
		apiKey = strings.TrimSpace(strings.TrimPrefix(apiKey, "Bearer "))
		if apiKey == "" {
			apiKey = strings.TrimSpace(r.Header.Get("X-API-Key"))
		}
		if apiKey == "" {
			http.Error(w, ErrMissingAPIKey.Error(), http.StatusUnauthorized)
			return
		}

		clientIP := l.ClientIP(r)
		if !l.IPAllowed(clientIP) {
			http.Error(w, "ip not allowed", http.StatusForbidden)
			return
		}

		// L1 global
		if ok, _, retry := l.global.allow("global"); !ok {
			writeLimited(w, "global", retry, 0)
			return
		}
		// L2 per-IP
		if ok, _, retry := l.ip.allow(clientIP); !ok {
			writeLimited(w, "ip", retry, 0)
			return
		}

		// Resolve policy for this caller.
		var cfg *UserConfig
		if c, ok := UserConfigFrom(ctx); ok {
			cfg = c
		} else if l.resolver != nil {
			c, err := l.resolver.Resolve(ctx, apiKey)
			if err != nil || c == nil {
				// Fail closed: unknown caller cannot be authorized.
				http.Error(w, "unable to resolve api key policy", http.StatusUnauthorized)
				return
			}
			cfg = c
			ctx = WithUserConfig(ctx, cfg)
			r = r.WithContext(ctx)
		} else {
			http.Error(w, "no policy resolver configured", http.StatusInternalServerError)
			return
		}

		// L3 per-user
		if cfg.RateLimit > 0 {
			if ok, remaining, retry := l.userWindow(cfg.UserID, cfg.RateLimit).allow(apiKey); !ok {
				writeLimited(w, "user", retry, remaining)
				return
			}
		}

		// L4 per-model, scoped to the user so one tenant cannot starve another.
		modelName := r.URL.Query().Get("model")
		if modelName != "" {
			if !modelAllowed(cfg.ModelWhitelist, modelName) {
				http.Error(w, "model not allowed for this key", http.StatusForbidden)
				return
			}
			key := strconv.FormatUint(uint64(cfg.UserID), 10) + "|" + modelName
			if ok, remaining, retry := l.model.allow(key); !ok {
				writeLimited(w, "model", retry, remaining)
				return
			}
		}

		next(w, r)
	}
}
