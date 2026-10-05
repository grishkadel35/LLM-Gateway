// Package dbtest gives tests a throwaway Postgres database.
//
// It lives outside the _test.go files because more than one package's tests
// need it: Go only shares test helpers between packages through an ordinary,
// importable package (net/http/httptest is the standard library's example).
package dbtest

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// New returns a connection to a fresh, empty database that is dropped when
// the test ends. Tests migrate it themselves, and may migrate it down as well
// as up, so it must never be the database DATABASE_URL names, which may hold
// real dev data.
//
// Tests that need Postgres skip when DATABASE_URL is unset, so a plain
// `go test ./...` still works without a database.
func New(t *testing.T) *sql.DB {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres tests")
	}

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })

	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "gateway_test_" + hex.EncodeToString(b)

	// Identifiers can't be query parameters; name is generated above, so
	// splicing it in is safe.
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("creating test database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec("DROP DATABASE " + name + " WITH (FORCE)"); err != nil {
			t.Errorf("dropping test database: %v", err)
		}
	})

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name

	conn, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	// Registered after the DROP cleanup, so it runs first: cleanups run in
	// reverse order.
	t.Cleanup(func() { conn.Close() })
	return conn
}
