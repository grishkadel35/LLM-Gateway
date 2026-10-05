package usage

import (
	"context"
	"database/sql"
	"time"

	"github.com/grishkadel/llm-gateway/internal/tenant"
)

// Totals sums usage rows. CostMicros covers priced rows only;
// UnpricedRequests says how many rows had no cost, so a partial total isn't
// mistaken for a complete one.
type Totals struct {
	Requests          int64 `json:"requests"`
	InputTokens       int64 `json:"input_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
	CacheWriteTokens  int64 `json:"cache_write_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	CostMicros        int64 `json:"cost_micros"`
	UnpricedRequests  int64 `json:"unpriced_requests"`
}

func (t *Totals) add(o Totals) {
	t.Requests += o.Requests
	t.InputTokens += o.InputTokens
	t.CachedInputTokens += o.CachedInputTokens
	t.CacheWriteTokens += o.CacheWriteTokens
	t.OutputTokens += o.OutputTokens
	t.CostMicros += o.CostMicros
	t.UnpricedRequests += o.UnpricedRequests
}

// ModelTotals is one provider and model's share of a Summary.
type ModelTotals struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Totals
}

// Summary is a tenant's usage over a period.
type Summary struct {
	Total   Totals        `json:"total"`
	ByModel []ModelTotals `json:"by_model"`
}

// Reports reads usage back out of usage_logs.
type Reports struct {
	db *sql.DB
}

func NewReports(db *sql.DB) *Reports {
	return &Reports{db: db}
}

// TenantUsage summarizes a tenant's usage over [from, to), by provider and
// model. It returns tenant.ErrNotFound for a tenant that doesn't exist, so
// an unknown tenant isn't mistaken for one with no usage.
func (r *Reports) TenantUsage(ctx context.Context, tenantID string, from, to time.Time) (Summary, error) {
	var exists bool
	if err := r.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE id = $1)`, tenantID).Scan(&exists); err != nil {
		return Summary{}, err
	}
	if !exists {
		return Summary{}, tenant.ErrNotFound
	}

	// The (tenant_id, created_at) index serves this.
	rows, err := r.db.QueryContext(ctx, `
		SELECT provider, model, count(*),
		       sum(input_tokens), sum(cached_input_tokens), sum(cache_write_tokens), sum(output_tokens),
		       coalesce(sum(cost_micros), 0), count(*) FILTER (WHERE cost_micros IS NULL)
		FROM usage_logs
		WHERE tenant_id = $1 AND created_at >= $2 AND created_at < $3
		GROUP BY provider, model
		ORDER BY provider, model`,
		tenantID, from, to,
	)
	if err != nil {
		return Summary{}, err
	}
	defer rows.Close()

	s := Summary{ByModel: []ModelTotals{}}
	for rows.Next() {
		var m ModelTotals
		if err := rows.Scan(&m.Provider, &m.Model, &m.Requests,
			&m.InputTokens, &m.CachedInputTokens, &m.CacheWriteTokens, &m.OutputTokens,
			&m.CostMicros, &m.UnpricedRequests); err != nil {
			return Summary{}, err
		}
		s.ByModel = append(s.ByModel, m)
		s.Total.add(m.Totals)
	}
	return s, rows.Err()
}
