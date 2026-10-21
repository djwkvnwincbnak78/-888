package billing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/djwkvnwincbnak78/nexus/internal/model"
)

// PriceTable resolves the per-1M-token price and the price version in force.
type PriceTable interface {
	PricePer1M(ctx context.Context, modelName string) (price model.NUMERIC, version int64, err error)
}

// StaticPriceTable is a deterministic in-memory price table.
type StaticPriceTable struct {
	mu     sync.RWMutex
	prices map[string]model.NUMERIC
	epoch  int64
}

// NewStaticPriceTable builds a table with the given epoch (price version).
func NewStaticPriceTable(epoch int64, prices map[string]model.NUMERIC) *StaticPriceTable {
	cp := make(map[string]model.NUMERIC, len(prices))
	for k, v := range prices {
		cp[k] = v
	}
	return &StaticPriceTable{prices: cp, epoch: epoch}
}

// PricePer1M implements PriceTable.
func (t *StaticPriceTable) PricePer1M(_ context.Context, modelName string) (model.NUMERIC, int64, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	p, ok := t.prices[modelName]
	if !ok {
		return 0, 0, fmt.Errorf("%w: %s", ErrUnknownModel, modelName)
	}
	return p, t.epoch, nil
}

// Set updates a price and bumps the version.
func (t *StaticPriceTable) Set(modelName string, price model.NUMERIC) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.prices[modelName] = price
	t.epoch++
	return t.epoch
}

// Estimate is the pre-flight quote for a request.
type Estimate struct {
	Model        string
	PricePer1M   model.NUMERIC
	PriceVersion int64
	PromptTokens int
	OutputTokens int
	Cost         model.NUMERIC
}

// Ledger implements estimate -> reserve -> usage -> reconcile. Persistence is
// delegated to a Store so the same arithmetic runs against memory or Postgres.
type Ledger struct {
	prices  PriceTable
	store   Store
	now     func() time.Time
	holdTTL time.Duration
	seq     atomic.Uint64
}

// Option customizes the Ledger.
type Option func(*Ledger)

// WithStore injects a persistence backend.
func WithStore(s Store) Option { return func(l *Ledger) { l.store = s } }

// WithClock injects a clock (tests).
func WithClock(f func() time.Time) Option { return func(l *Ledger) { l.now = f } }

// WithHoldTTL sets how long an unsettled hold survives before reconcile sweeps it.
func WithHoldTTL(d time.Duration) Option { return func(l *Ledger) { l.holdTTL = d } }

// NewLedger builds a ledger. Without WithStore it uses the in-memory backend.
func NewLedger(prices PriceTable, opts ...Option) *Ledger {
	l := &Ledger{prices: prices, now: time.Now, holdTTL: 5 * time.Minute}
	for _, o := range opts {
		o(l)
	}
	if l.store == nil {
		l.store = newMemoryStore(l.now)
	}
	return l
}

// Store exposes the persistence backend (health checks, admin).
func (l *Ledger) Store() Store { return l.store }

// OpenAccount creates the account if absent and returns a snapshot.
func (l *Ledger) OpenAccount(ctx context.Context, userID uint, initial model.NUMERIC) (*Account, error) {
	return l.store.OpenAccount(ctx, userID, initial)
}

// Balance returns the account snapshot.
func (l *Ledger) Balance(ctx context.Context, userID uint) (Account, error) {
	return l.store.Account(ctx, userID)
}

// Estimate quotes the cost of a request at the current price version.
func (l *Ledger) Estimate(ctx context.Context, modelName string, promptTokens, outputTokens int) (*Estimate, error) {
	if l.prices == nil {
		return nil, ErrUnknownModel
	}
	price, version, err := l.prices.PricePer1M(ctx, modelName)
	if err != nil {
		return nil, err
	}
	return &Estimate{
		Model:        modelName,
		PricePer1M:   price,
		PriceVersion: version,
		PromptTokens: promptTokens,
		OutputTokens: outputTokens,
		Cost:         Cost(price, promptTokens+outputTokens),
	}, nil
}

// Reserve holds estimated funds. The store performs the atomic check.
func (l *Ledger) Reserve(ctx context.Context, userID uint, modelName string, promptTokens, outputTokens int) (*Reservation, error) {
	est, err := l.Estimate(ctx, modelName, promptTokens, outputTokens)
	if err != nil {
		return nil, err
	}
	r := &Reservation{
		ID:           l.newReservationID(userID),
		UserID:       userID,
		Model:        modelName,
		PricePer1M:   est.PricePer1M,
		PriceVersion: est.PriceVersion,
		Held:         est.Cost,
		PromptTokens: promptTokens,
		OutputTokens: outputTokens,
		CreatedAt:    l.now(),
	}
	if err := l.store.Reserve(ctx, r); err != nil {
		return nil, err
	}
	return r, nil
}

// Settle replaces the hold with actual usage at the pinned price version.
func (l *Ledger) Settle(ctx context.Context, reservationID string, promptTokens, completionTokens int) (*Settlement, error) {
	r, err := l.store.Reservation(ctx, reservationID)
	if err != nil {
		return nil, err
	}
	if r.Settled {
		return nil, ErrReservationSettled
	}
	actual := Cost(r.PricePer1M, promptTokens+completionTokens)
	return l.store.Settle(ctx, reservationID, actual, promptTokens, completionTokens)
}

// Credit adds funds to an account (order payment / redeem code).
func (l *Ledger) Credit(ctx context.Context, userID uint, amount model.NUMERIC, note string) error {
	return l.store.Credit(ctx, userID, amount, note)
}

// Reconcile releases a hold. An empty reservationID sweeps holds older than ttl.
func (l *Ledger) Reconcile(ctx context.Context, reservationID string) (int, error) {
	return l.store.Reconcile(ctx, reservationID, l.holdTTL)
}

// Journal returns the append-only audit trail from a sequence number.
func (l *Ledger) Journal(ctx context.Context, fromSeq uint64) ([]JournalEntry, error) {
	return l.store.Journal(ctx, fromSeq)
}

func (l *Ledger) newReservationID(userID uint) string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is fatal for auditability; fall back to a
		// monotonic counter so IDs stay unique within the process.
		return fmt.Sprintf("res_%d_%d", userID, l.seq.Add(1))
	}
	return fmt.Sprintf("res_%d_%s", userID, hex.EncodeToString(b[:]))
}

// Cost computes price*tokens/1e6 rounded half-up with big.Int, so large
// prompts cannot overflow int64 (price is scaled by 1e10).
func Cost(price model.NUMERIC, tokens int) model.NUMERIC {
	if tokens <= 0 || price <= 0 {
		return 0
	}
	n := new(big.Int).Mul(big.NewInt(int64(price)), big.NewInt(int64(tokens)))
	d := big.NewInt(1_000_000)
	n.Add(n, new(big.Int).Div(d, big.NewInt(2))) // half-up
	return model.NUMERIC(new(big.Int).Div(n, d).Int64())
}
