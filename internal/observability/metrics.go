package observability

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Metrics is a dependency-free Prometheus exposition implementation.
// It keeps labelled counters, two gauges and a latency sum/count pair.
type Metrics struct {
	start time.Time
	info  map[string]string // build_info labels

	mu       sync.RWMutex
	counters map[string]map[string]uint64 // name -> labelKey -> value
	gauges   map[string]float64
	hists    map[string]map[string]*histSeries
	latSum   float64
	latCount uint64
}

type histSeries struct {
	bounds []float64
	counts []uint64
	sum    float64
	count  uint64
}

// New builds a Metrics registry.
func New(labels map[string]string) *Metrics {
	lbls := make(map[string]string, len(labels))
	for k, v := range labels {
		lbls[k] = v
	}
	return &Metrics{
		start:    time.Now(),
		info:     lbls,
		counters: make(map[string]map[string]uint64),
		gauges:   make(map[string]float64),
		hists:    make(map[string]map[string]*histSeries),
	}
}

func escapeLabel(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return v
}

func labelKey(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(escapeLabel(labels[k]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// Inc increments a labelled counter.
func (m *Metrics) Inc(name string, labels map[string]string) {
	key := labelKey(labels)
	m.mu.Lock()
	defer m.mu.Unlock()
	series, ok := m.counters[name]
	if !ok {
		series = make(map[string]uint64)
		m.counters[name] = series
	}
	series[key]++
}

// Add increments a counter by n.
func (m *Metrics) Add(name string, labels map[string]string, n uint64) {
	key := labelKey(labels)
	m.mu.Lock()
	defer m.mu.Unlock()
	series, ok := m.counters[name]
	if !ok {
		series = make(map[string]uint64)
		m.counters[name] = series
	}
	series[key] += n
}

// SetGauge sets a gauge value.
func (m *Metrics) SetGauge(name string, v float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gauges[name] = v
}

// Histogram registers a histogram with explicit bucket upper bounds.
func (m *Metrics) Histogram(name string, bounds []float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.hists[name]; ok {
		return
	}
	sorted := append([]float64(nil), bounds...)
	sort.Float64s(sorted)
	m.hists[name] = make(map[string]*histSeries)
	m.hists[name]["__bounds"] = &histSeries{bounds: sorted, counts: make([]uint64, len(sorted))}
}

// ObserveHistogram records one observation for a labelled histogram.
func (m *Metrics) ObserveHistogram(name string, labels map[string]string, v float64) {
	key := labelKey(labels)
	m.mu.Lock()
	defer m.mu.Unlock()
	series, ok := m.hists[name]
	if !ok {
		series = make(map[string]*histSeries)
		m.hists[name] = series
	}
	def := series["__bounds"]
	if def == nil {
		def = &histSeries{bounds: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}, counts: make([]uint64, 11)}
		series["__bounds"] = def
	}
	h, ok := series[key]
	if !ok {
		h = &histSeries{bounds: def.bounds, counts: make([]uint64, len(def.bounds))}
		series[key] = h
	}
	h.count++
	h.sum += v
	for i, b := range h.bounds {
		if v <= b {
			h.counts[i]++
		}
	}
}

// ObserveLatency records one request duration in seconds.
func (m *Metrics) ObserveLatency(seconds float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.latSum += seconds
	m.latCount++
}

// WriteTo renders the Prometheus text exposition format (version 0.0.4).
// Series are sorted so output is stable across scrapes.
func (m *Metrics) WriteExposition(w io.Writer) error {
	m.mu.RLock()
	counters := make(map[string][]string, len(m.counters))
	values := make(map[string]map[string]uint64, len(m.counters))
	for name, series := range m.counters {
		keys := make([]string, 0, len(series))
		for k := range series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		counters[name] = keys
		values[name] = series
	}
	gauges := make(map[string]float64, len(m.gauges))
	for k, v := range m.gauges {
		gauges[k] = v
	}
	latSum, latCount := m.latSum, m.latCount
	histCopy := make(map[string]map[string]histSeries, len(m.hists))
	for name, series := range m.hists {
		dst := make(map[string]histSeries, len(series))
		for k, v := range series {
			cp := histSeries{bounds: v.bounds, counts: append([]uint64(nil), v.counts...), sum: v.sum, count: v.count}
			dst[k] = cp
		}
		histCopy[name] = dst
	}
	m.mu.RUnlock()

	names := make([]string, 0, len(counters))
	for name := range counters {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if _, err := fmt.Fprintf(w, "# TYPE %s counter\n", name); err != nil {
			return err
		}
		for _, key := range counters[name] {
			if _, err := fmt.Fprintf(w, "%s%s %d\n", name, key, values[name][key]); err != nil {
				return err
			}
		}
	}

	// Latency summary rendered as sum/count, the standard histogram-free form.
	if latCount > 0 {
		if _, err := fmt.Fprintf(w, "# TYPE gateway_http_request_duration_seconds summary\n"); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "gateway_http_request_duration_seconds_sum %.6f\n", latSum); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "gateway_http_request_duration_seconds_count %d\n", latCount); err != nil {
			return err
		}
	}

	// Uptime + build info.
	if _, err := fmt.Fprintf(w, "# TYPE gateway_uptime_seconds gauge\ngateway_uptime_seconds %.0f\n",
		time.Since(m.start).Seconds()); err != nil {
		return err
	}
	if len(m.info) > 0 {
		if _, err := fmt.Fprintf(w, "# TYPE gateway_build_info gauge\ngateway_build_info%s 1\n",
			labelKey(m.info)); err != nil {
			return err
		}
	}

	histNames := make([]string, 0, len(histCopy))
	for name := range histCopy {
		histNames = append(histNames, name)
	}
	sort.Strings(histNames)
	for _, name := range histNames {
		def, ok := histCopy[name]["__bounds"]
		if !ok || len(def.bounds) == 0 {
			continue
		}
		keys := make([]string, 0, len(histCopy[name]))
		for k := range histCopy[name] {
			if k != "__bounds" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		if _, err := fmt.Fprintf(w, "# TYPE %s histogram\n", name); err != nil {
			return err
		}
		for _, k := range keys {
			h := histCopy[name][k]
			for i, b := range def.bounds {
				labels := mergeLabels(k, fmt.Sprintf("%g", b))
				if _, err := fmt.Fprintf(w, "%s_bucket%s %d\n", name, labels, h.counts[i]); err != nil {
					return err
				}
			}
			labels := mergeLabels(k, "+Inf")
			if _, err := fmt.Fprintf(w, "%s_bucket%s %d\n", name, labels, h.count); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "%s_sum%s %g\n", name, k, h.sum); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "%s_count%s %d\n", name, k, h.count); err != nil {
				return err
			}
		}
	}

	gaugeNames := make([]string, 0, len(gauges))
	for name := range gauges {
		gaugeNames = append(gaugeNames, name)
	}
	sort.Strings(gaugeNames)
	for _, name := range gaugeNames {
		if _, err := fmt.Fprintf(w, "# TYPE %s gauge\n%s %s\n",
			name, name, strconv.FormatFloat(gauges[name], 'f', -1, 64)); err != nil {
			return err
		}
	}
	return nil
}

