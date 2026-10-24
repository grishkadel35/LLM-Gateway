package tenant

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/grishkadel/llm-gateway/internal/db"
	"github.com/grishkadel/llm-gateway/internal/db/dbtest"
)

// TestLookupRejectsMalformedKeysWithoutQuerying uses a Store with no database:
// if Lookup tried to query, the nil *sql.DB would panic.
func TestLookupRejectsMalformedKeysWithoutQuerying(t *testing.T) {
	s := NewStore(nil)
	for _, key := range []string{
		"",
		"gw_",
		"gw_tooshort",
		"sk-" + strings.Repeat("a", keyLen-3),   // right length, wrong prefix
		KeyPrefix + strings.Repeat("a", keyLen), // right prefix, too long
	} {
		if _, _, err := s.Lookup(context.Background(), key); !errors.Is(err, ErrNotFound) {
			t.Errorf("Lookup(%q) error = %v, want ErrNotFound", key, err)
		}
	}
}

func TestHashKeyIsSHA256OfWholeKey(t *testing.T) {
	key := KeyPrefix + strings.Repeat("ab", keyBytes)
	want := sha256.Sum256([]byte(key))
	if got := hashKey(key); !bytes.Equal(got, want[:]) {
		t.Errorf("hashKey = %x, want %x", got, want)
	}
}

// newStore returns a Store over a migrated throwaway database.
func newStore(t *testing.T) *Store {
	t.Helper()
	conn := dbtest.New(t)
	if _, err := db.Migrate(context.Background(), conn); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	return NewStore(conn)
}

func TestCreateIssuesAWorkingKey(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	tn, key, err := s.Create(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(tn.ID, "tn_") || tn.Name != "acme" {
		t.Errorf("tenant = %+v, want a tn_ ID and name acme", tn)
	}
	// The schema's defaults come back, so callers see the limits in force.
	if tn.RateLimitTokensPerMin != 100000 || tn.BudgetMicros != 50000000 || tn.BudgetPeriod != "monthly" || tn.DefaultMaxTokens != 4096 {
		t.Errorf("tenant limits = %+v, want the schema defaults", tn)
	}
	if len(key.Plaintext) != keyLen || !strings.HasPrefix(key.Plaintext, KeyPrefix) {
		t.Errorf("key %q: want %s followed by %d hex characters", key.Plaintext, KeyPrefix, 2*keyBytes)
	}
	if key.Prefix != key.Plaintext[:displayLen] || key.TenantID != tn.ID {
		t.Errorf("key = %+v, want prefix %q and tenant %q", key.APIKey, key.Plaintext[:displayLen], tn.ID)
	}

	got, gotKey, err := s.Lookup(ctx, key.Plaintext)
	if err != nil {
		t.Fatalf("Lookup of a fresh key: %v", err)
	}
	if got.ID != tn.ID || gotKey.ID != key.ID || gotKey.Prefix != key.Prefix || gotKey.TenantID != tn.ID {
		t.Errorf("Lookup = %+v, %+v; want tenant %q, key %d", got, gotKey, tn.ID, key.ID)
	}
}

// TestLimitsMustBePositive checks the CHECK constraints from migration 00002:
// the rate limiter divides by the limit, and the default cap is the output
// assumed for a request that sets none, so neither may be zero or negative.
func TestLimitsMustBePositive(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	// The defaults satisfy the constraints, so a new tenant still inserts.
	tn, _, err := s.Create(ctx, "acme")
	if err != nil {
		t.Fatalf("creating a tenant with the default limits: %v", err)
	}

	const checkViolation = "23514"
	for _, col := range []struct{ name, constraint string }{
		{"rate_limit_tokens_per_min", "tenants_rate_limit_tokens_per_min_positive"},
		{"default_max_tokens", "tenants_default_max_tokens_positive"},
	} {
		for _, value := range []int{0, -1} {
			// A column name can't be a query parameter; col.name is from the list above.
			_, err := s.db.ExecContext(ctx, `UPDATE tenants SET `+col.name+` = $1 WHERE id = $2`, value, tn.ID)

			// Go note: database/sql returns the driver's own error value, and
			// errors.As reaches it even when it is wrapped. The target is a pointer
			// to a variable of the wanted type, here the pgx driver's
			// *pgconn.PgError, so the call takes &pgErr. Postgres tags every error
			// with a five-character SQLSTATE code; testing that is sturdier than
			// matching the message text.
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != checkViolation {
				t.Errorf("setting %s to %d: error = %v, want a check violation (SQLSTATE %s)", col.name, value, err, checkViolation)
			} else if pgErr.ConstraintName != col.constraint {
				t.Errorf("setting %s to %d: violated %q, want %q", col.name, value, pgErr.ConstraintName, col.constraint)
			}
		}
	}

	// One is the smallest valid limit, so the checks are > 0, not > 1.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE tenants SET rate_limit_tokens_per_min = 1, default_max_tokens = 1 WHERE id = $1`, tn.ID,
	); err != nil {
		t.Errorf("setting both limits to 1: %v", err)
	}
}

