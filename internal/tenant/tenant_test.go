package tenant

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

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
