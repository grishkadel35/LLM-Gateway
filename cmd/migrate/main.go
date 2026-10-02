// Command migrate applies the gateway's pending Postgres migrations, read from
// the copies embedded in internal/db. The database comes from DATABASE_URL,
// e.g. postgres://gateway:gateway@127.0.0.1:5432/gateway?sslmode=disable
// (`make migrate` supplies that default for the compose database).
package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"time"

	// Go note: a blank import (`_`) runs a package's init() without using any
	// of its names. pgx's stdlib package registers itself as the "pgx" driver
	// for database/sql there, which is what goose needs.
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/grishkadel/llm-gateway/internal/db"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if err := run(logger); err != nil {
		logger.Error("migration failed", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is not set")
	}

	// sql.Open only validates its arguments; the first query is what actually
	// connects, so a wrong host or password surfaces from Migrate below.
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	applied, err := db.Migrate(ctx, conn)
	if err != nil {
		return err
	}

	for _, r := range applied {
		logger.Info("applied migration", "file", r.Source.Path, "duration_ms", r.Duration.Milliseconds())
	}
	if len(applied) == 0 {
		logger.Info("database is up to date")
	}
	return nil
}
