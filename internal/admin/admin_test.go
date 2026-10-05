package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/grishkadel/llm-gateway/internal/db"
	"github.com/grishkadel/llm-gateway/internal/db/dbtest"
	"github.com/grishkadel/llm-gateway/internal/tenant"
	"github.com/grishkadel/llm-gateway/internal/usage"
)

// stores is what the gateway gives the admin API: tenants and usage.
type stores struct {
	*tenant.Store
	*usage.Reports
}

// newStore returns the admin stores over a fresh, migrated database.
func newStore(t *testing.T) stores {
	t.Helper()

	conn := dbtest.New(t)
	if _, err := db.Migrate(context.Background(), conn); err != nil {
		t.Fatalf("Migrate() returned error: %v", err)
	}
	return stores{tenant.NewStore(conn), usage.NewReports(conn)}
}

func serve(t *testing.T, store Store, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	h := Handler(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

// decode unmarshals a JSON response body into v, failing the test if it isn't
// JSON.
func decode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("body %q is not JSON: %v", rec.Body, err)
	}
}

type keyBody struct {
	ID        int64  `json:"id"`
	TenantID  string `json:"tenant_id"`
	Prefix    string `json:"prefix"`
	Plaintext string `json:"plaintext"`
}

type errorBody struct {
	Error struct {
		Type string `json:"type"`
	} `json:"error"`
}

