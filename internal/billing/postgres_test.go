package billing

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/djwkvnwincbnak78/nexus/internal/model"
)

// testStore connects to a real PostgreSQL instance. Set TEST_DB_DSN to run:
//
//	TEST_DB_DSN='postgres://postgres@127.0.0.1:55432/gateway?sslmode=disable' go test ./internal/billing/
func testStore(t *testing.T) (*PostgresStore, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("TEST_DB_DSN")
	if dsn == "" {
		t.Skip("TEST_DB_DSN not set; skipping PostgreSQL integration test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(10)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`TRUNCATE ledger_entries, ledger_reservations, ledger_accounts, semantic_cache_entries, request_logs, users RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewPostgresStore(db), db
}

func newPGTestLedger(t *testing.T) (*Ledger, *PostgresStore) {
	t.Helper()
	store, _ := testStore(t)
	ctx := context.Background()
	if err := store.EnsureUser(ctx, 1, "user-1", "hash-1"); err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	prices := NewStaticPriceTable(9, map[string]model.NUMERIC{"gpt-4": model.FromFloat(10.0)})
	return NewLedger(prices, WithStore(store)), store
}

func TestPGReserveSettleRoundTrip(t *testing.T) {
	l, _ := newPGTestLedger(t)
	ctx := context.Background()

	if _, err := l.OpenAccount(ctx, 1, model.FromFloat(1.0)); err != nil {
		t.Fatalf("open account: %v", err)
	}
	res, err := l.Reserve(ctx, 1, "gpt-4", 3000, 3000) // hold 0.06
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	acct, err := l.Balance(ctx, 1)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if acct.Reserved != model.FromFloat(0.06) {
		t.Fatalf("reserved=%s want 0.06", acct.Reserved.String())
	}

	set, err := l.Settle(ctx, res.ID, 1500, 1500) // actual 0.03
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if set.Actual != model.FromFloat(0.03) || set.Refunded != model.FromFloat(0.03) {
		t.Fatalf("settlement=%+v", set)
	}
	if set.PriceVersion != 9 {
		t.Fatalf("price version=%d want 9", set.PriceVersion)
	}

	acct, _ = l.Balance(ctx, 1)
	if acct.Reserved != 0 || acct.Balance != model.FromFloat(0.97) || acct.Spent != model.FromFloat(0.03) {
		t.Fatalf("account=%+v", acct)
	}

	entries, err := l.Journal(ctx, 0)
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("journal entries=%d want 3", len(entries))
	}

	if _, err := l.Settle(ctx, res.ID, 1, 1); !errors.Is(err, ErrReservationSettled) {
		t.Fatalf("double settle err=%v", err)
	}
}

func TestPGInsufficientBalance(t *testing.T) {
	l, _ := newPGTestLedger(t)
	ctx := context.Background()
	if _, err := l.OpenAccount(ctx, 1, model.FromFloat(0.01)); err != nil {
		t.Fatalf("open account: %v", err)
	}
	if _, err := l.Reserve(ctx, 1, "gpt-4", 3000, 3000); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("err=%v want ErrInsufficientBalance", err)
	}
}

// TestPGConcurrentReserveNeverOverdraws is the core safety property: 20
// concurrent holds of 0.06 against 1.00 must never let reserved exceed balance.
func TestPGConcurrentReserveNeverOverdraws(t *testing.T) {
	l, _ := newPGTestLedger(t)
	ctx := context.Background()
	if _, err := l.OpenAccount(ctx, 1, model.FromFloat(1.0)); err != nil {
		t.Fatalf("open account: %v", err)
	}

	const workers = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ok, rejected int

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.Reserve(ctx, 1, "gpt-4", 3000, 3000); err != nil {
				mu.Lock()
				rejected++
				mu.Unlock()
				return
			}
			mu.Lock()
			ok++
			mu.Unlock()
		}()
	}
	wg.Wait()

	if ok > 16 {
		t.Fatalf("accepted %d holds of 0.06 against 1.00: overdraw", ok)
	}
	if ok+rejected != workers {
		t.Fatalf("ok+rejected=%d want %d", ok+rejected, workers)
	}
	acct, err := l.Balance(ctx, 1)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if acct.Reserved > acct.Balance {
		t.Fatalf("reserved %s exceeds balance %s", acct.Reserved.String(), acct.Balance.String())
	}
	want := model.FromFloat(0.06 * float64(ok))
	if acct.Reserved != want {
		t.Fatalf("reserved=%s want %s", acct.Reserved.String(), want.String())
	}
}

func TestPGReconcileExpiredHold(t *testing.T) {
	l, _ := newPGTestLedger(t)
	ctx := context.Background()
	if _, err := l.OpenAccount(ctx, 1, model.FromFloat(1.0)); err != nil {
		t.Fatalf("open account: %v", err)
	}
	res, err := l.Reserve(ctx, 1, "gpt-4", 1000, 0)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	n, err := l.Reconcile(ctx, res.ID)
	if err != nil || n != 1 {
		t.Fatalf("reconcile n=%d err=%v", n, err)
	}
	acct, _ := l.Balance(ctx, 1)
	if acct.Reserved != 0 || acct.Balance != model.FromFloat(1.0) {
		t.Fatalf("account=%+v", acct)
	}
}

func TestPGUnknownReservation(t *testing.T) {
	l, _ := newPGTestLedger(t)
	if _, err := l.Settle(context.Background(), "nope", 1, 1); !errors.Is(err, ErrReservationNotFound) {
		t.Fatalf("err=%v want ErrReservationNotFound", err)
	}
}
