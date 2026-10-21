package router

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/djwkvnwincbnak78/nexus/internal/model"
)

// Strategy is the routing policy requested by the caller.
type Strategy string

const (
	StrategyFixed    Strategy = "fixed"    // pinned channel wins
	StrategyCost     Strategy = "cost"     // lowest price_per_1m
	StrategyLatency  Strategy = "latency"  // lowest observed p95 latency
	StrategyWeighted Strategy = "weighted" // weight-proportional random
)

// Route reason codes returned in every decision (stable enum, safe for logs).
const (
	ReasonFixedPinned      = "fixed_pinned"
	ReasonCostOptimized    = "cost_optimized"
	ReasonLatencyPreferred = "latency_preferred"
	ReasonWeightedRandom   = "weighted_random_selected"
	ReasonFallbackHealthy  = "fallback_healthy_channel"
	ReasonNoCandidate      = "no_available_channel"
	ReasonAllBreakersOpen  = "all_circuit_breakers_open"
)

// Errors surfaced to the HTTP layer.
var (
	ErrNoCandidate     = errors.New("router: no candidate channel for model")
	ErrAllBreakersOpen = errors.New("router: all channels are circuit-broken")
	ErrUnknownStrategy = errors.New("router: unknown strategy")
)

// Candidate is one concrete (model, channel) pair offered to the router.
// Price and weight come from model.ModelChannel / model.Channel — nothing is
// invented here.
type Candidate struct {
	Model     *model.ModelChannel
	Channel   *model.Channel
	Pinned    bool    // operator pin for fixed routing
	LatencyMS float64 // observed p95, 0 means unknown
	Healthy   bool    // last health probe result
}

// Decision is the routing outcome and always carries a reason code.
type Decision struct {
	Channel   *model.Channel
	Model     *model.ModelChannel
	Reason    string
	Strategy  Strategy
	CostPer1M model.NUMERIC
	LatencyMS float64
	Weight    int
	Depth     int // failover attempts consumed
}

// LatencyProvider supplies observed latency for a channel.
type LatencyProvider interface {
	LatencyMS(ctx context.Context, channelID uint) (float64, bool)
}

// breaker is a consecutive-failure circuit breaker, safe for concurrent use.
type breaker struct {
	mu        sync.Mutex
	failures  int
	threshold int
	openUntil time.Time
	coolDown  time.Duration
}

func newBreaker(threshold int, coolDown time.Duration) *breaker {
	return &breaker{threshold: threshold, coolDown: coolDown}
}

func (b *breaker) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.IsZero() {
		return true
	}
	if now.After(b.openUntil) {
		// half-open: let one probe through
		b.openUntil = time.Time{}
		b.failures = 0
		return true
	}
	return false
}

func (b *breaker) success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.openUntil = time.Time{}
}

func (b *breaker) failure(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	if b.failures >= b.threshold {
		b.openUntil = now.Add(b.coolDown)
	}
}

// Router holds breaker state and the RNG. Zero value is not usable; use NewRouter.
type Router struct {
	mu        sync.RWMutex
	breakers  map[uint]*breaker
	latency   LatencyProvider
	threshold int
	coolDown  time.Duration
	shared    SharedBreaker

	rndMu sync.Mutex
	rnd   *rand.Rand
	now   func() time.Time
}

// NewRouter builds a router with the given latency provider (may be nil).
func NewRouter(lat LatencyProvider) *Router { return NewRouterWithShared(lat, nil) }

// NewRouterWithShared adds a cross-process breaker (Redis). When nil, only the
// in-process breaker is used.
func NewRouterWithShared(lat LatencyProvider, shared SharedBreaker) *Router {
	return &Router{
		breakers:  make(map[uint]*breaker),
		latency:   lat,
		threshold: 5,
		coolDown:  30 * time.Second,
		shared:    shared,
		rnd:       rand.New(rand.NewSource(time.Now().UnixNano())),
		now:       time.Now,
	}
}

func (r *Router) breakerFor(channelID uint) *breaker {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.breakers[channelID]
	if !ok {
		b = newBreaker(r.threshold, r.coolDown)
		r.breakers[channelID] = b
	}
	return b
}

// Select picks a channel for modelName under the requested strategy.
// It never fabricates a channel: an empty candidate list is an error.
func (r *Router) Select(ctx context.Context, modelName string, candidates []Candidate, strategy Strategy) (*Decision, error) {
	if len(candidates) == 0 {
		return nil, ErrNoCandidate
	}
	now := r.now()

	// Filter: must serve the model, be healthy, and have a closed breaker.
	eligible := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if c.Channel == nil || c.Model == nil {
			continue
		}
		if c.Model.ModelName != modelName {
			continue
		}
		if c.Model.ChannelID != 0 && c.Channel.ID != c.Model.ChannelID {
			continue
		}
		if !c.Healthy {
			continue
		}
		if !r.breakerFor(c.Channel.ID).allow(now) {
			continue
		}
		if r.shared != nil {
			if open, err := r.shared.Open(ctx, c.Channel.ID); err == nil && open {
				continue
			}
		}
		if c.LatencyMS == 0 && r.latency != nil {
			if ms, ok := r.latency.LatencyMS(ctx, c.Channel.ID); ok {
				c.LatencyMS = ms
			}
		}
		eligible = append(eligible, c)
	}
	if len(eligible) == 0 {
		return nil, ErrAllBreakersOpen
	}

	switch strategy {
	case StrategyFixed:
		for _, c := range eligible {
			if c.Pinned {
				return r.decide(c, ReasonFixedPinned, strategy, 0), nil
			}
		}
		return r.weighted(eligible, strategy, ReasonFallbackHealthy)

	case StrategyCost:
		best := eligible[0]
		for _, c := range eligible[1:] {
			if c.Model.PricePer1M < best.Model.PricePer1M {
				best = c
			}
		}
		return r.decide(best, ReasonCostOptimized, strategy, 0), nil

	case StrategyLatency:
		withLatency := eligible[:0:0]
		for _, c := range eligible {
			if c.LatencyMS > 0 {
				withLatency = append(withLatency, c)
			}
		}
		if len(withLatency) == 0 {
			// No measurement yet: degrade to cost, and say so.
			best := eligible[0]
			for _, c := range eligible[1:] {
				if c.Model.PricePer1M < best.Model.PricePer1M {
					best = c
				}
			}
			d := r.decide(best, ReasonCostOptimized, StrategyCost, 0)
			d.Reason = "latency_unmeasured_" + ReasonCostOptimized
			return d, nil
		}
		best := withLatency[0]
		for _, c := range withLatency[1:] {
			if c.LatencyMS < best.LatencyMS {
				best = c
			}
		}
		return r.decide(best, ReasonLatencyPreferred, strategy, 0), nil

	case StrategyWeighted, "":
		return r.weighted(eligible, StrategyWeighted, ReasonWeightedRandom)

	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownStrategy, strategy)
	}
}

