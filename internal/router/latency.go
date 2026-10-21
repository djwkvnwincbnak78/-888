package router

import (
	"context"
	"sync"
)

// LatencyTracker is an EWMA latency provider per channel. Runes feed it from
// the request path, so the "latency" routing strategy is driven by real
// measurements instead of a nil provider.
type LatencyTracker struct {
	mu    sync.RWMutex
	stats map[uint]*latStat
	alpha float64
	min   int
}

type latStat struct {
	ewma    float64
	samples int
}

// NewLatencyTracker builds a tracker with alpha=0.2 and minSamples=3.
func NewLatencyTracker() *LatencyTracker {
	return &LatencyTracker{stats: make(map[uint]*latStat), alpha: 0.2, min: 3}
}

// Observe records one request duration in milliseconds.
func (t *LatencyTracker) Observe(channelID uint, ms float64) {
	if channelID == 0 || ms < 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.stats[channelID]
	if !ok {
		st = &latStat{ewma: ms}
		t.stats[channelID] = st
	}
	st.ewma = t.alpha*ms + (1-t.alpha)*st.ewma
	st.samples++
}

// LatencyMS implements LatencyProvider.
func (t *LatencyTracker) LatencyMS(_ context.Context, channelID uint) (float64, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	st, ok := t.stats[channelID]
	if !ok || st.samples < t.min {
		return 0, false
	}
	return st.ewma, true
}

// Snapshot returns a copy of all channel latencies (metrics/tests).
func (t *LatencyTracker) Snapshot() map[uint]float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[uint]float64, len(t.stats))
	for id, st := range t.stats {
		if st.samples >= t.min {
			out[id] = st.ewma
		}
	}
	return out
}
