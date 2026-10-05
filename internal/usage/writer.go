package usage

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Row is one usage_logs row: one request that reached a provider.
type Row struct {
	RequestID string
	// ProviderRequestID is stored as NULL when "".
	ProviderRequestID string
	TenantID          string
	APIKeyID          int64
	Provider          string
	Model             string
	Endpoint          string
	Status            int
	Usage
	// CostMicros is nil when the model isn't priced, and stored as NULL.
	CostMicros *int64
	Cached     bool
	Streamed   bool
	LatencyMS  int64
	// CreatedAt is when the request arrived, not when the row was flushed,
	// so a row lands in the right budget period.
	CreatedAt time.Time
}

const (
	// DefaultQueue rows can wait for a flush before Write starts dropping.
	DefaultQueue = 10_000
	batchSize    = 100
	flushEvery   = time.Second
	// flushTimeout bounds one batch insert, so a hung database can't stall
	// the writer forever.
	flushTimeout = 10 * time.Second
)

// Writer inserts usage rows in batches, off the response path: Write only
// queues a row, and a goroutine flushes every batchSize rows or flushEvery,
// whichever comes first.
//
// Rows are lost only when the queue is full, on a write after Close, or when
// a batch insert fails; each is logged, and the first two are counted by
// Dropped. A gateway crash loses at most what is queued.
type Writer struct {
	db         *sql.DB
	logger     *slog.Logger
	rows       chan Row
	batchSize  int
	flushEvery time.Duration

	// mu guards closed: Write must never send on the closed channel.
	mu      sync.RWMutex
	closed  bool
	done    chan struct{}
	dropped atomic.Int64
}

// NewWriter returns a running Writer. Close it on shutdown to flush what is
// queued.
func NewWriter(db *sql.DB, logger *slog.Logger) *Writer {
	w := newWriter(db, logger, DefaultQueue, batchSize, flushEvery)
	w.start()
	return w
}

func newWriter(db *sql.DB, logger *slog.Logger, queue, batch int, every time.Duration) *Writer {
	return &Writer{
		db:         db,
		logger:     logger,
		rows:       make(chan Row, queue),
		batchSize:  batch,
		flushEvery: every,
		done:       make(chan struct{}),
	}
}

func (w *Writer) start() { go w.run() }

// Write queues r without blocking. If the queue is full, or the writer is
// closed, r is dropped, logged and counted.
func (w *Writer) Write(r Row) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	if !w.closed {
		select {
		case w.rows <- r:
			return
		default:
		}
	}
	w.dropped.Add(1)
	w.logger.Error("usage row dropped", "request_id", r.RequestID, "tenant_id", r.TenantID, "closed", w.closed)
}

// Dropped reports how many rows Write has dropped.
func (w *Writer) Dropped() int64 { return w.dropped.Load() }

// Close stops accepting rows and waits until everything queued is written,
// or until ctx ends.
func (w *Writer) Close(ctx context.Context) error {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.rows)
	}
	w.mu.Unlock()

	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("usage writer: %w with rows still queued", ctx.Err())
	}
}

func (w *Writer) run() {
	defer close(w.done)

	ticker := time.NewTicker(w.flushEvery)
	defer ticker.Stop()

	batch := make([]Row, 0, w.batchSize)
	for {
		select {
		case r, ok := <-w.rows:
			if !ok {
				w.flush(batch)
				return
			}
			batch = append(batch, r)
			if len(batch) >= w.batchSize {
				w.flush(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			w.flush(batch)
			batch = batch[:0]
		}
	}
}

const rowColumns = 17

// flush inserts batch as one multi-row INSERT.
func (w *Writer) flush(batch []Row) {
	if len(batch) == 0 {
		return
	}

	var q strings.Builder
	q.WriteString(`INSERT INTO usage_logs (request_id, provider_request_id, tenant_id, api_key_id,
		provider, model, endpoint, status, input_tokens, cached_input_tokens, cache_write_tokens,
		output_tokens, cost_micros, cached, streamed, latency_ms, created_at) VALUES `)
	args := make([]any, 0, len(batch)*rowColumns)
	for i, r := range batch {
		if i > 0 {
			q.WriteString(", ")
		}
		q.WriteByte('(')
		for c := range rowColumns {
			if c > 0 {
				q.WriteString(", ")
			}
			fmt.Fprintf(&q, "$%d", i*rowColumns+c+1)
		}
		q.WriteByte(')')

		var providerID sql.NullString
		if r.ProviderRequestID != "" {
			providerID = sql.NullString{String: r.ProviderRequestID, Valid: true}
		}
		args = append(args, r.RequestID, providerID, r.TenantID, r.APIKeyID,
			r.Provider, r.Model, r.Endpoint, r.Status, r.Input, r.CachedInput,
			// One column for both TTLs: the cost already priced them apart.
			r.CacheWrite+r.CacheWrite1h,
			r.Output, r.CostMicros, r.Cached, r.Streamed, r.LatencyMS, r.CreatedAt)
	}

	ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
	defer cancel()
	if _, err := w.db.ExecContext(ctx, q.String(), args...); err != nil {
		w.logger.Error("usage rows lost: batch insert failed", "rows", len(batch), "error", err)
	}
}