// TestCreateTenant: the response holds the only copy of the first key that
// will ever exist, so it must work, and must not be cached on the way.
func TestCreateTenant(t *testing.T) {
	store := newStore(t)

	rec := serve(t, store, http.MethodPost, "/admin/tenants", `{"name":"acme"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, http.StatusCreated, rec.Body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store on a response carrying a key", got)
	}

	var body struct {
		Tenant struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			BudgetMicros int64  `json:"budget_micros"`
		} `json:"tenant"`
		Key keyBody `json:"key"`
	}
	decode(t, rec, &body)

	if body.Tenant.Name != "acme" || !strings.HasPrefix(body.Tenant.ID, "tn_") {
		t.Errorf("tenant = %+v, want name acme and a tn_ ID", body.Tenant)
	}
	if body.Tenant.BudgetMicros != 50_000_000 {
		t.Errorf("budget_micros = %d, want the schema default 50000000", body.Tenant.BudgetMicros)
	}
	if !strings.HasPrefix(body.Key.Plaintext, body.Key.Prefix) || body.Key.TenantID != body.Tenant.ID {
		t.Errorf("key = %+v, want a plaintext starting with its prefix, owned by %s", body.Key, body.Tenant.ID)
	}

	got, k, err := store.Lookup(context.Background(), body.Key.Plaintext)
	if err != nil {
		t.Fatalf("returned key does not authenticate: %v", err)
	}
	if got.ID != body.Tenant.ID || k.ID != body.Key.ID {
		t.Errorf("key looks up to tenant %q key %d, want %q key %d", got.ID, k.ID, body.Tenant.ID, body.Key.ID)
	}
}

func TestCreateTenantRejectsBadInput(t *testing.T) {
	store := newStore(t)

	for _, body := range []string{``, `not json`, `{}`, `{"name":""}`, `{"name":"   "}`, `{"name":42}`} {
		rec := serve(t, store, http.MethodPost, "/admin/tenants", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want %d", body, rec.Code, http.StatusBadRequest)
		}
	}
}

func TestIssueKey(t *testing.T) {
	store := newStore(t)
	created, first, err := store.Create(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}

	rec := serve(t, store, http.MethodPost, "/admin/tenants/"+created.ID+"/keys", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, http.StatusCreated, rec.Body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store on a response carrying a key", got)
	}

	var body struct {
		Key keyBody `json:"key"`
	}
	decode(t, rec, &body)

	if body.Key.ID == first.ID || body.Key.Plaintext == first.Plaintext {
		t.Errorf("issued key %+v repeats the first key", body.Key)
	}
	got, _, err := store.Lookup(context.Background(), body.Key.Plaintext)
	if err != nil || got.ID != created.ID {
		t.Errorf("issued key looks up to tenant %q (err %v), want %q", got.ID, err, created.ID)
	}
}

func TestIssueKeyUnknownTenant(t *testing.T) {
	rec := serve(t, newStore(t), http.MethodPost, "/admin/tenants/tn_0000000000000000/keys", "")

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	var body errorBody
	decode(t, rec, &body)
	if body.Error.Type != "not_found" {
		t.Errorf("error type = %q, want not_found", body.Error.Type)
	}
}

// TestRevokeKey: revoking is idempotent, and the revoked key stops working.
// The response never carries the key.
func TestRevokeKey(t *testing.T) {
	store := newStore(t)
	_, key, err := store.Create(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}

	for i := range 2 {
		rec := serve(t, store, http.MethodDelete, "/admin/keys/"+strconv.FormatInt(key.ID, 10), "")
		if rec.Code != http.StatusNoContent {
			t.Errorf("revoke #%d: status = %d, want %d", i+1, rec.Code, http.StatusNoContent)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("revoke #%d: body = %q, want empty", i+1, rec.Body)
		}
	}

	if _, _, err := store.Lookup(context.Background(), key.Plaintext); !errors.Is(err, tenant.ErrNotFound) {
		t.Errorf("Lookup() of a revoked key = %v, want ErrNotFound", err)
	}
}

func TestRevokeKeyBadOrUnknownID(t *testing.T) {
	store := newStore(t)

	cases := []struct {
		id   string
		want int
	}{
		{"999999", http.StatusNotFound},
		{"abc", http.StatusBadRequest},
		{"-1", http.StatusBadRequest},
		{"0", http.StatusBadRequest},
	}
	for _, tc := range cases {
		rec := serve(t, store, http.MethodDelete, "/admin/keys/"+tc.id, "")
		if rec.Code != tc.want {
			t.Errorf("id %q: status = %d, want %d", tc.id, rec.Code, tc.want)
		}
	}
}

// failingStore stands in for a database that is down.
type failingStore struct{}

var errDown = errors.New("connection refused")

func (failingStore) Create(context.Context, string) (tenant.Tenant, tenant.NewKey, error) {
	return tenant.Tenant{}, tenant.NewKey{}, errDown
}
func (failingStore) IssueKey(context.Context, string) (tenant.NewKey, error) {
	return tenant.NewKey{}, errDown
}
func (failingStore) RevokeKey(context.Context, int64) error { return errDown }
func (failingStore) TenantUsage(context.Context, string, time.Time, time.Time) (usage.Summary, error) {
	return usage.Summary{}, errDown
}

// TestStoreFailureIs500: a database error is the gateway's fault, and its
// text (which may name hosts or SQL) stays in the log, not the response.
func TestStoreFailureIs500(t *testing.T) {
	requests := []struct{ method, path, body string }{
		{http.MethodPost, "/admin/tenants", `{"name":"acme"}`},
		{http.MethodPost, "/admin/tenants/tn_x/keys", ""},
		{http.MethodDelete, "/admin/keys/1", ""},
		{http.MethodGet, "/admin/tenants/tn_x/usage", ""},
	}
	for _, r := range requests {
		rec := serve(t, failingStore{}, r.method, r.path, r.body)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s %s: status = %d, want %d", r.method, r.path, rec.Code, http.StatusInternalServerError)
		}
		if strings.Contains(rec.Body.String(), errDown.Error()) {
			t.Errorf("%s %s: response leaks the store error: %s", r.method, r.path, rec.Body)
		}
	}
}

// TestUnknownAdminRouteIsJSON: paths and methods the admin API doesn't serve
// get the same JSON error shape as everything else, not ServeMux's text.
func TestUnknownAdminRouteIsJSON(t *testing.T) {
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/admin/nope"},
		{http.MethodGet, "/admin/tenants"},
		{http.MethodPut, "/admin/keys/1"},
	} {
		rec := serve(t, failingStore{}, r.method, r.path, "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: status = %d, want %d", r.method, r.path, rec.Code, http.StatusNotFound)
		}
		var body errorBody
		decode(t, rec, &body)
		if body.Error.Type != "not_found" {
			t.Errorf("%s %s: error type = %q, want not_found", r.method, r.path, body.Error.Type)
		}
	}
}

// usageStore records the period it was asked for.
type usageStore struct {
	failingStore
	tenantID string
	from, to time.Time
}

func (u *usageStore) TenantUsage(_ context.Context, tenantID string, from, to time.Time) (usage.Summary, error) {
	if tenantID == "tn_missing" {
		return usage.Summary{}, tenant.ErrNotFound
	}
	u.tenantID, u.from, u.to = tenantID, from, to
	return usage.Summary{
		Total:   usage.Totals{Requests: 2, CostMicros: 150},
		ByModel: []usage.ModelTotals{{Provider: "openai", Model: "gpt-4o", Totals: usage.Totals{Requests: 2, CostMicros: 150}}},
	}, nil
}

func TestTenantUsageExplicitPeriod(t *testing.T) {
	store := &usageStore{}
	rec := serve(t, store, http.MethodGet, "/admin/tenants/tn_a/usage?from=2026-09-01T00:00:00Z&to=2026-09-15T12:00:00%2B02:00", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}

	if store.tenantID != "tn_a" ||
		!store.from.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) ||
		!store.to.Equal(time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("asked for %s %v..%v", store.tenantID, store.from, store.to)
	}

	var body struct {
		TenantID string    `json:"tenant_id"`
		From     time.Time `json:"from"`
		To       time.Time `json:"to"`
		Total    struct {
			Requests   int64 `json:"requests"`
			CostMicros int64 `json:"cost_micros"`
		} `json:"total"`
		ByModel []struct {
			Model string `json:"model"`
		} `json:"by_model"`
	}
	decode(t, rec, &body)
	if body.TenantID != "tn_a" || body.Total.Requests != 2 || body.Total.CostMicros != 150 || len(body.ByModel) != 1 {
		t.Errorf("body = %+v", body)
	}
	if !body.From.Equal(store.from) || !body.To.Equal(store.to) {
		t.Errorf("body period %v..%v, want the period queried", body.From, body.To)
	}
}

// TestTenantUsageDefaultsToCurrentMonth: with no period given, the current
// calendar month in UTC, the period a monthly budget covers.
func TestTenantUsageDefaultsToCurrentMonth(t *testing.T) {
	store := &usageStore{}
	before := time.Now().UTC()
	rec := serve(t, store, http.MethodGet, "/admin/tenants/tn_a/usage", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}

	start := time.Date(before.Year(), before.Month(), 1, 0, 0, 0, 0, time.UTC)
	if !store.from.Equal(start) || !store.to.Equal(start.AddDate(0, 1, 0)) {
		t.Errorf("period = %v..%v, want %v..%v", store.from, store.to, start, start.AddDate(0, 1, 0))
	}
}

func TestTenantUsageRejectsBadInput(t *testing.T) {
	cases := []struct {
		query string
		want  int
	}{
		{"?from=yesterday", http.StatusBadRequest},
		{"?to=2026-13-01T00:00:00Z", http.StatusBadRequest},
		{"?from=2026-10-01T00:00:00Z&to=2026-10-01T00:00:00Z", http.StatusBadRequest},
		{"?from=2026-11-01T00:00:00Z&to=2026-10-01T00:00:00Z", http.StatusBadRequest},
	}
	for _, tc := range cases {
		rec := serve(t, &usageStore{}, http.MethodGet, "/admin/tenants/tn_a/usage"+tc.query, "")
		if rec.Code != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.query, rec.Code, tc.want)
		}
	}

	rec := serve(t, &usageStore{}, http.MethodGet, "/admin/tenants/tn_missing/usage", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown tenant: status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}