func (r *Router) weighted(list []Candidate, strategy Strategy, reason string) (*Decision, error) {
	total := 0
	for _, c := range list {
		if c.Channel.Weight > 0 {
			total += c.Channel.Weight
		}
	}
	if total <= 0 {
		return r.decide(list[0], ReasonFallbackHealthy, strategy, 0), nil
	}

	r.rndMu.Lock()
	pick := r.rnd.Intn(total)
	r.rndMu.Unlock()

	acc := 0
	for _, c := range list {
		if c.Channel.Weight <= 0 {
			continue
		}
		acc += c.Channel.Weight
		if pick < acc {
			return r.decide(c, reason, strategy, 0), nil
		}
	}
	return r.decide(list[len(list)-1], ReasonFallbackHealthy, strategy, 0), nil
}

func (r *Router) decide(c Candidate, reason string, strategy Strategy, depth int) *Decision {
	return &Decision{
		Channel:   c.Channel,
		Model:     c.Model,
		Reason:    reason,
		Strategy:  strategy,
		CostPer1M: c.Model.PricePer1M,
		LatencyMS: c.LatencyMS,
		Weight:    c.Channel.Weight,
		Depth:     depth,
	}
}

// Outcome classifies a provider error for failover purposes.
type Outcome int

const (
	OutcomeSuccess     Outcome = iota
	OutcomeRateLimited         // 429: queue and retry the SAME channel
	OutcomeServerError         // 5xx: trip the breaker, move to another channel
	OutcomeClientError         // 4xx: do not retry, do not fail over
)

// Classify maps an HTTP status code to a failover outcome.
func Classify(status int) Outcome {
	switch {
	case status >= 200 && status < 300:
		return OutcomeSuccess
	case status == 429:
		return OutcomeRateLimited
	case status >= 500:
		return OutcomeServerError
	default:
		return OutcomeClientError
	}
}

// Report feeds a result back into the breaker for a channel.
func (r *Router) Report(channelID uint, outcome Outcome) {
	r.ReportContext(context.Background(), channelID, outcome)
}

// ReportContext is Report with a caller context, used to bound Redis calls.
func (r *Router) ReportContext(ctx context.Context, channelID uint, outcome Outcome) {
	b := r.breakerFor(channelID)
	switch outcome {
	case OutcomeSuccess:
		b.success()
	case OutcomeServerError:
		b.failure(r.now())
	}
	// 429 and 4xx carry no breaker penalty: 429 is congestion, not failure.
	if r.shared == nil {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	switch outcome {
	case OutcomeSuccess:
		_ = r.shared.RecordSuccess(callCtx, channelID)
	case OutcomeServerError:
		_ = r.shared.RecordFailure(callCtx, channelID)
	}
}

// Failover selects a channel and, on server errors, walks the remaining
// candidates with the breaker tripped, preserving the reason chain.
func (r *Router) Failover(ctx context.Context, modelName string, candidates []Candidate, strategy Strategy, maxAttempts int, attempt func(*Decision) (Outcome, error)) (*Decision, error) {
	if maxAttempts <= 0 {
		maxAttempts = len(candidates)
	}
	tried := make(map[uint]bool, len(candidates))
	remaining := candidates
	var lastErr error
	var last *Decision

	for depth := 0; depth < maxAttempts && len(remaining) > 0; depth++ {
		pool := make([]Candidate, 0, len(remaining))
		for _, c := range remaining {
			if c.Channel == nil || tried[c.Channel.ID] {
				continue
			}
			pool = append(pool, c)
		}
		if len(pool) == 0 {
			break
		}

		d, err := r.Select(ctx, modelName, pool, strategy)
		if err != nil {
			return last, err
		}
		d.Depth = depth
		last = d

		outcome, err := attempt(d)
		tried[d.Channel.ID] = true
		r.Report(d.Channel.ID, outcome)
		if outcome == OutcomeSuccess {
			return d, nil
		}
		lastErr = err
		if outcome == OutcomeClientError {
			return d, err
		}
		if outcome == OutcomeRateLimited {
			// Same channel is congested: retrying it is the correct move, but
			// only once per depth to avoid a hot loop.
			continue
		}
		remaining = pool
	}
	if lastErr == nil {
		lastErr = ErrNoCandidate
	}
	return last, lastErr
}
