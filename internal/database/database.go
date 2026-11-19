// Package database is Lighthouse's Postgres store, with its built-in migrations.
package database

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// maxConns caps the daemon's pool, since every project shares sparkdb's connections.
const maxConns = 10

// readerMaxConns caps lighthouse-api's pool: it only answers a few reads at a time.
const readerMaxConns = 4

// Database is a connection pool, logged in as lighthouse_app.
type Database struct {
	pool *pgxpool.Pool
}

// Connect opens a pool for LIGHTHOUSE_DATABASE_URL and checks that the database answers.
func Connect(ctx context.Context, connString string) (*Database, error) {
	return connect(ctx, "LIGHTHOUSE_DATABASE_URL", connString, maxConns)
}

// ConnectReader opens lighthouse-api's pool for LIGHTHOUSE_DATABASE_READER_URL, which can only read.
func ConnectReader(ctx context.Context, connString string) (*Database, error) {
	return connect(ctx, "LIGHTHOUSE_DATABASE_READER_URL", connString, readerMaxConns)
}

// connect opens a pool for connString, the setting key, and checks that the database answers.
func connect(ctx context.Context, key string, connString string, max int32) (*Database, error) {
	cfg, err := poolConfig(key, connString, max)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("%s isn't a valid connection string: %w", key, err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("can't reach the database at %s: %w", key, err)
	}
	return &Database{pool: pool}, nil
}

// poolConfig parses connString, capped at max unless it sets pool_max_conns.
func poolConfig(key string, connString string, max int32) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("%s isn't a valid connection string: %w", key, err)
	}
	if !strings.Contains(connString, "pool_max_conns") {
		cfg.MaxConns = max
	}
	return cfg, nil
}

// Ping checks that the database answers, giving up after 2 seconds.
func (d *Database) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return d.pool.Ping(ctx)
}

func (d *Database) Close() {
	d.pool.Close()
}

// IsUnreachable reports whether err means the database couldn't be reached at all.
func IsUnreachable(err error) bool {
	var connectErr *pgconn.ConnectError
	if !errors.As(err, &connectErr) {
		return false
	}
	var pgErr *pgconn.PgError
	return !errors.As(err, &pgErr)
}
