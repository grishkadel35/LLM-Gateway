// Package db owns the gateway's Postgres schema.
//
// The migration files are embedded into the binary, so a build always carries
// exactly the schema its code was written against: there is no separate
// directory to ship, or to forget to ship.
package db

import (
	"context"
	"database/sql"
	"embed"
	"io/fs"

	"github.com/pressly/goose/v3"
)

// Go note: a //go:embed directive makes the compiler copy matching files into
// the binary, readable at runtime through an fs.FS. It must sit directly above
// a package-level variable of type embed.FS, string or []byte.
//
//go:embed migrations/*.sql
var migrations embed.FS

// newProvider returns a goose provider over the embedded migrations.
//
// goose's Provider API keeps all state in the value it returns, unlike its
// older package-level functions, which share global settings.
func newProvider(conn *sql.DB) (*goose.Provider, error) {
	// fs.Sub strips the "migrations/" prefix, so goose sees the SQL files at the
	// root of the filesystem it's given.
	fsys, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return nil, err
	}
	return goose.NewProvider(goose.DialectPostgres, conn, fsys)
}

// Migrate applies every pending migration in order and returns what it ran.
// An up-to-date database is not an error: it returns no results.
func Migrate(ctx context.Context, conn *sql.DB) ([]*goose.MigrationResult, error) {
	p, err := newProvider(conn)
	if err != nil {
		return nil, err
	}
	return p.Up(ctx)
}
