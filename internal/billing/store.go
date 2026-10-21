package billing

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/djwkvnwincbnak78/nexus/internal/model"
)

// Persistence-level errors.
var (
	ErrConflict            = errors.New("billing: conflict")
	ErrInsufficientBalance = errors.New("billing: insufficient balance")
	ErrReservationNotFound = errors.New("billing: reservation not found")
	ErrReservationSettled  = errors.New("billing: reservation already settled")
	ErrUnknownModel        = errors.New("billing: no price for model")
)

// Account is a tenant balance. Balance is total funds, Reserved is the open
// hold, Spent is settled spend. Available = Balance - Reserved.
type Account struct {
	UserID   uint
	Balance  model.NUMERIC
	Reserved model.NUMERIC
	Spent    model.NUMERIC
}

// Available is the spendable amount.
func (a Account) Available() model.NUMERIC { return a.Balance - a.Reserved }

// Reservation is a hold created by Reserve and closed by Settle/Reconcile.
type Reservation struct {
	ID           string
	UserID       uint
	Model        string
	PricePer1M   model.NUMERIC
	PriceVersion int64
	Held         model.NUMERIC
	PromptTokens int
	OutputTokens int
	CreatedAt    time.Time
	Settled      bool
}

// Settlement is the outcome of Settle.
type Settlement struct {
	ReservationID string
	Actual        model.NUMERIC
	Refunded      model.NUMERIC
	Overrun       model.NUMERIC
	PriceVersion  int64
	// CreditExceeded is true when the actual charge pushed the balance below
	// zero. The charge still lands (never lose revenue), but it is journalled
	// as a credit violation for collection/dunning.
	CreditExceeded bool
}

// EntryKind enumerates journal entry types.
type EntryKind string

const (
	EntryReserve   EntryKind = "reserve"
	EntrySettle    EntryKind = "settle"
	EntryRefund    EntryKind = "refund"
	EntryOverrun   EntryKind = "overrun"
	EntryReconcile EntryKind = "reconcile"
	EntryCredit    EntryKind = "credit"
)

// JournalEntry is an append-only audit record.
type JournalEntry struct {
	Seq          uint64
	Kind         EntryKind
	UserID       uint
	Reservation  string
	Amount       model.NUMERIC
	PriceVersion int64
	Note         string
	At           time.Time
}

// Store is the persistence contract behind the Ledger. Every money-moving
// method must be atomic: the implementation locks the account row before
// checking availability.
type Store interface {
	// EnsureUser makes the tenant row exist (Postgres FK requirement).
	EnsureUser(ctx context.Context, userID uint, username, apiKeyHash string) error
	// OpenAccount creates the ledger account if absent and returns a snapshot.
	OpenAccount(ctx context.Context, userID uint, initial model.NUMERIC) (*Account, error)
	// Account returns a snapshot.
	Account(ctx context.Context, userID uint) (Account, error)
	// Reserve atomically holds funds or returns ErrInsufficientBalance.
	Reserve(ctx context.Context, r *Reservation) error
	// Reservation loads one reservation.
	Reservation(ctx context.Context, id string) (*Reservation, error)
	// Settle closes a hold with the actual charge.
	Settle(ctx context.Context, id string, actual model.NUMERIC, promptTokens, completionTokens int) (*Settlement, error)
	// Reconcile releases a hold (empty id = sweep older than ttl).
	Reconcile(ctx context.Context, id string, ttl time.Duration) (int, error)
	// Credit adds funds (top-up / redeem). Amount must be positive.
	Credit(ctx context.Context, userID uint, amount model.NUMERIC, note string) error
	// Journal returns entries with Seq >= fromSeq.
	Journal(ctx context.Context, fromSeq uint64) ([]JournalEntry, error)
}

// ---------------------------------------------------------------------------
// memoryStore — process-local implementation, used for tests and for
// DB-less deployments. Mutex-guarded, no goroutines.
// ---------------------------------------------------------------------------

type memoryStore struct {
	mu           sync.Mutex
	accounts     map[uint]*Account
	reservations map[string]*Reservation
	journal      []JournalEntry
	seq          uint64
	now          func() time.Time
}

func newMemoryStore(now func() time.Time) *memoryStore {
	return &memoryStore{
		accounts:     make(map[uint]*Account),
		reservations: make(map[string]*Reservation),
		now:          now,
	}
}

// EnsureUser implements Store (no-op for the in-process backend).
func (m *memoryStore) EnsureUser(context.Context, uint, string, string) error { return nil }

// OpenAccount implements Store.
func (m *memoryStore) OpenAccount(_ context.Context, userID uint, initial model.NUMERIC) (*Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a, ok := m.accounts[userID]; ok {
		return a, nil
	}
	a := &Account{UserID: userID, Balance: initial}
	m.accounts[userID] = a
	return a, nil
}

