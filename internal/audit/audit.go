package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/djwkvnwincbnak78/nexus/internal/model"
)

// Entry is one request record destined for request_logs.
type Entry struct {
	RequestID        string
	UserID           uint
	Model            string
	Provider         string
	RouteReason      string
	PromptTokens     int
	CompletionTokens int
	Cost             model.NUMERIC
	PriceVersion     int64
	Status           string
	HTTPStatus       int
	ErrorMessage     string
	LatencyMS        int
	Cached           bool
	// Body is the verbatim client request (JSON), used by Request Replay.
	Body     json.RawMessage
	Metadata map[string]any
}

// Sink persists request records. Write must never block the request path.
type Sink interface {
	Write(ctx context.Context, e Entry) error
	Close(ctx context.Context) error
	Dropped() uint64
}

// NopSink discards records (used when no database is configured).
type NopSink struct{}

// Write implements Sink.
func (NopSink) Write(context.Context, Entry) error { return nil }

// Close implements Sink.
func (NopSink) Close(context.Context) error { return nil }

// Dropped implements Sink.
func (NopSink) Dropped() uint64 { return 0 }

// MemorySink keeps records in memory (tests, local debugging).
type MemorySink struct {
	mu      sync.Mutex
	Entries []Entry
}

// Write implements Sink.
func (s *MemorySink) Write(_ context.Context, e Entry) error {
	s.mu.Lock()
	s.Entries = append(s.Entries, e)
	s.mu.Unlock()
	return nil
}

// Close implements Sink.
func (s *MemorySink) Close(context.Context) error { return nil }

// Dropped implements Sink.
func (s *MemorySink) Dropped() uint64 { return 0 }

// PostgresSink buffers records and batch-inserts them into request_logs.
// Bounded channel: when the DB is slow the oldest pending records are dropped
// and counted, never queued without limit (memory must stay flat under load).
type PostgresSink struct {
	db      *sql.DB
	ch      chan Entry
	done    chan struct{}
	wg      sync.WaitGroup
	dropped atomic.Uint64
	once    sync.Once

	batchSize  int
	flushEvery time.Duration
}

// NewPostgresSink starts the background writer.
func NewPostgresSink(db *sql.DB, buffer, batchSize int) *PostgresSink {
	if buffer <= 0 {
		buffer = 8192
	}
	if batchSize <= 0 {
		batchSize = 200
	}
	s := &PostgresSink{
		db:         db,
		ch:         make(chan Entry, buffer),
		done:       make(chan struct{}),
		batchSize:  batchSize,
		flushEvery: time.Second,
	}
	s.wg.Add(1)
	go s.run()
	return s
}

// Write implements Sink without blocking: a full buffer drops the record.
func (s *PostgresSink) Write(_ context.Context, e Entry) error {
	select {
	case s.ch <- e:
		return nil
	default:
		s.dropped.Add(1)
		return nil
	}
}

// Dropped implements Sink.
func (s *PostgresSink) Dropped() uint64 { return s.dropped.Load() }

func (s *PostgresSink) run() {
	defer s.wg.Done()
	batch := make([]Entry, 0, s.batchSize)
	ticker := time.NewTicker(s.flushEvery)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := s.insert(ctx, batch); err != nil {
			s.dropped.Add(uint64(len(batch)))
		}
		cancel()
		batch = batch[:0]
	}

	for {
		select {
		case e, ok := <-s.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, e)
			if len(batch) >= s.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (s *PostgresSink) insert(ctx context.Context, batch []Entry) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	const q = `INSERT INTO request_logs
	  (request_id, user_id, model_name, provider, route_reason,
	   prompt_tokens, completion_tokens, cost, price_version,
	   status, http_status, error_message, latency_ms, cached, metadata, request_body)
	 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`

	for _, e := range batch {
		meta := e.Metadata
		if meta == nil {
			meta = map[string]any{}
		}
		raw, err := json.Marshal(meta)
		if err != nil {
			raw = []byte("{}")
		}
		if _, err := tx.ExecContext(ctx, q,
			e.RequestID, nullableUser(e.UserID), e.Model, e.Provider, e.RouteReason,
			e.PromptTokens, e.CompletionTokens, e.Cost, e.PriceVersion,
			e.Status, e.HTTPStatus, nullString(e.ErrorMessage), e.LatencyMS, e.Cached, raw, jsonBody(e.Body),
		); err != nil {
			return fmt.Errorf("audit insert: %w", err)
		}
	}
	return tx.Commit()
}

// Close drains the buffer and flushes remaining records.
func jsonBody(b []byte) any {
	if len(b) == 0 || !json.Valid(b) {
		return nil
	}
	return b
}

func (s *PostgresSink) Close(ctx context.Context) error {
	s.once.Do(func() { close(s.ch) })
	wait := make(chan struct{})
	go func() { s.wg.Wait(); close(wait) }()
	select {
	case <-wait:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func nullableUser(id uint) any {
	if id == 0 {
		return nil
	}
	return id
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}
