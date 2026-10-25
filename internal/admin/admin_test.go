package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/grishkadel/llm-gateway/internal/budget"
	"github.com/grishkadel/llm-gateway/internal/db"
	"github.com/grishkadel/llm-gateway/internal/db/dbtest"
	"github.com/grishkadel/llm-gateway/internal/ratelimit"
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

// serve sends the request to the admin API over store and a ledger that has
// spent nothing.
func serve(t *testing.T, store Store, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	return serveLedger(t, store, &fakeLedger{}, method, path, body)
}

// serveLedger is serve over ledger, which may be nil.
func serveLedger(t *testing.T, store Store, ledger Ledger, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	h := Handler(store, ledger, slog.New(slog.NewTextHandler(io.Discard, nil)))
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

type tenantBody struct {
	ID                    string    `json:"id"`
	Name                  string    `json:"name"`
	RateLimitTokensPerMin int64     `json:"rate_limit_tokens_per_min"`
	BudgetMicros          int64     `json:"budget_micros"`
	BudgetPeriod          string    `json:"budget_period"`
	DefaultMaxTokens      int       `json:"default_max_tokens"`
	CreatedAt             time.Time `json:"created_at"`
}

type errorBody struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
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

// TestPatchTenant: the response is the tenant as it now stands, only the
// fields sent change, and the database holds the change by the time it
// answers. The ends of each range are valid values.
func TestPatchTenant(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	// A new tenant has 50,000,000 µ$, 100,000 tokens a minute and a cap of 4096.
	cases := []struct {
		name, body   string
		budget, rate int64
		tokens       int
	}{
		{"budget alone", `{"budget_micros":7000000}`, 7_000_000, 100_000, 4096},
		{"rate limit alone", `{"rate_limit_tokens_per_min":1234}`, 50_000_000, 1234, 4096},
		{"default cap alone", `{"default_max_tokens":99}`, 50_000_000, 100_000, 99},
		{"all three", `{"budget_micros":1,"rate_limit_tokens_per_min":2,"default_max_tokens":3}`, 1, 2, 3},
		{"lowest values", `{"budget_micros":0,"rate_limit_tokens_per_min":1,"default_max_tokens":1}`, 0, 1, 1},
		{"highest values",
			fmt.Sprintf(`{"budget_micros":%d,"rate_limit_tokens_per_min":%d,"default_max_tokens":%d}`,
				int64(tenant.MaxBudgetMicros), int64(ratelimit.MaxCost), int64(math.MaxInt32)),
			tenant.MaxBudgetMicros, ratelimit.MaxCost, math.MaxInt32},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			created, key, err := store.Create(ctx, "acme")
			if err != nil {
				t.Fatal(err)
			}

			rec := serve(t, store, http.MethodPatch, "/admin/tenants/"+created.ID, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, http.StatusOK, rec.Body)
			}
			var body struct {
				Tenant tenantBody `json:"tenant"`
			}
			decode(t, rec, &body)

			got := body.Tenant
			if got.ID != created.ID || got.Name != "acme" || got.BudgetPeriod != "monthly" || !got.CreatedAt.Equal(created.CreatedAt) {
				t.Errorf("tenant = %+v, want the ID, name, period and creation time of %+v", got, created)
			}
			if got.BudgetMicros != tc.budget || got.RateLimitTokensPerMin != tc.rate || got.DefaultMaxTokens != tc.tokens {
				t.Errorf("limits = %d µ$, %d tokens/min, cap %d; want %d, %d, %d",
					got.BudgetMicros, got.RateLimitTokensPerMin, got.DefaultMaxTokens, tc.budget, tc.rate, tc.tokens)
			}

			// Lookup is what every request goes through, so this is the change
			// taking effect.
			looked, _, err := store.Lookup(ctx, key.Plaintext)
			if err != nil {
				t.Fatal(err)
			}
			if looked.BudgetMicros != tc.budget || looked.RateLimitTokensPerMin != tc.rate || looked.DefaultMaxTokens != tc.tokens {
				t.Errorf("Lookup sees %+v, want the limits the response reported", looked)
			}
		})
	}
}

