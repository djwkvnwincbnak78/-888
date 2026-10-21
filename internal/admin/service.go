package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/djwkvnwincbnak78/nexus/internal/model"
	"github.com/djwkvnwincbnak78/nexus/internal/secretbox"
)

// Service is the control-plane facade over PostgreSQL.
type Service struct {
	DB        *sql.DB
	Box       *secretbox.Box
	JWTSecret []byte
}

// Channel is a management view of an upstream channel (no plaintext key).
type Channel struct {
	ID        uint      `json:"id"`
	Name      string    `json:"name"`
	Provider  string    `json:"provider"`
	BaseURL   string    `json:"base_url"`
	Models    []string  `json:"models"`
	Weight    int       `json:"weight"`
	Enabled   bool      `json:"enabled"`
	HasKey    bool      `json:"has_key"`
	CreatedAt time.Time `json:"created_at"`
}

// Token is a management view of an API token.
type Token struct {
	ID        uint          `json:"id"`
	Name      string        `json:"name"`
	Prefix    string        `json:"prefix"`
	Quota     model.NUMERIC `json:"quota"`
	Used      model.NUMERIC `json:"used"`
	Status    string        `json:"status"`
	ExpiresAt *time.Time    `json:"expires_at,omitempty"`
	CreatedAt time.Time     `json:"created_at"`
}

// Dashboard is the aggregate payload for the overview page.
type Dashboard struct {
	Requests int64         `json:"requests"`
	Tokens   int64         `json:"tokens"`
	Cost     model.NUMERIC `json:"cost"`
	Series   []DailyPoint  `json:"series"`
	Models   []ModelSpend  `json:"models"`
}

// DailyPoint is one day of traffic.
type DailyPoint struct {
	Date     string `json:"date"`
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
}

// ModelSpend is token usage per model.
type ModelSpend struct {
	Model  string `json:"model"`
	Tokens int64  `json:"tokens"`
}

// SetDB wires the service.
func New(db *sql.DB, box *secretbox.Box, jwtSecret string) *Service {
	return &Service{DB: db, Box: box, JWTSecret: []byte(jwtSecret)}
}

// SeedAdmin creates/updates the console account.
func (s *Service) SeedAdmin(ctx context.Context, email, password string) error {
	if email == "" || password == "" {
		return nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	apiKeyHash := sha256Hex("seed:" + email)
	_, err = s.DB.ExecContext(ctx, `
		INSERT INTO users (username, api_key_hash, tenant_id, role, email, password_hash, rate_limit)
		VALUES ($1, $2, 1, 'admin', $3, $4, 600)
		ON CONFLICT (username) DO UPDATE
		   SET email = EXCLUDED.email, password_hash = EXCLUDED.password_hash, role = 'admin'`,
		email, apiKeyHash, email, string(hash))
	if err != nil {
		return fmt.Errorf("admin: seed: %w", err)
	}
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO tenants (id, name) VALUES (1, 'default') ON CONFLICT (id) DO NOTHING`); err != nil {
		return err
	}
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO ledger_accounts (user_id, balance) VALUES (1, 0) ON CONFLICT (user_id) DO NOTHING`); err != nil {
		return err
	}
	return nil
}

// Login verifies credentials and returns a JWT.
func (s *Service) Login(ctx context.Context, email, password string) (string, *Claims, error) {
	var (
		id     uint
		tenant uint
		role   string
		hash   sql.NullString
		status string
	)
	err := s.DB.QueryRowContext(ctx,
		`SELECT id, COALESCE(tenant_id, 1), role, password_hash, status
		   FROM users WHERE lower(email) = lower($1)`, email).
		Scan(&id, &tenant, &role, &hash, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, errors.New("invalid credentials")
	}
	if err != nil {
		return "", nil, err
	}
	if status != "active" || !hash.Valid {
		return "", nil, errors.New("invalid credentials")
	}
	if bcrypt.CompareHashAndPassword([]byte(hash.String), []byte(password)) != nil {
		return "", nil, errors.New("invalid credentials")
	}
	claims := Claims{Sub: id, Email: email, Role: role, Tenant: tenant}
	tok, err := Sign(s.JWTSecret, claims)
	if err != nil {
		return "", nil, err
	}
	return tok, &claims, nil
}

