package usage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/grishkadel/llm-gateway/internal/tenant"
)

func TestTenantUsage(t *testing.T) {
	conn, tn, key := newWriterDB(t)
	ctx := context.Background()
	other, otherKey, err := tenant.NewStore(conn).Create(ctx, "other")
	if err != nil {
		t.Fatal(err)
	}

	oct := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	nov := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	row := func(tn tenant.Tenant, key tenant.NewKey, provider, model string, at time.Time, cost *int64) Row {
		return Row{RequestID: "r", TenantID: tn.ID, APIKeyID: key.ID, Provider: provider, Model: model,
			Endpoint: "/e", Status: 200, Usage: Usage{Input: 10, CachedInput: 2, CacheWrite: 1, CacheWrite1h: 1, Output: 5},
			CostMicros: cost, CreatedAt: at}
	}
	c100, c50 := int64(100), int64(50)

	w := newWriter(conn, discard(), 100, 100, time.Hour)
	w.start()
	w.Write(row(tn, key, "openai", "gpt-4o", oct.Add(time.Hour), &c100))
	w.Write(row(tn, key, "openai", "gpt-4o", oct.Add(2*time.Hour), &c50))
	w.Write(row(tn, key, "groq", "llama-3.3-70b-versatile", oct.Add(3*time.Hour), nil))
	// Outside the period: the boundary is half-open, so November's first
	// instant belongs to November.
	w.Write(row(tn, key, "openai", "gpt-4o", oct.Add(-time.Second), &c100))
	w.Write(row(tn, key, "openai", "gpt-4o", nov, &c100))
	// Someone else's usage.
	w.Write(row(other, otherKey, "openai", "gpt-4o", oct.Add(time.Hour), &c100))
	if err := w.Close(ctx); err != nil {
		t.Fatal(err)
	}

	got, err := NewReports(conn).TenantUsage(ctx, tn.ID, oct, nov)
	if err != nil {
		t.Fatalf("TenantUsage() returned error: %v", err)
	}

	wantTotal := Totals{Requests: 3, InputTokens: 30, CachedInputTokens: 6, CacheWriteTokens: 6, OutputTokens: 15,
		CostMicros: 150, UnpricedRequests: 1}
	if got.Total != wantTotal {
		t.Errorf("Total = %+v, want %+v", got.Total, wantTotal)
	}
	if len(got.ByModel) != 2 {
		t.Fatalf("ByModel = %+v, want groq and openai", got.ByModel)
	}
	// Sorted by provider, then model.
	if g := got.ByModel[0]; g.Provider != "groq" || g.Requests != 1 || g.CostMicros != 0 || g.UnpricedRequests != 1 {
		t.Errorf("ByModel[0] = %+v, want groq: 1 request, unpriced", g)
	}
	if o := got.ByModel[1]; o.Provider != "openai" || o.Model != "gpt-4o" || o.Requests != 2 || o.CostMicros != 150 {
		t.Errorf("ByModel[1] = %+v, want openai/gpt-4o: 2 requests, 150", o)
	}
}

func TestTenantUsageEmptyPeriod(t *testing.T) {
	conn, tn, _ := newWriterDB(t)

	got, err := NewReports(conn).TenantUsage(context.Background(), tn.ID,
		time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != (Totals{}) || got.ByModel == nil || len(got.ByModel) != 0 {
		t.Errorf("summary = %+v, want zero totals and an empty, non-nil ByModel", got)
	}
}

func TestTenantUsageUnknownTenant(t *testing.T) {
	conn, _, _ := newWriterDB(t)

	_, err := NewReports(conn).TenantUsage(context.Background(), "tn_0000000000000000", time.Time{}, time.Now())
	if !errors.Is(err, tenant.ErrNotFound) {
		t.Errorf("TenantUsage() error = %v, want tenant.ErrNotFound", err)
	}
}
