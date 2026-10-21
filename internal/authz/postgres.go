package authz

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/djwkvnwincbnak78/nexus/internal/model"
)

// PostgresStore persists RBAC and commerce state.
type PostgresStore struct {
	db *sql.DB
}

// NewPostgresStore wraps a database handle.
func NewPostgresStore(db *sql.DB) *PostgresStore { return &PostgresStore{db: db} }

// ResolveAdminKey implements Store.
func (s *PostgresStore) ResolveAdminKey(ctx context.Context, keyHash string) (*Principal, error) {
	var (
		tenantID uint
		role     string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT tenant_id, role FROM admin_api_keys
		  WHERE key_hash = $1 AND revoked_at IS NULL`, keyHash).Scan(&tenantID, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	id := keyHash
	if len(id) > 8 {
		id = id[:8]
	}
	return &Principal{TenantID: tenantID, Role: Role(role), KeyID: id}, nil
}

// EnsureAdminKey implements Store.
func (s *PostgresStore) EnsureAdminKey(ctx context.Context, keyHash string, tenantID uint, role Role) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO admin_api_keys (tenant_id, key_hash, role) VALUES ($1, $2, $3)
		 ON CONFLICT (key_hash) DO UPDATE SET role = EXCLUDED.role, revoked_at = NULL`,
		tenantID, keyHash, string(role))
	if err != nil {
		return fmt.Errorf("authz: ensure admin key: %w", err)
	}
	return nil
}

// CreateOrder implements Store.
func (s *PostgresStore) CreateOrder(ctx context.Context, tenantID uint, amount model.NUMERIC, currency string) (*Order, error) {
	if amount <= 0 {
		return nil, ErrConflict
	}
	if currency == "" {
		currency = "USD"
	}
	o := &Order{ID: newID("ord_"), TenantID: tenantID, Amount: amount, Currency: currency, Status: "pending"}
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO orders (id, tenant_id, amount, currency, status)
		 VALUES ($1, $2, $3, $4, 'pending')
		 RETURNING created_at`,
		o.ID, o.TenantID, o.Amount, o.Currency).Scan(&o.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("authz: create order: %w", err)
	}
	return o, nil
}

// GetOrder implements Store.
func (s *PostgresStore) GetOrder(ctx context.Context, tenantID uint, id string) (*Order, error) {
	o := &Order{ID: id}
	var paidAt sql.NullTime
	err := s.db.QueryRowContext(ctx,
		`SELECT tenant_id, amount, currency, status, created_at, paid_at
		   FROM orders WHERE id = $1 AND tenant_id = $2`, id, tenantID).
		Scan(&o.TenantID, &o.Amount, &o.Currency, &o.Status, &o.CreatedAt, &paidAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if paidAt.Valid {
		t := paidAt.Time
		o.PaidAt = &t
	}
	return o, nil
}

// MarkOrderPaid implements Store.
func (s *PostgresStore) MarkOrderPaid(ctx context.Context, tenantID uint, id string) (*Order, error) {
	o := &Order{ID: id}
	var paidAt sql.NullTime
	err := s.db.QueryRowContext(ctx,
		`UPDATE orders SET status = 'paid', paid_at = now()
		  WHERE id = $1 AND tenant_id = $2 AND status = 'pending'
		  RETURNING tenant_id, amount, currency, status, created_at, paid_at`,
		id, tenantID).
		Scan(&o.TenantID, &o.Amount, &o.Currency, &o.Status, &o.CreatedAt, &paidAt)
	if errors.Is(err, sql.ErrNoRows) {
		// Either missing or already paid; distinguish for a useful status code.
		if _, gerr := s.GetOrder(ctx, tenantID, id); gerr == nil {
			return nil, ErrConflict
		}
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if paidAt.Valid {
		t := paidAt.Time
		o.PaidAt = &t
	}
	return o, nil
}

// CreateRedeemCodes implements Store.
func (s *PostgresStore) CreateRedeemCodes(ctx context.Context, tenantID uint, amount model.NUMERIC, count int) ([]RedeemCode, error) {
	if amount <= 0 || count <= 0 || count > 1000 {
		return nil, ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	out := make([]RedeemCode, 0, count)
	for i := 0; i < count; i++ {
		c := RedeemCode{Code: newID("rc_"), TenantID: tenantID, Amount: amount, Status: "active"}
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO redeem_codes (code, tenant_id, amount, status)
			 VALUES ($1, $2, $3, 'active') RETURNING created_at`,
			c.Code, c.TenantID, c.Amount).Scan(&c.CreatedAt); err != nil {
			return nil, fmt.Errorf("authz: insert redeem code: %w", err)
		}
		out = append(out, c)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// ClaimRedeem implements Store.
func (s *PostgresStore) ClaimRedeem(ctx context.Context, tenantID, userID uint, code string) (*RedeemCode, error) {
	c := &RedeemCode{Code: code}
	var claimedAt sql.NullTime
	var claimedBy sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`UPDATE redeem_codes
		    SET status = 'claimed', claimed_at = now(), claimed_by = $3
		  WHERE code = $1 AND tenant_id = $2 AND status = 'active'
		  RETURNING tenant_id, amount, status, created_at, claimed_at, claimed_by`,
		code, tenantID, userID).
		Scan(&c.TenantID, &c.Amount, &c.Status, &c.CreatedAt, &claimedAt, &claimedBy)
	if errors.Is(err, sql.ErrNoRows) {
		var status string
		gerr := s.db.QueryRowContext(ctx,
			`SELECT status FROM redeem_codes WHERE code = $1 AND tenant_id = $2`, code, tenantID).Scan(&status)
		if errors.Is(gerr, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		if gerr != nil {
			return nil, gerr
		}
		return nil, ErrConflict
	}
	if err != nil {
		return nil, err
	}
	if claimedAt.Valid {
		t := claimedAt.Time
		c.ClaimedAt = &t
	}
	if claimedBy.Valid {
		c.ClaimedBy = uint(claimedBy.Int64)
	}
	return c, nil
}

var _ = time.Now