// TestPatchTenantRejectsBadInput: every refusal is a 400 invalid_request, and
// names the field at fault where there is one. failingStore fails every call,
// so a 400 also shows the request never reached the store.
func TestPatchTenantRejectsBadInput(t *testing.T) {
	fields := []string{"budget_micros", "rate_limit_tokens_per_min", "default_max_tokens"}
	valid := map[string]string{"budget_micros": "1", "rate_limit_tokens_per_min": "1", "default_max_tokens": "1"}

	var cases []struct{ body, mention string }
	add := func(body, mention string) {
		cases = append(cases, struct{ body, mention string }{body, mention})
	}

	// Not a JSON object.
	for _, body := range []string{
		``, `not json`, `[]`, `[{"budget_micros":1}]`, `"budget_micros"`, `5`, `true`, `null`,
		`{"budget_micros":1} x`, `{"budget_micros":1}{"budget_micros":2}`,
	} {
		add(body, "JSON object")
	}

	// No field at all.
	add(`{}`, "at least one")

	// A field the endpoint doesn't change: the rest of the tenant is out of
	// reach, names must match exactly, and a typo is caught. Beside a valid
	// field, it still refuses the whole request.
	for _, name := range []string{"name", "id", "budget_period", "created_at", "bugdet_micros", "Budget_Micros"} {
		add(fmt.Sprintf(`{%q:1}`, name), name)
		add(fmt.Sprintf(`{"budget_micros":1,%q:1}`, name), name)
	}

	// The wrong type, null included. A null beside a valid field is the case
	// that tells null from a missing field: if it were skipped like one, that
	// request would succeed.
	for i, field := range fields {
		sibling := fields[(i+1)%len(fields)]
		for _, value := range []string{`null`, `"5"`, `""`, `true`, `1.5`, `1e3`, `[]`, `[1]`, `{}`} {
			add(fmt.Sprintf(`{%q:%s}`, field, value), field)
			add(fmt.Sprintf(`{%q:%s,%q:%s}`, field, value, sibling, valid[sibling]), field)
		}
	}

	// Out of range, each side of each range, and past int64 altogether.
	for _, c := range []struct{ field, value string }{
		{"budget_micros", `-1`},
		{"budget_micros", strconv.FormatInt(tenant.MaxBudgetMicros+1, 10)},
		{"rate_limit_tokens_per_min", `0`},
		{"rate_limit_tokens_per_min", `-1`},
		{"rate_limit_tokens_per_min", strconv.FormatInt(ratelimit.MaxCost+1, 10)},
		{"default_max_tokens", `0`},
		{"default_max_tokens", `-1`},
		{"default_max_tokens", strconv.FormatInt(math.MaxInt32+1, 10)},
		{"budget_micros", `9223372036854775808`},
		{"rate_limit_tokens_per_min", `9223372036854775808`},
		{"default_max_tokens", `9223372036854775808`},
	} {
		add(fmt.Sprintf(`{%q:%s}`, c.field, c.value), c.field)
	}
	// One refused value refuses the request, valid fields beside it included.
	add(`{"budget_micros":1,"rate_limit_tokens_per_min":0,"default_max_tokens":1}`, "rate_limit_tokens_per_min")

	for _, tc := range cases {
		rec := serve(t, failingStore{}, http.MethodPatch, "/admin/tenants/tn_a", tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want %d; response %s", tc.body, rec.Code, http.StatusBadRequest, rec.Body)
			continue
		}
		var body errorBody
		decode(t, rec, &body)
		if body.Error.Type != "invalid_request" || !strings.Contains(body.Error.Message, tc.mention) {
			t.Errorf("body %q: error = %+v, want invalid_request mentioning %q", tc.body, body.Error, tc.mention)
		}
	}
}

