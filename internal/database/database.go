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

// maxConns caps the pool, since every project shares sparkdb's connections.
const maxConns = 10

// Database is a connection pool, logged in as lighthouse_app.
type Database struct {
	pool *pgxpool.Pool
}

// Connect opens a pool for LIGHTHOUSE_DATABASE_URL and checks that the database answers.
func Connect(ctx context.Context, connString string) (*Database, error) {
	cfg, err := poolConfig(connString)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("LIGHTHOUSE_DATABASE_URL isn't a valid connection string: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("can't reach the database at LIGHTHOUSE_DATABASE_URL: %w", err)
	}
	return &Database{pool: pool}, nil
}

// poolConfig parses connString, capped at maxConns unless it sets pool_max_conns.
func poolConfig(connString string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("LIGHTHOUSE_DATABASE_URL isn't a valid connection string: %w", err)
	}
	if !strings.Contains(connString, "pool_max_conns") {
		cfg.MaxConns = maxConns
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
