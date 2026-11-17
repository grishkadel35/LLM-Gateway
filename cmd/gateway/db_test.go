package main

import (
	"context"
	"database/sql"
	"os"
	"testing"
)

// TestOpenDBSizesThePool: openDB caps the pool at dbPoolSize and keeps that
// many connections idle, so a burst reuses them instead of dialing Postgres.
func TestOpenDBSizesThePool(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres tests")
	}
	conn, err := openDB(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if got := conn.Stats().MaxOpenConnections; got != dbPoolSize {
		t.Errorf("MaxOpenConnections = %d, want %d", got, dbPoolSize)
	}

	// Hold every connection the pool allows at once, then give them back:
	// all of them stay open, idle.
	ctx := context.Background()
	held := make([]*sql.Conn, dbPoolSize)
	for i := range held {
		if held[i], err = conn.Conn(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range held {
		c.Close()
	}
	if got := conn.Stats().Idle; got != dbPoolSize {
		t.Errorf("after returning %d connections, %d are idle, want all of them", dbPoolSize, got)
	}
}
