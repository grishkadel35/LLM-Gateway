package db

import (
	"context"
	"database/sql"
	"testing"

	"github.com/grishkadel/llm-gateway/internal/db/dbtest"
)

func tableExists(t *testing.T, conn *sql.DB, table string) bool {
	t.Helper()

	var exists bool
	err := conn.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`,
		table,
	).Scan(&exists)
	if err != nil {
		t.Fatal(err)
	}
	return exists
}

func columns(t *testing.T, conn *sql.DB, table string) map[string]bool {
	t.Helper()

	rows, err := conn.Query(
		`SELECT column_name FROM information_schema.columns WHERE table_name = $1`,
		table,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	cols := map[string]bool{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		cols[c] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return cols
}

// TestMigrateUpDownUp applies every migration, checks the schema, rolls it all
// back, then applies it again. The round trip proves each Down undoes its Up;
// a broken Down usually goes unnoticed until the day it's needed.
func TestMigrateUpDownUp(t *testing.T) {
	ctx := context.Background()
	conn := dbtest.New(t)

	applied, err := Migrate(ctx, conn)
	if err != nil {
		t.Fatalf("migrating up: %v", err)
	}
	if len(applied) == 0 {
		t.Fatal("no migrations applied to an empty database")
	}

	for _, table := range []string{"tenants", "api_keys", "usage_logs"} {
		if !tableExists(t, conn, table) {
			t.Fatalf("table %s missing after migrating up", table)
		}
	}
	cols := columns(t, conn, "usage_logs")
	for _, c := range []string{"request_id", "tenant_id", "api_key_id", "provider", "model", "endpoint", "status", "cache_write_tokens", "cost_micros"} {
		if !cols[c] {
			t.Errorf("usage_logs has no %s column", c)
		}
	}
	cols = columns(t, conn, "api_keys")
	for _, c := range []string{"tenant_id", "key_hash", "key_prefix", "revoked_at"} {
		if !cols[c] {
			t.Errorf("api_keys has no %s column", c)
		}
	}

	again, err := Migrate(ctx, conn)
	if err != nil {
		t.Fatalf("migrating up a second time: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second Migrate applied %d migrations, want 0", len(again))
	}

	p, err := newProvider(conn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(ctx, 0); err != nil {
		t.Fatalf("migrating down: %v", err)
	}
	for _, table := range []string{"tenants", "api_keys", "usage_logs"} {
		if tableExists(t, conn, table) {
			t.Errorf("table %s still exists after migrating down", table)
		}
	}

	if _, err := Migrate(ctx, conn); err != nil {
		t.Fatalf("migrating up after down: %v", err)
	}
}
