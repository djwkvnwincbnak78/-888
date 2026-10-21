package billing

import (
	"context"
	"errors"
	"testing"

	"github.com/djwkvnwincbnak78/nexus/internal/model"
)

type fakePriceSource struct {
	rows []PriceRow
	err  error
}

func (f *fakePriceSource) Load(context.Context) ([]PriceRow, error) {
	return f.rows, f.err
}

func TestDynamicPriceTableLoadsAndRefreshes(t *testing.T) {
	src := &fakePriceSource{rows: []PriceRow{
		{Model: "gpt-4o", Price: model.FromFloat(2.5), Version: 7},
	}}
	fallback := map[string]model.NUMERIC{"claude-3": model.FromFloat(15)}
	tbl := NewDynamicPriceTable(context.Background(), src, fallback)

	price, version, err := tbl.PricePer1M(context.Background(), "gpt-4o")
	if err != nil {
		t.Fatalf("price: %v", err)
	}
	if price != model.FromFloat(2.5) || version != 7 {
		t.Fatalf("price=%s version=%d", price.String(), version)
	}

	// Fallback for models the operator has not priced.
	price, version, err = tbl.PricePer1M(context.Background(), "claude-3")
	if err != nil || price != model.FromFloat(15) || version != 1 {
		t.Fatalf("fallback price=%s version=%d err=%v", price.String(), version, err)
	}

	if _, _, err := tbl.PricePer1M(context.Background(), "unknown"); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("unknown model err=%v", err)
	}

	// Operator updates the price: refresh must pick it up.
	src.rows = []PriceRow{{Model: "gpt-4o", Price: model.FromFloat(1.0), Version: 8}}
	if err := tbl.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	price, version, _ = tbl.PricePer1M(context.Background(), "gpt-4o")
	if price != model.FromFloat(1.0) || version != 8 {
		t.Fatalf("after refresh price=%s version=%d", price.String(), version)
	}
}

func TestDynamicPriceTableKeepsDataOnRefreshError(t *testing.T) {
	src := &fakePriceSource{rows: []PriceRow{{Model: "m", Price: model.FromFloat(3), Version: 2}}}
	tbl := NewDynamicPriceTable(context.Background(), src, nil)

	src.err = errors.New("db down")
	if err := tbl.Refresh(context.Background()); err == nil {
		t.Fatal("expected refresh error")
	}
	price, version, err := tbl.PricePer1M(context.Background(), "m")
	if err != nil || price != model.FromFloat(3) || version != 2 {
		t.Fatalf("cached price lost: %s v%d err=%v", price.String(), version, err)
	}

	prices, versions, lastErr := tbl.Snapshot()
	if lastErr == nil || prices["m"] != model.FromFloat(3) || versions["m"] != 2 {
		t.Fatalf("snapshot=%v %v %v", prices, versions, lastErr)
	}
}

func TestDynamicPriceTableWithoutSource(t *testing.T) {
	tbl := NewDynamicPriceTable(context.Background(), nil, map[string]model.NUMERIC{"m": model.FromFloat(1)})
	if err := tbl.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh with nil source: %v", err)
	}
	if _, _, err := tbl.PricePer1M(context.Background(), "m"); err != nil {
		t.Fatalf("fallback: %v", err)
	}
}

func TestLedgerUsesDynamicPriceVersion(t *testing.T) {
	src := &fakePriceSource{rows: []PriceRow{{Model: "m", Price: model.FromFloat(10), Version: 42}}}
	tbl := NewDynamicPriceTable(context.Background(), src, nil)
	l := NewLedger(tbl)

	acct, err := l.OpenAccount(context.Background(), 1, model.FromFloat(1))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	res, err := l.Reserve(context.Background(), 1, "m", 1000, 0)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if res.PriceVersion != 42 {
		t.Fatalf("reservation version=%d want 42", res.PriceVersion)
	}
	set, err := l.Settle(context.Background(), res.ID, 1000, 0)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if set.PriceVersion != 42 {
		t.Fatalf("settlement version=%d want 42", set.PriceVersion)
	}
	if acct.Balance != model.FromFloat(0.99) { // 1000 tokens * 10 / 1e6 = 0.01
		t.Fatalf("balance=%s want 0.99", acct.Balance.String())
	}
}
