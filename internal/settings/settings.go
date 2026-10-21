// Package settings: DB-backed config with an in-memory copy. Write the DB
// first, then swap the map; a failed write must not change live behaviour.
package settings

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// Only these keys are writable from the console.
var Known = map[string]string{
	"enable_compress":        "Prompt 压缩器（确定性去冗余）",
	"enable_semantic_cache":  "语义缓存（需配置 EMBEDDING_API_URL）",
	"enable_shadow_test":     "影子测试：主请求返回后异步复制到影子模型",
	"shadow_model":           "影子模型名（必须在渠道目录中存在）",
	"shadow_sample_rate":     "影子采样率，0-100",
	"replay_enabled":         "允许控制台一键重放历史请求",
	"shadow_timeout_seconds": "影子请求超时（秒）",
	"replay_timeout_seconds": "重放请求超时（秒）",
}

// Defaults are used when the database is unavailable.
var Defaults = map[string]string{
	"enable_compress":        "true",
	"enable_semantic_cache":  "true",
	"enable_shadow_test":     "false",
	"shadow_model":           "",
	"shadow_sample_rate":     "100",
	"replay_enabled":         "true",
	"shadow_timeout_seconds": "60",
	"replay_timeout_seconds": "120",
}

// Store is a hot-reloadable settings snapshot.
type Store struct {
	db *sql.DB

	mu     sync.RWMutex
	values map[string]string
}

// New returns a store seeded with defaults (safe before Load).
func New(db *sql.DB) *Store {
	vals := make(map[string]string, len(Defaults))
	for k, v := range Defaults {
		vals[k] = v
	}
	return &Store{db: db, values: vals}
}

// Load refreshes the snapshot. Missing keys fall back to defaults.
func (s *Store) Load(ctx context.Context) error {
	if s.db == nil {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM system_settings`)
	if err != nil {
		return fmt.Errorf("settings load: %w", err)
	}
	defer rows.Close()

	next := make(map[string]string, len(Defaults))
	for k, v := range Defaults {
		next[k] = v
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return err
		}
		if _, ok := Known[k]; ok {
			next[k] = v
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.values = next
	s.mu.Unlock()
	return nil
}

// Get returns the value or def.
func (s *Store) Get(key, def string) string {
	s.mu.RLock()
	v, ok := s.values[key]
	s.mu.RUnlock()
	if !ok || v == "" {
		return def
	}
	return v
}

// Bool accepts 1/true/on/yes.
func (s *Store) Bool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(s.Get(key, "")))
	if v == "" {
		return def
	}
	switch v {
	case "1", "true", "on", "yes":
		return true
	case "0", "false", "off", "no":
		return false
	default:
		return def
	}
}

// Int parses an integer value.
func (s *Store) Int(key string, def int) int {
	v := strings.TrimSpace(s.Get(key, ""))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// Snapshot returns a copy of all current values.
func (s *Store) Snapshot() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.values))
	for k, v := range s.values {
		out[k] = v
	}
	return out
}

// Update: validate all keys first, then one transaction, then swap memory.
// No partial writes, no half-applied config.
func (s *Store) Update(ctx context.Context, kv map[string]string) error {
	if len(kv) == 0 {
		return fmt.Errorf("no settings supplied")
	}
	for k := range kv {
		if _, ok := Known[k]; !ok {
			return fmt.Errorf("unknown setting %q", k)
		}
	}
	if s.db != nil {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback() //nolint:errcheck
		for k, v := range kv {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO system_settings (key, value, description, updated_at)
				VALUES ($1, $2, $3, now())
				ON CONFLICT (key) DO UPDATE
				  SET value = EXCLUDED.value, updated_at = now()`, k, v, Known[k]); err != nil {
				return fmt.Errorf("settings write %s: %w", k, err)
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}

	s.mu.Lock()
	if s.values == nil {
		s.values = make(map[string]string, len(Defaults))
	}
	for k, v := range kv {
		s.values[k] = v
	}
	s.mu.Unlock()
	return nil
}
