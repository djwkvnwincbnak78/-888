package billing

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/djwkvnwincbnak78/nexus/internal/model"
)

// PriceRow is one model price at a specific version.
type PriceRow struct {
	Model   string
	Price   model.NUMERIC
	Version int64
}

// PriceSource loads the current price set from storage.
type PriceSource interface {
	Load(ctx context.Context) ([]PriceRow, error)
}

// PostgresPriceSource reads the latest version per model.
type PostgresPriceSource struct {
	db *sql.DB
}

// NewPostgresPriceSource wraps a database handle.
func NewPostgresPriceSource(db *sql.DB) *PostgresPriceSource {
	return &PostgresPriceSource{db: db}
}

// Load implements PriceSource.
func (s *PostgresPriceSource) Load(ctx context.Context) ([]PriceRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT ON (model_name) model_name, price_per_1m, version
		  FROM price_versions
		 ORDER BY model_name, version DESC`)
	if err != nil {
		return nil, fmt.Errorf("billing: load prices: %w", err)
	}
	defer rows.Close()

	var out []PriceRow
	for rows.Next() {
		var r PriceRow
		if err := rows.Scan(&r.Model, &r.Price, &r.Version); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SeedPrices inserts a version-1 baseline row for models that have none.
// ON CONFLICT keeps operator edits authoritative.
func SeedPrices(ctx context.Context, db *sql.DB, prices map[string]model.NUMERIC) error {
	for name, price := range prices {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO price_versions (model_name, version, price_per_1m)
			VALUES ($1, 1, $2)
			ON CONFLICT (model_name, version) DO NOTHING`, name, price); err != nil {
			return fmt.Errorf("billing: seed price %s: %w", name, err)
		}
	}
	return nil
}

// DynamicPriceTable serves prices from storage and falls back to the static
// baseline for models the operator has not priced yet. Refresh runs on a
// timer, so changing a price in price_versions takes effect without a restart.
type DynamicPriceTable struct {
	mu       sync.RWMutex
	prices   map[string]model.NUMERIC
	versions map[string]int64
	fallback map[string]model.NUMERIC
	source   PriceSource
	lastErr  error
	lastLoad time.Time
	now      func() time.Time
}

// NewDynamicPriceTable builds the table and performs the first load.
func NewDynamicPriceTable(ctx context.Context, source PriceSource, fallback map[string]model.NUMERIC) *DynamicPriceTable {
	fb := make(map[string]model.NUMERIC, len(fallback))
	for k, v := range fallback {
		fb[k] = v
	}
	t := &DynamicPriceTable{
		prices:   make(map[string]model.NUMERIC),
		versions: make(map[string]int64),
		fallback: fb,
		source:   source,
		now:      time.Now,
	}
	_ = t.Refresh(ctx)
	return t
}

// Refresh reloads prices from the source; on failure the previous set stays.
func (t *DynamicPriceTable) Refresh(ctx context.Context) error {
	if t.source == nil {
		return nil
	}
	rows, err := t.source.Load(ctx)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastLoad = t.now()
	if err != nil {
		t.lastErr = err
		return err
	}
	prices := make(map[string]model.NUMERIC, len(rows))
	versions := make(map[string]int64, len(rows))
	for _, r := range rows {
		prices[r.Model] = r.Price
		versions[r.Model] = r.Version
	}
	t.prices, t.versions, t.lastErr = prices, versions, nil
	return nil
}

// PricePer1M implements PriceTable.
func (t *DynamicPriceTable) PricePer1M(_ context.Context, modelName string) (model.NUMERIC, int64, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if p, ok := t.prices[modelName]; ok {
		return p, t.versions[modelName], nil
	}
	if p, ok := t.fallback[modelName]; ok {
		return p, 1, nil
	}
	return 0, 0, fmt.Errorf("%w: %s", ErrUnknownModel, modelName)
}

// StartRefresh reloads every interval until ctx is cancelled.
func (t *DynamicPriceTable) StartRefresh(ctx context.Context, interval time.Duration) {
	if interval <= 0 || t.source == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refreshCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				_ = t.Refresh(refreshCtx)
				cancel()
			}
		}
	}()
}

// Snapshot returns the current price set and last load metadata.
func (t *DynamicPriceTable) Snapshot() (map[string]model.NUMERIC, map[string]int64, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	prices := make(map[string]model.NUMERIC, len(t.prices))
	versions := make(map[string]int64, len(t.versions))
	for k, v := range t.prices {
		prices[k] = v
	}
	for k, v := range t.versions {
		versions[k] = v
	}
	return prices, versions, t.lastErr
}
