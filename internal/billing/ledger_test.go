package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/djwkvnwincbnak78/nexus/internal/model"
)

func newTestLedger() *Ledger {
	prices := NewStaticPriceTable(7, map[string]model.NUMERIC{
		"gpt-4": model.FromFloat(10.0), // 10 per 1M tokens
	})
	return NewLedger(prices)
}

func mustOpen(t *testing.T, l *Ledger, userID uint, initial float64) *Account {
	t.Helper()
	a, err := l.OpenAccount(context.Background(), userID, model.FromFloat(initial))
	if err != nil {
		t.Fatalf("open account: %v", err)
	}
	return a
}

func TestEstimateUsesPriceVersion(t *testing.T) {
	l := newTestLedger()
	est, err := l.Estimate(context.Background(), "gpt-4", 1000, 2000)
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	// 3000 tokens * 10 / 1e6 = 0.03
	if est.Cost != model.FromFloat(0.03) {
		t.Fatalf("cost=%s want 0.03", est.Cost.String())
	}
	if est.PriceVersion != 7 {
		t.Fatalf("version=%d want 7", est.PriceVersion)
	}
}

func TestReserveSettleRefundsUnusedHold(t *testing.T) {
	l := newTestLedger()
	acct := mustOpen(t, l, 1, 1.0)

	res, err := l.Reserve(context.Background(), 1, "gpt-4", 3000, 3000)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if acct.Reserved != model.FromFloat(0.06) {
		t.Fatalf("reserved=%s want 0.06", acct.Reserved.String())
	}
	if acct.Available() != model.FromFloat(0.94) {
		t.Fatalf("available=%s want 0.94", acct.Available().String())
	}

	set, err := l.Settle(context.Background(), res.ID, 1500, 1500)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if set.Actual != model.FromFloat(0.03) || set.Refunded != model.FromFloat(0.03) {
		t.Fatalf("actual=%s refunded=%s", set.Actual.String(), set.Refunded.String())
	}
	if acct.Reserved != 0 {
		t.Fatalf("reserved=%s want 0", acct.Reserved.String())
	}
	if acct.Balance != model.FromFloat(0.97) {
		t.Fatalf("balance=%s want 0.97", acct.Balance.String())
	}
	if acct.Spent != model.FromFloat(0.03) {
		t.Fatalf("spent=%s want 0.03", acct.Spent.String())
	}

	if _, err := l.Settle(context.Background(), res.ID, 1, 1); !errors.Is(err, ErrReservationSettled) {
		t.Fatalf("want ErrReservationSettled, got %v", err)
	}
}

func TestReserveRejectsInsufficientBalance(t *testing.T) {
	l := newTestLedger()
	mustOpen(t, l, 2, 0.01)
	if _, err := l.Reserve(context.Background(), 2, "gpt-4", 3000, 3000); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("want ErrInsufficientBalance, got %v", err)
	}
}

func TestReserveRejectsUnknownModel(t *testing.T) {
	l := newTestLedger()
	mustOpen(t, l, 3, 100)
	if _, err := l.Reserve(context.Background(), 3, "no-such-model", 10, 10); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("want ErrUnknownModel, got %v", err)
	}
}

func TestSettleOverrunChargesExtra(t *testing.T) {
	l := newTestLedger()
	acct := mustOpen(t, l, 4, 1.0)
	res, err := l.Reserve(context.Background(), 4, "gpt-4", 1000, 0) // hold 0.01
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	set, err := l.Settle(context.Background(), res.ID, 5000, 0) // actual 0.05
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if set.Overrun != model.FromFloat(0.04) {
		t.Fatalf("overrun=%s want 0.04", set.Overrun.String())
	}
	if acct.Balance != model.FromFloat(0.95) {
		t.Fatalf("balance=%s want 0.95", acct.Balance.String())
	}
}

func TestReconcileReclaimsExpiredHold(t *testing.T) {
	now := time.Unix(1700000000, 0)
	l := NewLedger(
		NewStaticPriceTable(1, map[string]model.NUMERIC{"gpt-4": model.FromFloat(10.0)}),
		WithClock(func() time.Time { return now }),
	)
	acct := mustOpen(t, l, 5, 1.0)

	if _, err := l.Reserve(context.Background(), 5, "gpt-4", 1000, 0); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if n, err := l.Reconcile(context.Background(), ""); n != 0 || err != nil {
		t.Fatalf("premature reconcile released %d, err=%v", n, err)
	}
	now = now.Add(6 * time.Minute)
	n, err := l.Reconcile(context.Background(), "")
	if err != nil || n != 1 {
		t.Fatalf("reconcile released %d, err=%v", n, err)
	}
	if acct.Reserved != 0 {
		t.Fatalf("reserved=%s want 0", acct.Reserved.String())
	}
	if acct.Balance != model.FromFloat(1.0) {
		t.Fatalf("balance=%s want 1.0", acct.Balance.String())
	}
}

func TestJournalIsAppendOnly(t *testing.T) {
	l := newTestLedger()
	mustOpen(t, l, 6, 1.0)
	res, err := l.Reserve(context.Background(), 6, "gpt-4", 1000, 0)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := l.Settle(context.Background(), res.ID, 500, 0); err != nil {
		t.Fatalf("settle: %v", err)
	}
	entries, err := l.Journal(context.Background(), 0)
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	if len(entries) != 3 { // reserve, refund, settle
		t.Fatalf("got %d journal entries, want 3", len(entries))
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].Seq <= entries[i-1].Seq {
			t.Fatal("journal sequence is not monotonic")
		}
	}
}

func TestReconcileUnknownReservation(t *testing.T) {
	l := newTestLedger()
	mustOpen(t, l, 7, 1.0)
	if _, err := l.Reconcile(context.Background(), "does-not-exist"); !errors.Is(err, ErrReservationNotFound) {
		t.Fatalf("want ErrReservationNotFound, got %v", err)
	}
}
