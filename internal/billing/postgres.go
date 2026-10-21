package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/djwkvnwincbnak78/nexus/internal/model"
)

// PostgresStore persists the ledger in PostgreSQL. Every money-moving method
// runs in a transaction and locks the account row with SELECT ... FOR UPDATE
// before checking availability, so concurrent reserves cannot overdraw.
type PostgresStore struct {
	db  *sql.DB
	now func() time.Time
}

// NewPostgresStore wraps a database handle (pgx stdlib driver).
func NewPostgresStore(db *sql.DB) *PostgresStore {
	return &PostgresStore{db: db, now: time.Now}
}

// EnsureUser makes the tenant row exist so ledger FKs hold. The API key is
// stored only as its SHA-256 hex digest by the caller.
func (s *PostgresStore) EnsureUser(ctx context.Context, userID uint, username, apiKeyHash string) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id, username, api_key_hash) VALUES ($1, $2, $3)
		 ON CONFLICT (id) DO NOTHING`, userID, username, apiKeyHash); err != nil {
		return fmt.Errorf("billing: ensure user %d: %w", userID, err)
	}
	if _, err := s.db.ExecContext(ctx,
		`SELECT setval(pg_get_serial_sequence('users','id'),
		               GREATEST((SELECT COALESCE(MAX(id), 1) FROM users), 1))`); err != nil {
		return fmt.Errorf("billing: bump users sequence: %w", err)
	}
	return nil
}

// OpenAccount implements Store.
func (s *PostgresStore) OpenAccount(ctx context.Context, userID uint, initial model.NUMERIC) (*Account, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO ledger_accounts (user_id, balance) VALUES ($1, $2)
		 ON CONFLICT (user_id) DO NOTHING`, userID, initial); err != nil {
		return nil, fmt.Errorf("billing: open account %d: %w", userID, err)
	}
	a := &Account{UserID: userID}
	if err := tx.QueryRowContext(ctx,
		`SELECT balance, reserved, spent FROM ledger_accounts WHERE user_id = $1`,
		userID).Scan(&a.Balance, &a.Reserved, &a.Spent); err != nil {
		return nil, fmt.Errorf("billing: load account %d: %w", userID, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return a, nil
}

// Account implements Store.
func (s *PostgresStore) Account(ctx context.Context, userID uint) (Account, error) {
	a := Account{UserID: userID}
	err := s.db.QueryRowContext(ctx,
		`SELECT balance, reserved, spent FROM ledger_accounts WHERE user_id = $1`,
		userID).Scan(&a.Balance, &a.Reserved, &a.Spent)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, fmt.Errorf("%w: user %d", ErrInsufficientBalance, userID)
	}
	if err != nil {
		return Account{}, err
	}
	return a, nil
}

