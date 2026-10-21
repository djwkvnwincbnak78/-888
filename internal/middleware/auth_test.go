package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fixedPolicy(limit int, models ...string) PolicyResolver {
	return PolicyResolverFunc(func(_ context.Context, key string) (*UserConfig, error) {
		if key == "bad" {
			return nil, errors.New("unknown key")
		}
		return &UserConfig{UserID: 42, RateLimit: limit, ModelWhitelist: models}, nil
	})
}

func doRequest(l *Limiter, key, model, ip string) int {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?model="+model, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.RemoteAddr = ip + ":1234"
	rec := httptest.NewRecorder()
	l.Middleware(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})(rec, req)
	return rec.Code
}

func TestMissingAPIKeyRejected(t *testing.T) {
	l := NewLimiter(LimiterConfig{}, fixedPolicy(10))
	defer l.Close()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	l.Middleware(func(w http.ResponseWriter, _ *http.Request) {})(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", rec.Code)
	}
}

func TestUserQuotaEnforced(t *testing.T) {
	l := NewLimiter(LimiterConfig{Window: time.Minute, GlobalLimit: 1000, IPLimit: 1000, ModelLimit: 1000}, fixedPolicy(3))
	defer l.Close()

	for i := 0; i < 3; i++ {
		if code := doRequest(l, "k1", "", "10.0.0.1"); code != http.StatusOK {
			t.Fatalf("request %d status=%d want 200", i, code)
		}
	}
	if code := doRequest(l, "k1", "", "10.0.0.1"); code != http.StatusTooManyRequests {
		t.Fatalf("4th request status=%d want 429", code)
	}
}

func TestIPQuotaIsPerHost(t *testing.T) {
	l := NewLimiter(LimiterConfig{Window: time.Minute, GlobalLimit: 1000, IPLimit: 2, ModelLimit: 1000}, fixedPolicy(1000))
	defer l.Close()

	doRequest(l, "a", "", "10.0.0.1")
	doRequest(l, "a", "", "10.0.0.1")
	if code := doRequest(l, "a", "", "10.0.0.1"); code != http.StatusTooManyRequests {
		t.Fatalf("third request from same IP status=%d want 429", code)
	}
	if code := doRequest(l, "a", "", "10.0.0.2"); code != http.StatusOK {
		t.Fatalf("other IP status=%d want 200", code)
	}
}

func TestModelWhitelistFromPolicy(t *testing.T) {
	l := NewLimiter(LimiterConfig{Window: time.Minute, GlobalLimit: 1000, IPLimit: 1000, ModelLimit: 1000}, fixedPolicy(100, "gpt-4"))
	defer l.Close()

	if code := doRequest(l, "k", "gpt-4", "10.0.0.5"); code != http.StatusOK {
		t.Fatalf("allowed model status=%d want 200", code)
	}
	if code := doRequest(l, "k", "claude-3", "10.0.0.5"); code != http.StatusForbidden {
		t.Fatalf("denied model status=%d want 403", code)
	}
}

func TestIPWhitelist(t *testing.T) {
	l := NewLimiter(LimiterConfig{
		Window: time.Minute, GlobalLimit: 1000, IPLimit: 1000, ModelLimit: 1000,
		IPWhitelist: []string{"10.0.0.0/24"},
	}, fixedPolicy(100))
	defer l.Close()

	if code := doRequest(l, "k", "", "10.0.0.9"); code != http.StatusOK {
		t.Fatalf("whitelisted status=%d want 200", code)
	}
	if code := doRequest(l, "k", "", "192.168.1.9"); code != http.StatusForbidden {
		t.Fatalf("non-whitelisted status=%d want 403", code)
	}
}

func TestUnresolvableKeyFailsClosed(t *testing.T) {
	l := NewLimiter(LimiterConfig{Window: time.Minute}, fixedPolicy(10))
	defer l.Close()
	if code := doRequest(l, "bad", "", "10.0.0.1"); code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", code)
	}
}

func TestLimiterConcurrentAccess(t *testing.T) {
	l := NewLimiter(LimiterConfig{Window: time.Minute, GlobalLimit: 100000, IPLimit: 100000, ModelLimit: 100000}, fixedPolicy(100000))
	defer l.Close()

	var ok int64
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if doRequest(l, "shared", "gpt-4", "10.0.0.1") == http.StatusOK {
					atomic.AddInt64(&ok, 1)
				}
			}
		}()
	}
	wg.Wait()
	if ok != 1000 {
		t.Fatalf("allowed %d requests, want 1000", ok)
	}
}
