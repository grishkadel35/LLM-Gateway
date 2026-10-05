// Package tenant manages tenants and their gateway API keys.
//
// A key is shown in plaintext exactly once, when it is issued. Only its
// SHA-256 hash is stored, so a leaked database doesn't leak working keys.
// SHA-256 rather than bcrypt: keys are 256 bits of randomness, so a slow hash
// adds no protection, and a fast one allows an indexed lookup on every
// request instead of comparing against every stored hash.
//
// A tenant can hold several keys at once. Rotation is: issue a new key, move
// clients to it, revoke the old one. A revoked key stops working immediately,
// but its row stays so past usage still attributes to it.
package tenant

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

const (
	// KeyPrefix starts every gateway key, so one is recognizable on sight (in
	// a log, a config file, a secret scanner) and can't be mistaken for a
	// provider's key.
	KeyPrefix = "gw_"
	// keyBytes of randomness, hex-encoded after KeyPrefix.
	keyBytes = 32
	keyLen   = len(KeyPrefix) + 2*keyBytes
	// displayLen is how much of a key is stored in clear, e.g. "gw_a1b2c3":
	// enough to tell a tenant's keys apart, far too little to guess the rest.
	displayLen = len(KeyPrefix) + 6
)

// ErrNotFound reports a tenant, key or key ID that doesn't exist. Lookup also
// returns it for revoked and malformed keys: a caller presenting a bad key
// learns nothing about why it was rejected.
var ErrNotFound = errors.New("not found")

// Tenant is one customer of the gateway. Limits come from the column defaults
// in the schema until something sets them.
type Tenant struct {
	ID                    string
	Name                  string
	RateLimitTokensPerMin int64
	BudgetMicros          int64
	BudgetPeriod          string
	DefaultMaxTokens      int
	CreatedAt             time.Time
}

// APIKey describes a stored key. It never holds the key itself.
type APIKey struct {
	ID        int64
	TenantID  string
	Prefix    string
	CreatedAt time.Time
}

// NewKey is a key just issued: the only time its plaintext exists outside the
// client that holds it.
type NewKey struct {
	APIKey
	Plaintext string
}

// Store reads and writes tenants and keys in Postgres.
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// Create adds a tenant and issues its first key, in one transaction, so a
// tenant never exists without a way to use it.
func (s *Store) Create(ctx context.Context, name string) (Tenant, NewKey, error) {
	t := Tenant{ID: "tn_" + randomHex(8), Name: name}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Tenant{}, NewKey{}, err
	}
	// Go note: deferring Rollback is the standard pattern. After a successful
	// Commit it does nothing (it returns sql.ErrTxDone, ignored here); on any
	// early return it undoes the transaction.
	defer tx.Rollback()

	err = tx.QueryRowContext(ctx, `
		INSERT INTO tenants (id, name) VALUES ($1, $2)
		RETURNING rate_limit_tokens_per_min, budget_micros, budget_period, default_max_tokens, created_at`,
		t.ID, t.Name,
	).Scan(&t.RateLimitTokensPerMin, &t.BudgetMicros, &t.BudgetPeriod, &t.DefaultMaxTokens, &t.CreatedAt)
	if err != nil {
		return Tenant{}, NewKey{}, err
	}

	key, err := insertKey(ctx, tx, t.ID)
	if err != nil {
		return Tenant{}, NewKey{}, err
	}
	if err := tx.Commit(); err != nil {
		return Tenant{}, NewKey{}, err
	}
	return t, key, nil
}

// IssueKey gives an existing tenant another key. It returns ErrNotFound if the
// tenant doesn't exist.
func (s *Store) IssueKey(ctx context.Context, tenantID string) (NewKey, error) {
	return insertKey(ctx, s.db, tenantID)
}

// RevokeKey stops a key working. Revoking an already-revoked key succeeds and
// keeps the original revocation time. It returns ErrNotFound if no key has
// that ID.
func (s *Store) RevokeKey(ctx context.Context, keyID int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE api_keys SET revoked_at = COALESCE(revoked_at, NOW()) WHERE id = $1`,
		keyID,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Lookup finds the tenant a key belongs to. Unknown, revoked and malformed
// keys all return ErrNotFound.
func (s *Store) Lookup(ctx context.Context, key string) (Tenant, APIKey, error) {
	// Anything that can't be a gateway key is rejected without a query, so
	// junk in a credential header costs nothing.
	if len(key) != keyLen || !strings.HasPrefix(key, KeyPrefix) {
		return Tenant{}, APIKey{}, ErrNotFound
	}

	var t Tenant
	var k APIKey
	err := s.db.QueryRowContext(ctx, `
		SELECT t.id, t.name, t.rate_limit_tokens_per_min, t.budget_micros, t.budget_period,
		       t.default_max_tokens, t.created_at, k.id, k.key_prefix, k.created_at
		FROM api_keys k JOIN tenants t ON t.id = k.tenant_id
		WHERE k.key_hash = $1 AND k.revoked_at IS NULL`,
		hashKey(key),
	).Scan(&t.ID, &t.Name, &t.RateLimitTokensPerMin, &t.BudgetMicros, &t.BudgetPeriod,
		&t.DefaultMaxTokens, &t.CreatedAt, &k.ID, &k.Prefix, &k.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Tenant{}, APIKey{}, ErrNotFound
	}
	if err != nil {
		return Tenant{}, APIKey{}, err
	}
	k.TenantID = t.ID
	return t, k, nil
}

// queryRower is satisfied by both *sql.DB and *sql.Tx, so insertKey runs
// inside Create's transaction or on its own.
//
// Go note: interfaces are satisfied implicitly. Neither type declares that it
// implements queryRower; having the method is enough.
type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func insertKey(ctx context.Context, q queryRower, tenantID string) (NewKey, error) {
	plaintext := KeyPrefix + randomHex(keyBytes)
	k := NewKey{
		APIKey:    APIKey{TenantID: tenantID, Prefix: plaintext[:displayLen]},
		Plaintext: plaintext,
	}

	// INSERT ... SELECT inserts nothing when the tenant doesn't exist, which
	// surfaces as sql.ErrNoRows rather than a driver-specific foreign-key
	// error.
	err := q.QueryRowContext(ctx, `
		INSERT INTO api_keys (tenant_id, key_hash, key_prefix)
		SELECT id, $2, $3 FROM tenants WHERE id = $1
		RETURNING id, created_at`,
		tenantID, hashKey(plaintext), k.Prefix,
	).Scan(&k.ID, &k.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return NewKey{}, ErrNotFound
	}
	if err != nil {
		return NewKey{}, err
	}
	return k, nil
}

// hashKey is what the api_keys.key_hash column holds: SHA-256 of the whole
// key, prefix included.
func hashKey(key string) []byte {
	h := sha256.Sum256([]byte(key))
	return h[:]
}

// randomHex returns n random bytes, hex-encoded. crypto/rand.Read never
// returns an error (it crashes the program instead if the OS can't supply
// randomness), so there is nothing to check.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