func TestPatchTenantUnknownTenant(t *testing.T) {
	rec := serve(t, newStore(t), http.MethodPatch, "/admin/tenants/tn_0000000000000000", `{"budget_micros":1}`)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	var body errorBody
	decode(t, rec, &body)
	if body.Error.Type != "not_found" {
		t.Errorf("error type = %q, want not_found", body.Error.Type)
	}
}

// TestPatchTenantLogsWhatChanged: one Info line, naming the tenant and the
// fields changed, whatever order the body listed them in.
func TestPatchTenantLogsWhatChanged(t *testing.T) {
	store := newStore(t)
	created, _, err := store.Create(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	h := Handler(store, &fakeLedger{}, slog.New(slog.NewJSONHandler(&logs, nil)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/admin/tenants/"+created.ID,
		strings.NewReader(`{"default_max_tokens":512,"budget_micros":0}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, http.StatusOK, rec.Body)
	}

	var entry struct {
		Level    string   `json:"level"`
		TenantID string   `json:"tenant_id"`
		Fields   []string `json:"fields"`
	}
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatalf("log output %q is not one JSON line: %v", logs.String(), err)
	}
	if entry.Level != "INFO" || entry.TenantID != created.ID || !slices.Equal(entry.Fields, []string{"budget_micros", "default_max_tokens"}) {
		t.Errorf("log line = %+v, want INFO for %s naming budget_micros and default_max_tokens", entry, created.ID)
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
func (failingStore) Get(context.Context, string) (tenant.Tenant, error) {
	return tenant.Tenant{}, errDown
}
func (failingStore) Update(context.Context, string, tenant.Update) (tenant.Tenant, error) {
	return tenant.Tenant{}, errDown
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
		{http.MethodPatch, "/admin/tenants/tn_x", `{"budget_micros":1}`},
		{http.MethodPost, "/admin/tenants/tn_x/keys", ""},
		{http.MethodDelete, "/admin/keys/1", ""},
		{http.MethodGet, "/admin/tenants/tn_x/usage", ""},
		{http.MethodGet, "/admin/tenants/tn_x/budget", ""},
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
		{http.MethodPut, "/admin/tenants/tn_x"},
		{http.MethodPut, "/admin/keys/1"},
		{http.MethodPost, "/admin/tenants/tn_x/budget"},
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

// fakeLedger stands in for the Redis ledger: Spent returns spent, or fails
// with err when it is set, and records its call. The handler runs on the
// test's own goroutine, so the record needs no lock.
type fakeLedger struct {
	spent int64
	err   error
	calls []spentCall
}

// spentCall is one Spent a fakeLedger received.
type spentCall struct {
	tenantID string
	at       time.Time
}

func (f *fakeLedger) Spent(_ context.Context, tenantID string, at time.Time) (int64, error) {
	f.calls = append(f.calls, spentCall{tenantID, at})
	return f.spent, f.err
}

// TestTenantBudget: the report sets the tenant's budget, from the store, beside
// its spend, from the ledger, and what is left. The ledger is asked once, for
// the tenant in the path, at a time within the request, and the period and the
// reset are the ones of that time, so they can't name another month than the
// spend. A tenant spent past its budget, as when the budget is lowered, has 0
// left, not a negative amount, and its spend is reported in full.
func TestTenantBudget(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	cases := []struct {
		name                     string
		budget, spent, remaining int64
	}{
		{"part spent", 7_000_000, 2_500_000, 4_500_000},
		{"nothing spent", 7_000_000, 0, 7_000_000},
		{"all spent", 7_000_000, 7_000_000, 0},
		{"overspent", 1_000_000, 3_000_000, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			created, _, err := store.Create(ctx, "acme")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Update(ctx, created.ID, tenant.Update{BudgetMicros: &tc.budget}); err != nil {
				t.Fatal(err)
			}
			ledger := &fakeLedger{spent: tc.spent}

			before := time.Now()
			rec := serveLedger(t, store, ledger, http.MethodGet, "/admin/tenants/"+created.ID+"/budget", "")
			after := time.Now()

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, http.StatusOK, rec.Body)
			}
			if len(ledger.calls) != 1 {
				t.Fatalf("Spent calls = %+v, want one", ledger.calls)
			}
			call := ledger.calls[0]
			if call.tenantID != created.ID || call.at.Before(before) || call.at.After(after) {
				t.Errorf("Spent(%q, %v), want Spent(%q) at a time from %v to %v", call.tenantID, call.at, created.ID, before, after)
			}

			// The month of the time the ledger was asked about, not of the clock
			// now, so a test run across the end of a month still passes.
			start, end := budget.Period(call.at)
			want := map[string]any{
				"tenant_id":        created.ID,
				"period":           start.Format("2006-01"),
				"budget_micros":    float64(tc.budget),
				"spent_micros":     float64(tc.spent),
				"remaining_micros": float64(tc.remaining),
				"resets_at":        end.Format(time.RFC3339),
			}
			var got map[string]any
			decode(t, rec, &got)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("body = %v\nwant %v", got, want)
			}
		})
	}
}

// TestTenantBudgetUnknownTenant: an unknown ID is a 404, and the ledger is never
// asked, so it never builds a hash in Redis for an ID that is not a tenant.
func TestTenantBudgetUnknownTenant(t *testing.T) {
	ledger := &fakeLedger{}
	rec := serveLedger(t, newStore(t), ledger, http.MethodGet, "/admin/tenants/tn_0000000000000000/budget", "")

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	var body errorBody
	decode(t, rec, &body)
	if body.Error.Type != "not_found" {
		t.Errorf("error type = %q, want not_found", body.Error.Type)
	}
	if len(ledger.calls) != 0 {
		t.Errorf("Spent calls = %+v, want none", ledger.calls)
	}
}

// TestTenantBudgetLedgerFailure: a ledger that can't answer is a 503 the client
// can retry. The error (which may name Redis or database hosts) stays in the
// log, beside the tenant, and out of the response.
func TestTenantBudgetLedgerFailure(t *testing.T) {
	store := newStore(t)
	created, _, err := store.Create(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	errLedger := errors.New("redis: i/o timeout")

	var logs bytes.Buffer
	h := Handler(store, &fakeLedger{err: errLedger}, slog.New(slog.NewJSONHandler(&logs, nil)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/tenants/"+created.ID+"/budget", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d; body %s", rec.Code, http.StatusServiceUnavailable, rec.Body)
	}
	if v := rec.Header().Values("Retry-After"); v != nil {
		t.Errorf("Retry-After = %q, want none", v)
	}
	var body errorBody
	decode(t, rec, &body)
	if body.Error.Type != "budget_unavailable" {
		t.Errorf("error type = %q, want budget_unavailable", body.Error.Type)
	}
	if strings.Contains(rec.Body.String(), errLedger.Error()) {
		t.Errorf("response leaks the ledger error: %s", rec.Body)
	}

	var entry struct {
		Level    string `json:"level"`
		TenantID string `json:"tenant_id"`
		Error    string `json:"error"`
	}
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatalf("log output %q is not one JSON line: %v", logs.String(), err)
	}
	if entry.Level != "ERROR" || entry.TenantID != created.ID || entry.Error != errLedger.Error() {
		t.Errorf("log line = %+v, want ERROR for %s with %q", entry, created.ID, errLedger)
	}
}

// TestTenantBudgetWithoutLedger: with no ledger there are no budgets, and the
// budget route isn't registered, so the catch-all answers it. failingStore
// fails every call, so a 404 also shows the request never reached the store.
func TestTenantBudgetWithoutLedger(t *testing.T) {
	rec := serveLedger(t, failingStore{}, nil, http.MethodGet, "/admin/tenants/tn_x/budget", "")

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d; body %s", rec.Code, http.StatusNotFound, rec.Body)
	}
	var body errorBody
	decode(t, rec, &body)
	if body.Error.Type != "not_found" {
		t.Errorf("error type = %q, want not_found", body.Error.Type)
	}
}