// Account implements Store.
func (m *memoryStore) Account(_ context.Context, userID uint) (Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.accounts[userID]
	if !ok {
		return Account{}, ErrInsufficientBalance
	}
	return *a, nil
}

// Reserve implements Store.
func (m *memoryStore) Reserve(_ context.Context, r *Reservation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.accounts[r.UserID]
	if !ok {
		return ErrInsufficientBalance
	}
	if a.Available() < r.Held {
		return ErrInsufficientBalance
	}
	a.Reserved += r.Held
	cp := *r
	m.reservations[r.ID] = &cp
	m.append(JournalEntry{
		Kind: EntryReserve, UserID: r.UserID, Reservation: r.ID,
		Amount: r.Held, PriceVersion: r.PriceVersion, Note: "hold estimated usage",
	})
	return nil
}

// Reservation implements Store.
func (m *memoryStore) Reservation(_ context.Context, id string) (*Reservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.reservations[id]
	if !ok {
		return nil, ErrReservationNotFound
	}
	cp := *r
	return &cp, nil
}

// Settle implements Store.
func (m *memoryStore) Settle(_ context.Context, id string, actual model.NUMERIC, promptTokens, completionTokens int) (*Settlement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.reservations[id]
	if !ok {
		return nil, ErrReservationNotFound
	}
	if r.Settled {
		return nil, ErrReservationSettled
	}
	a := m.accounts[r.UserID]

	a.Reserved -= r.Held
	if a.Reserved < 0 {
		a.Reserved = 0
	}

	out := &Settlement{ReservationID: id, Actual: actual, PriceVersion: r.PriceVersion}
	if actual <= r.Held {
		out.Refunded = r.Held - actual
		m.append(JournalEntry{Kind: EntryRefund, UserID: r.UserID, Reservation: id,
			Amount: out.Refunded, PriceVersion: r.PriceVersion, Note: "release unused hold"})
	} else {
		out.Overrun = actual - r.Held
		note := "charge overrun"
		if a.Balance-actual < 0 {
			out.CreditExceeded = true
			note = "charge overrun (credit exceeded, balance goes negative)"
		}
		m.append(JournalEntry{Kind: EntryOverrun, UserID: r.UserID, Reservation: id,
			Amount: out.Overrun, PriceVersion: r.PriceVersion, Note: note})
	}

	a.Balance -= actual
	a.Spent += actual
	r.Settled = true
	r.PromptTokens, r.OutputTokens = promptTokens, completionTokens
	m.append(JournalEntry{Kind: EntrySettle, UserID: r.UserID, Reservation: id,
		Amount: actual, PriceVersion: r.PriceVersion,
		Note: "settle prompt=" + itoa(promptTokens) + " completion=" + itoa(completionTokens)})
	return out, nil
}

// Reconcile implements Store.
func (m *memoryStore) Reconcile(_ context.Context, id string, ttl time.Duration) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	released := 0
	matched := false
	for rid, r := range m.reservations {
		if r.Settled {
			continue
		}
		if id != "" && rid != id {
			continue
		}
		matched = true
		if id == "" && now.Sub(r.CreatedAt) < ttl {
			continue
		}
		a, ok := m.accounts[r.UserID]
		if !ok {
			continue
		}
		a.Reserved -= r.Held
		if a.Reserved < 0 {
			a.Reserved = 0
		}
		r.Settled = true
		released++
		m.append(JournalEntry{Kind: EntryReconcile, UserID: r.UserID, Reservation: rid,
			Amount: r.Held, PriceVersion: r.PriceVersion, Note: "reclaimed hold"})
	}
	if id != "" && !matched {
		return 0, ErrReservationNotFound
	}
	return released, nil
}

// Credit implements Store.
func (m *memoryStore) Credit(_ context.Context, userID uint, amount model.NUMERIC, note string) error {
	if amount <= 0 {
		return ErrConflict
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.accounts[userID]
	if !ok {
		return ErrInsufficientBalance
	}
	a.Balance += amount
	m.append(JournalEntry{Kind: EntryCredit, UserID: userID, Amount: amount, Note: note})
	return nil
}

// Journal implements Store.
func (m *memoryStore) Journal(_ context.Context, fromSeq uint64) ([]JournalEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]JournalEntry, 0, len(m.journal))
	for _, e := range m.journal {
		if e.Seq >= fromSeq {
			out = append(out, e)
		}
	}
	return out, nil
}

func (m *memoryStore) append(e JournalEntry) {
	m.seq++
	e.Seq = m.seq
	e.At = m.now()
	m.journal = append(m.journal, e)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
