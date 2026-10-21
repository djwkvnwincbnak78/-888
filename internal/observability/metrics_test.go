package observability

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExpositionFormat(t *testing.T) {
	m := New(map[string]string{"version": "9.9.9"})
	m.Inc("gateway_http_requests_total", map[string]string{"path": "/v1/chat/completions", "status": "200"})
	m.Inc("gateway_http_requests_total", map[string]string{"path": "/v1/chat/completions", "status": "200"})
	m.Inc("gateway_route_decisions_total", map[string]string{"reason": "cost_optimized"})
	m.SetGauge("gateway_inflight_requests", 3)
	m.ObserveLatency(0.25)

	var buf bytes.Buffer
	if err := m.WriteExposition(&buf); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"# TYPE gateway_http_requests_total counter",
		`gateway_http_requests_total{path="/v1/chat/completions",status="200"} 2`,
		`gateway_route_decisions_total{reason="cost_optimized"} 1`,
		"gateway_http_request_duration_seconds_sum 0.250000",
		"gateway_http_request_duration_seconds_count 1",
		`gateway_build_info{version="9.9.9"} 1`,
		"gateway_inflight_requests 3",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in exposition:\n%s", want, out)
		}
	}
}

func TestLabelEscaping(t *testing.T) {
	m := New(nil)
	m.Inc("x_total", map[string]string{"reason": `a"b\c`})
	var buf bytes.Buffer
	_ = m.WriteExposition(&buf)
	if !strings.Contains(buf.String(), `x_total{reason="a\"b\\c"} 1`) {
		t.Fatalf("escaping wrong:\n%s", buf.String())
	}
}

func TestMiddlewareRecordsStatusAndRateLimitScope(t *testing.T) {
	m := New(nil)
	h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Scope", "user")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	var buf bytes.Buffer
	_ = m.WriteExposition(&buf)
	out := buf.String()
	if !strings.Contains(out, `gateway_http_requests_total{method="POST",path="/v1/chat/completions",status="429"} 1`) {
		t.Fatalf("status counter missing:\n%s", out)
	}
	if !strings.Contains(out, `gateway_rate_limited_total{scope="user"} 1`) {
		t.Fatalf("rate limit counter missing:\n%s", out)
	}
}

func TestHandlerContentType(t *testing.T) {
	m := New(nil)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Fatalf("content-type=%q", ct)
	}
}

func TestRouteLabelCollapsesUnknownPaths(t *testing.T) {
	if got := routeLabel("/v1/chat/completions"); got != "/v1/chat/completions" {
		t.Fatalf("got %q", got)
	}
	if got := routeLabel("/admin/secret"); got != "other" {
		t.Fatalf("got %q want other", got)
	}
}

func TestStatusRecorderSupportsFlushAndUnwrap(t *testing.T) {
	m := New(nil)
	inner := &flushSpy{header: http.Header{}}
	h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("metrics wrapper broke http.Flusher")
		}
		f.Flush()
		if _, ok := w.(interface{ Unwrap() http.ResponseWriter }); !ok {
			t.Fatal("metrics wrapper lost Unwrap")
		}
	}))
	h.ServeHTTP(inner, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if !inner.flushed {
		t.Fatal("Flush did not reach the underlying writer")
	}
}

type flushSpy struct {
	header  http.Header
	flushed bool
}

func (f *flushSpy) Header() http.Header         { return f.header }
func (f *flushSpy) Write(b []byte) (int, error) { return len(b), nil }
func (f *flushSpy) WriteHeader(int)             {}
func (f *flushSpy) Flush()                      { f.flushed = true }

func TestHistogramExposition(t *testing.T) {
	m := New(nil)
	m.Histogram("gateway_latency_seconds", []float64{0.1, 0.5, 1})
	m.ObserveHistogram("gateway_latency_seconds", map[string]string{"path": "/v1/chat/completions"}, 0.2)
	m.ObserveHistogram("gateway_latency_seconds", map[string]string{"path": "/v1/chat/completions"}, 2.0)

	var buf bytes.Buffer
	if err := m.WriteExposition(&buf); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`# TYPE gateway_latency_seconds histogram`,
		`gateway_latency_seconds_bucket{path="/v1/chat/completions",le="0.1"} 0`,
		`gateway_latency_seconds_bucket{path="/v1/chat/completions",le="0.5"} 1`,
		`gateway_latency_seconds_bucket{path="/v1/chat/completions",le="+Inf"} 2`,
		`gateway_latency_seconds_sum{path="/v1/chat/completions"} 2.2`,
		`gateway_latency_seconds_count{path="/v1/chat/completions"} 2`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}