// violatedCheck names the CHECK constraint err reports as violated (SQLSTATE
// 23514), or returns "" for any other error.
func violatedCheck(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23514" {
		return pgErr.ConstraintName
	}
	return ""
}

// TestBudgetChecks checks the CHECK constraints from migration 00003: the
// budget ledger's Lua is exact only below 2^53, so a budget stops at
// MaxBudgetMicros, and monthly is the only period the gateway enforces.
func TestBudgetChecks(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	tn, _, err := s.Create(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		column, constraint string
		value              any
	}{
		{"budget_micros", "tenants_budget_micros_range", int64(-1)},
		{"budget_micros", "tenants_budget_micros_range", int64(MaxBudgetMicros) + 1},
		{"budget_period", "tenants_budget_period_monthly", "weekly"},
	} {
		// A column name can't be a query parameter; tc.column is from the list above.
		_, err := s.db.ExecContext(ctx, `UPDATE tenants SET `+tc.column+` = $1 WHERE id = $2`, tc.value, tn.ID)
		if got := violatedCheck(err); got != tc.constraint {
			t.Errorf("setting %s to %v: error = %v, want a violation of %q", tc.column, tc.value, err, tc.constraint)
		}
	}

	// Both ends of the range are valid, so the check is BETWEEN, not a strict
	// inequality on either side. MaxBudgetMicros is the cap the database holds.
	for _, budget := range []int64{0, MaxBudgetMicros} {
		if _, err := s.db.ExecContext(ctx, `UPDATE tenants SET budget_micros = $1 WHERE id = $2`, budget, tn.ID); err != nil {
			t.Errorf("setting the budget to %d: %v", budget, err)
		}
	}
}

// TestGetMatchesLookup: Get returns the tenant as Lookup does, and ErrNotFound
// for an unknown ID.
func TestGetMatchesLookup(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	tn, key, err := s.Create(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	// Limits that differ from the defaults and from each other, so a column
	// read into the wrong field shows.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE tenants SET rate_limit_tokens_per_min = 111, budget_micros = 222, default_max_tokens = 333 WHERE id = $1`, tn.ID,
	); err != nil {
		t.Fatal(err)
	}

	want, _, err := s.Lookup(ctx, key.Plaintext)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("Get = %+v, want what Lookup returns, %+v", got, want)
	}
	if got.RateLimitTokensPerMin != 111 || got.BudgetMicros != 222 || got.DefaultMaxTokens != 333 || got.Name != "acme" {
		t.Errorf("Get = %+v, want name acme and limits 111, 222, 333", got)
	}

	if _, err := s.Get(ctx, "tn_missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get of an unknown ID: error = %v, want ErrNotFound", err)
	}
}

// TestUpdateChangesOnlyTheFieldsSet: Update changes what it is given, returns
// the tenant after the change, and the next Lookup sees it. Nothing else
// changes, another tenant's row included.
func TestUpdateChangesOnlyTheFieldsSet(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	other, _, err := s.Create(ctx, "other")
	if err != nil {
		t.Fatal(err)
	}

	budget, rate, tokens, zero := int64(7_000_000), int64(1234), 99, int64(0)
	// A new tenant has 50,000,000 µ$, 100,000 tokens a minute and a cap of 4096.
	cases := []struct {
		name         string
		u            Update
		budget, rate int64
		tokens       int
	}{
		{"budget alone", Update{BudgetMicros: &budget}, budget, 100_000, 4096},
		{"rate limit alone", Update{RateLimitTokensPerMin: &rate}, 50_000_000, rate, 4096},
		{"default cap alone", Update{DefaultMaxTokens: &tokens}, 50_000_000, 100_000, tokens},
		{"all three", Update{BudgetMicros: &budget, RateLimitTokensPerMin: &rate, DefaultMaxTokens: &tokens}, budget, rate, tokens},
		{"budget of zero", Update{BudgetMicros: &zero}, 0, 100_000, 4096},
		{"nothing", Update{}, 50_000_000, 100_000, 4096},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before, key, err := s.Create(ctx, "acme")
			if err != nil {
				t.Fatal(err)
			}

			got, err := s.Update(ctx, before.ID, tc.u)
			if err != nil {
				t.Fatal(err)
			}
			if got.ID != before.ID || got.Name != before.Name || got.BudgetPeriod != before.BudgetPeriod || !got.CreatedAt.Equal(before.CreatedAt) {
				t.Errorf("Update returned %+v, want the ID, name, period and creation time of %+v", got, before)
			}
			if got.BudgetMicros != tc.budget || got.RateLimitTokensPerMin != tc.rate || got.DefaultMaxTokens != tc.tokens {
				t.Errorf("Update returned limits %d, %d, %d; want %d, %d, %d",
					got.BudgetMicros, got.RateLimitTokensPerMin, got.DefaultMaxTokens, tc.budget, tc.rate, tc.tokens)
			}

			looked, _, err := s.Lookup(ctx, key.Plaintext)
			if err != nil {
				t.Fatal(err)
			}
			if looked != got {
				t.Errorf("Lookup = %+v, want what Update returned, %+v", looked, got)
			}

			if again, err := s.Get(ctx, other.ID); err != nil || again != other {
				t.Errorf("another tenant is now %+v (error %v), want %+v", again, err, other)
			}
		})
	}
}