// Handler serves /metrics.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_ = m.WriteExposition(w)
	})
}

// statusRecorder captures the response status for metrics.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying writer so SSE streaming survives the
// metrics wrapper (a bare wrapper would fail the http.Flusher assertion).
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach SetWriteDeadline on the real
// ResponseWriter.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Middleware records request count, status class, rate-limit scope and latency.
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		status := strconv.Itoa(rec.status)
		m.Inc("gateway_http_requests_total", map[string]string{
			"method": r.Method,
			"path":   routeLabel(r.URL.Path),
			"status": status,
		})
		elapsed := time.Since(start).Seconds()
		m.ObserveLatency(elapsed)
		m.ObserveHistogram("gateway_http_request_duration_seconds",
			map[string]string{"path": routeLabel(r.URL.Path), "status": status}, elapsed)

		if rec.status == http.StatusTooManyRequests {
			scope := rec.Header().Get("X-RateLimit-Scope")
			if scope == "" {
				scope = "unknown"
			}
			m.Inc("gateway_rate_limited_total", map[string]string{"scope": scope})
		}
	})
}

// mergeLabels appends the le label to an existing label set.
func mergeLabels(existing, le string) string {
	if existing == "" || existing == "{}" {
		return `{le="` + le + `"}`
	}
	return existing[:len(existing)-1] + `,le="` + le + `"}`
}

// routeLabel collapses high-cardinality paths.
func routeLabel(path string) string {
	switch path {
	case "/v1/chat/completions", "/health", "/metrics":
		return path
	default:
		return "other"
	}
}