// Dashboard aggregates the last 7 days.
func (s *Service) Dashboard(ctx context.Context) (*Dashboard, error) {
	d := &Dashboard{Series: []DailyPoint{}, Models: []ModelSpend{}}

	if err := s.DB.QueryRowContext(ctx, `
		SELECT count(*), COALESCE(sum(total_tokens), 0), COALESCE(sum(cost), 0)
		  FROM request_logs WHERE request_time > now() - interval '7 days'`).
		Scan(&d.Requests, &d.Tokens, &d.Cost); err != nil {
		return nil, err
	}

	rows, err := s.DB.QueryContext(ctx, `
		SELECT to_char(request_time::date, 'YYYY-MM-DD') AS d,
		       count(*), COALESCE(sum(total_tokens), 0)
		  FROM request_logs
		 WHERE request_time > now() - interval '7 days'
		 GROUP BY 1 ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p DailyPoint
		if err := rows.Scan(&p.Date, &p.Requests, &p.Tokens); err != nil {
			return nil, err
		}
		d.Series = append(d.Series, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	mrows, err := s.DB.QueryContext(ctx, `
		SELECT model_name, COALESCE(sum(total_tokens), 0) AS t
		  FROM request_logs
		 WHERE request_time > now() - interval '7 days'
		 GROUP BY 1 ORDER BY t DESC LIMIT 8`)
	if err != nil {
		return nil, err
	}
	defer mrows.Close()
	for mrows.Next() {
		var m ModelSpend
		if err := mrows.Scan(&m.Model, &m.Tokens); err != nil {
			return nil, err
		}
		d.Models = append(d.Models, m)
	}
	return d, mrows.Err()
}

// ListChannels returns all channels (keys never leave the server).
func (s *Service) ListChannels(ctx context.Context) ([]Channel, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, name, provider, base_url, models::text, weight, enabled,
		       (COALESCE(api_key_cipher, '') <> '' OR COALESCE(api_key, '') <> ''), created_at
		  FROM channels ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Channel
	for rows.Next() {
		var c Channel
		var modelsRaw string
		if err := rows.Scan(&c.ID, &c.Name, &c.Provider, &c.BaseURL, &modelsRaw,
			&c.Weight, &c.Enabled, &c.HasKey, &c.CreatedAt); err != nil {
			return nil, err
		}
		c.Models = parseTextArray(modelsRaw)
		if c.Models == nil {
			c.Models = []string{}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CreateChannelInput is the POST payload.
type CreateChannelInput struct {
	Name     string   `json:"name"`
	Provider string   `json:"provider"`
	BaseURL  string   `json:"base_url"`
	APIKey   string   `json:"api_key"`
	Models   []string `json:"models"`
	Weight   int      `json:"weight"`
}

// CreateChannel encrypts the key and inserts a channel.
func (s *Service) CreateChannel(ctx context.Context, in CreateChannelInput) (*Channel, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Provider = strings.TrimSpace(in.Provider)
	if in.Name == "" || in.APIKey == "" || len(in.Models) == 0 {
		return nil, errors.New("name, api_key and models are required")
	}
	switch in.Provider {
	case "openai", "anthropic", "ollama":
	default:
		return nil, errors.New("provider must be openai, anthropic or ollama")
	}
	if in.Weight <= 0 {
		in.Weight = 1
	}
	cipherText, err := s.Box.Seal(in.APIKey)
	if err != nil {
		return nil, err
	}
	var id uint
	err = s.DB.QueryRowContext(ctx, `
		INSERT INTO channels (name, provider, api_key, base_url, models, weight, enabled, api_key_cipher)
		VALUES ($1, $2, '', $3, $4, $5, TRUE, $6)
		RETURNING id`,
		in.Name, in.Provider, in.BaseURL, in.Models, in.Weight, cipherText).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("admin: create channel: %w", err)
	}
	return &Channel{ID: id, Name: in.Name, Provider: in.Provider, BaseURL: in.BaseURL,
		Models: in.Models, Weight: in.Weight, Enabled: true, HasKey: true, CreatedAt: time.Now()}, nil
}

// SetChannelEnabled toggles a channel.
func (s *Service) SetChannelEnabled(ctx context.Context, id uint, enabled bool) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE channels SET enabled = $2, updated_at = now() WHERE id = $1`, id, enabled)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteChannel removes a channel and its model mappings.
func (s *Service) DeleteChannel(ctx context.Context, id uint) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM channels WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListTokens returns a user's tokens.
func (s *Service) ListTokens(ctx context.Context, userID uint) ([]Token, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, name, token_prefix, quota, used, status, expires_at, created_at
		  FROM api_tokens WHERE user_id = $1 ORDER BY id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		var exp sql.NullTime
		if err := rows.Scan(&t.ID, &t.Name, &t.Prefix, &t.Quota, &t.Used, &t.Status, &exp, &t.CreatedAt); err != nil {
			return nil, err
		}
		if exp.Valid {
			tt := exp.Time
			t.ExpiresAt = &tt
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateToken issues sk-nexus-* and stores only its hash. The plaintext is
// returned exactly once.
func (s *Service) CreateToken(ctx context.Context, userID uint, name string, quota model.NUMERIC, days int) (*Token, string, error) {
	if name == "" {
		name = "default"
	}
	if quota < 0 {
		return nil, "", errors.New("quota must be >= 0")
	}
	plain := "sk-nexus-" + randomHex(24)
	hash := sha256Hex(plain)
	prefix := plain[:16]
	var exp any
	if days > 0 {
		exp = time.Now().Add(time.Duration(days) * 24 * time.Hour)
	}
	var id uint
	err := s.DB.QueryRowContext(ctx, `
		INSERT INTO api_tokens (user_id, name, token_hash, token_prefix, quota, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		userID, name, hash, prefix, quota, exp).Scan(&id)
	if err != nil {
		return nil, "", fmt.Errorf("admin: create token: %w", err)
	}
	return &Token{ID: id, Name: name, Prefix: prefix, Quota: quota, Status: "active", CreatedAt: time.Now()}, plain, nil
}

// DeleteToken revokes a user's token.
func (s *Service) DeleteToken(ctx context.Context, userID, id uint) error {
	res, err := s.DB.ExecContext(ctx,
		`UPDATE api_tokens SET status = 'disabled' WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ResolveAPIToken maps an sk-nexus key to its owner (gateway auth).
func (s *Service) ResolveAPIToken(ctx context.Context, plain string) (uint, model.NUMERIC, bool) {
	var userID uint
	var quota, used model.NUMERIC
	var status string
	var expires sql.NullTime
	err := s.DB.QueryRowContext(ctx, `
		SELECT user_id, quota, used, status, expires_at
		  FROM api_tokens WHERE token_hash = $1`, sha256Hex(plain)).
		Scan(&userID, &quota, &used, &status, &expires)
	if err != nil || status != "active" {
		return 0, 0, false
	}
	if expires.Valid && time.Now().After(expires.Time) {
		return 0, 0, false
	}
	if quota > 0 && used >= quota {
		return 0, 0, false
	}
	return userID, quota - used, true
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b)
}

// parseTextArray converts PostgreSQL's {"a","b"} literal into a slice.
func parseTextArray(raw string) []string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "{")
	raw = strings.TrimSuffix(raw, "}")
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, `"`)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