func TestUpdateUnknownTenant(t *testing.T) {
	budget := int64(1)
	if _, err := newStore(t).Update(context.Background(), "tn_missing", Update{BudgetMicros: &budget}); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

// TestUpdateRefusedValue: a value the CHECK constraints refuse is an error,
// and the statement is all or nothing: the valid fields sent with it don't
// change either.
func TestUpdateRefusedValue(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	ok, negative, tooBig, zero, noTokens := int64(5), int64(-1), int64(MaxBudgetMicros)+1, int64(0), 0
	for _, tc := range []struct {
		name       string
		u          Update
		constraint string
	}{
		{"budget below 0", Update{BudgetMicros: &negative}, "tenants_budget_micros_range"},
		{"budget above the cap", Update{BudgetMicros: &tooBig}, "tenants_budget_micros_range"},
		{"rate limit of 0", Update{RateLimitTokensPerMin: &zero}, "tenants_rate_limit_tokens_per_min_positive"},
		{"default cap of 0", Update{DefaultMaxTokens: &noTokens}, "tenants_default_max_tokens_positive"},
		{"one refused beside a valid one", Update{BudgetMicros: &ok, DefaultMaxTokens: &noTokens}, "tenants_default_max_tokens_positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, _, err := s.Create(ctx, "acme")
			if err != nil {
				t.Fatal(err)
			}

			if _, err := s.Update(ctx, before.ID, tc.u); violatedCheck(err) != tc.constraint {
				t.Errorf("error = %v, want a violation of %q", err, tc.constraint)
			}
			if after, err := s.Get(ctx, before.ID); err != nil || after != before {
				t.Errorf("after the refusal the tenant is %+v (error %v), want it unchanged: %+v", after, err, before)
			}
		})
	}
}

func TestOnlyTheHashIsStored(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	_, key, err := s.Create(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}

	var stored []byte
	var plaintextFound bool
	err = s.db.QueryRowContext(ctx, `
		SELECT key_hash,
		       EXISTS (SELECT 1 FROM api_keys a WHERE a::text LIKE '%' || $2 || '%')
		FROM api_keys WHERE id = $1`,
		key.ID, key.Plaintext[displayLen:],
	).Scan(&stored, &plaintextFound)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, hashKey(key.Plaintext)) {
		t.Errorf("key_hash = %x, want SHA-256 of the key", stored)
	}
	if plaintextFound {
		t.Error("the secret part of the key appears in api_keys")
	}
}

func TestLookupUnknownKey(t *testing.T) {
	s := newStore(t)
	if _, _, err := s.Create(context.Background(), "acme"); err != nil {
		t.Fatal(err)
	}

	unknown := KeyPrefix + randomHex(keyBytes)
	if _, _, err := s.Lookup(context.Background(), unknown); !errors.Is(err, ErrNotFound) {
		t.Errorf("Lookup of an unknown key: error = %v, want ErrNotFound", err)
	}
}

// TestRotation walks the rotation the package exists for: issue a second key,
// revoke the first, and the second keeps working throughout.
func TestRotation(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	tn, oldKey, err := s.Create(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	newKey, err := s.IssueKey(ctx, tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if newKey.Plaintext == oldKey.Plaintext || newKey.ID == oldKey.ID {
		t.Fatal("IssueKey returned the existing key")
	}

	for _, k := range []NewKey{oldKey, newKey} {
		if _, _, err := s.Lookup(ctx, k.Plaintext); err != nil {
			t.Fatalf("before revocation, key %s: %v", k.Prefix, err)
		}
	}

	if err := s.RevokeKey(ctx, oldKey.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Lookup(ctx, oldKey.Plaintext); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoked key: error = %v, want ErrNotFound", err)
	}
	if got, _, err := s.Lookup(ctx, newKey.Plaintext); err != nil || got.ID != tn.ID {
		t.Errorf("remaining key: tenant %q, error %v; want %q, nil", got.ID, err, tn.ID)
	}

	// Revoking again succeeds and keeps the first revocation time.
	revokedAt := func() time.Time {
		var at time.Time
		if err := s.db.QueryRowContext(ctx, `SELECT revoked_at FROM api_keys WHERE id = $1`, oldKey.ID).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	first := revokedAt()
	if err := s.RevokeKey(ctx, oldKey.ID); err != nil {
		t.Errorf("revoking a revoked key: %v", err)
	}
	if again := revokedAt(); !again.Equal(first) {
		t.Errorf("revoked_at moved from %v to %v on a second revoke", first, again)
	}
}

func TestIssueKeyUnknownTenant(t *testing.T) {
	s := newStore(t)
	if _, err := s.IssueKey(context.Background(), "tn_missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestRevokeUnknownKey(t *testing.T) {
	s := newStore(t)
	if err := s.RevokeKey(context.Background(), 12345); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}