// Reserve implements Store.
func (s *PostgresStore) Reserve(ctx context.Context, r *Reservation) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var balance, reserved model.NUMERIC
	err = tx.QueryRowContext(ctx,
		`SELECT balance, reserved FROM ledger_accounts WHERE user_id = $1 FOR UPDATE`,
		r.UserID).Scan(&balance, &reserved)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: user %d has no ledger account", ErrInsufficientBalance, r.UserID)
	}
	if err != nil {
		return err
	}
	if balance-reserved < r.Held {
		return fmt.Errorf("%w: need %s, available %s",
			ErrInsufficientBalance, r.Held.String(), (balance - reserved).String())
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE ledger_accounts SET reserved = reserved + $2, updated_at = now() WHERE user_id = $1`,
		r.UserID, r.Held); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO ledger_reservations
		   (id, user_id, model_name, price_version, price_per_1m, held,
		    prompt_tokens, output_tokens, status, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'open', $9)`,
		r.ID, r.UserID, r.Model, r.PriceVersion, r.PricePer1M, r.Held,
		r.PromptTokens, r.OutputTokens, r.CreatedAt); err != nil {
		return fmt.Errorf("billing: insert reservation: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO ledger_entries (user_id, reservation_id, kind, amount, price_version, note)
		 VALUES ($1, $2, 'reserve', $3, $4, $5)`,
		r.UserID, r.ID, r.Held, r.PriceVersion, "hold estimated usage"); err != nil {
		return err
	}
	return tx.Commit()
}

// Reservation implements Store.
func (s *PostgresStore) Reservation(ctx context.Context, id string) (*Reservation, error) {
	r := &Reservation{ID: id}
	var status string
	err := s.db.QueryRowContext(ctx,
		`SELECT user_id, model_name, price_version, price_per_1m, held,
		        prompt_tokens, output_tokens, status, created_at
		   FROM ledger_reservations WHERE id = $1`, id).
		Scan(&r.UserID, &r.Model, &r.PriceVersion, &r.PricePer1M, &r.Held,
			&r.PromptTokens, &r.OutputTokens, &status, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrReservationNotFound
	}
	if err != nil {
		return nil, err
	}
	r.Settled = status != "open"
	return r, nil
}

// Settle implements Store.
func (s *PostgresStore) Settle(ctx context.Context, id string, actual model.NUMERIC, promptTokens, completionTokens int) (*Settlement, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	var (
		userID       uint
		held         model.NUMERIC
		priceVersion int64
		status       string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT user_id, held, price_version, status
		   FROM ledger_reservations WHERE id = $1 FOR UPDATE`, id).
		Scan(&userID, &held, &priceVersion, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrReservationNotFound
	}
	if err != nil {
		return nil, err
	}
	if status != "open" {
		return nil, ErrReservationSettled
	}

	var balance, reserved model.NUMERIC
	if err := tx.QueryRowContext(ctx,
		`SELECT balance, reserved FROM ledger_accounts WHERE user_id = $1 FOR UPDATE`,
		userID).Scan(&balance, &reserved); err != nil {
		return nil, err
	}

	out := &Settlement{ReservationID: id, Actual: actual, PriceVersion: priceVersion}
	if actual <= held {
		out.Refunded = held - actual
	} else {
		out.Overrun = actual - held
	}

	reservedAfter := reserved - held
	if reservedAfter < 0 {
		reservedAfter = 0
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE ledger_accounts
		    SET reserved = $2, balance = balance - $3, spent = spent + $3, updated_at = now()
		  WHERE user_id = $1`, userID, reservedAfter, actual); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE ledger_reservations
		    SET status = 'settled', actual = $2, prompt_tokens = $3,
		        output_tokens = $4, settled_at = now()
		  WHERE id = $1`, id, actual, promptTokens, completionTokens); err != nil {
		return nil, err
	}

	if out.Refunded > 0 {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO ledger_entries (user_id, reservation_id, kind, amount, price_version, note)
			 VALUES ($1, $2, 'refund', $3, $4, 'release unused hold')`,
			userID, id, out.Refunded, priceVersion); err != nil {
			return nil, err
		}
	}
	if out.Overrun > 0 {
		note := "charge overrun"
		if balance-actual < 0 {
			out.CreditExceeded = true
			note = "charge overrun (credit exceeded, balance goes negative)"
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO ledger_entries (user_id, reservation_id, kind, amount, price_version, note)
			 VALUES ($1, $2, 'overrun', $3, $4, $5)`,
			userID, id, out.Overrun, priceVersion, note); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO ledger_entries (user_id, reservation_id, kind, amount, price_version, note)
		 VALUES ($1, $2, 'settle', $3, $4, $5)`,
		userID, id, actual, priceVersion,
		fmt.Sprintf("settle prompt=%d completion=%d", promptTokens, completionTokens)); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// Reconcile implements Store.
func (s *PostgresStore) Reconcile(ctx context.Context, id string, ttl time.Duration) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	var rows *sql.Rows
	if id != "" {
		rows, err = tx.QueryContext(ctx,
			`SELECT id, user_id, held, price_version FROM ledger_reservations
			  WHERE status = 'open' AND id = $1 FOR UPDATE SKIP LOCKED`, id)
	} else {
		rows, err = tx.QueryContext(ctx,
			`SELECT id, user_id, held, price_version FROM ledger_reservations
			  WHERE status = 'open' AND created_at < now() - make_interval(secs => $1)
			  FOR UPDATE SKIP LOCKED`, int(ttl.Seconds()))
	}
	if err != nil {
		return 0, err
	}

	type pending struct {
		id      string
		userID  uint
		held    model.NUMERIC
		version int64
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.userID, &p.held, &p.version); err != nil {
			rows.Close()
			return 0, err
		}
		todo = append(todo, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	for _, p := range todo {
		if _, err := tx.ExecContext(ctx,
			`UPDATE ledger_accounts
			    SET reserved = GREATEST(reserved - $2, 0), updated_at = now()
			  WHERE user_id = $1`, p.userID, p.held); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE ledger_reservations SET status = 'reconciled' WHERE id = $1`, p.id); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO ledger_entries (user_id, reservation_id, kind, amount, price_version, note)
			 VALUES ($1, $2, 'reconcile', $3, $4, 'reclaimed hold')`,
			p.userID, p.id, p.held, p.version); err != nil {
			return 0, err
		}
	}

	if id != "" && len(todo) == 0 {
		return 0, ErrReservationNotFound
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(todo), nil
}

// Credit implements Store.
func (s *PostgresStore) Credit(ctx context.Context, userID uint, amount model.NUMERIC, note string) error {
	if amount <= 0 {
		return ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	res, err := tx.ExecContext(ctx,
		`UPDATE ledger_accounts SET balance = balance + $2, updated_at = now() WHERE user_id = $1`,
		userID, amount)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: user %d", ErrInsufficientBalance, userID)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO ledger_entries (user_id, kind, amount, note) VALUES ($1, 'credit', $2, $3)`,
		userID, amount, note); err != nil {
		return err
	}
	return tx.Commit()
}

// Journal implements Store.
func (s *PostgresStore) Journal(ctx context.Context, fromSeq uint64) ([]JournalEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, kind, user_id, COALESCE(reservation_id, ''), amount, price_version,
		        COALESCE(note, ''), created_at
		   FROM ledger_entries WHERE seq >= $1 ORDER BY seq`, int64(fromSeq))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []JournalEntry
	for rows.Next() {
		var e JournalEntry
		if err := rows.Scan(&e.Seq, &e.Kind, &e.UserID, &e.Reservation, &e.Amount,
			&e.PriceVersion, &e.Note, &e.At); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
