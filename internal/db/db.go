// Package db owns Postgres access: the pool, the embedded migrations and the
// transaction helper.
//
// The rule that holds backend integrity together: EVERY write goes through
// WithTx. No loose pool.Exec in domain code. One operation is one transaction,
// and if anything fails nothing is left half-applied.
package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Config holds the pool parameters.
//
// MaxConns is deliberately low: every Postgres backend is a process costing
// 5-10 MB of RSS and the e2-micro has 1 GB total. A careless max_connections is
// the most common way to OOM a small box.
type Config struct {
	URL             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
}

// DefaultConfig returns values sized for the e2-micro.
func DefaultConfig(url string) Config {
	return Config{
		URL:             url,
		MaxConns:        8,
		MinConns:        2,
		MaxConnLifetime: 30 * time.Minute,
		MaxConnIdleTime: 5 * time.Minute,
	}
}

// NewPool opens the pool and verifies the database answers.
func NewPool(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid database URL: %w", err)
	}

	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime

	// Nothing in the system may depend on the server's timezone: everything is
	// stored in UTC and rendered in America/El_Salvador on the client.
	poolCfg.ConnConfig.RuntimeParams["timezone"] = "UTC"

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("could not create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database is not responding: %w", err)
	}

	return pool, nil
}

// Migrate applies the migrations embedded in the binary.
//
// It runs at API startup, with a single replica, and logs the resulting
// version. There is no separate migration job: one binary, one deploy.
func Migrate(ctx context.Context, url string) error {
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("goose dialect: %w", err)
	}

	// goose speaks database/sql; stdlib.OpenDB wraps the pgx config so no
	// separate driver is needed.
	connCfg, err := pgx.ParseConfig(url)
	if err != nil {
		return fmt.Errorf("invalid database URL: %w", err)
	}
	sqlDB := stdlib.OpenDB(*connCfg)
	defer sqlDB.Close()

	if err := goose.UpContext(ctx, sqlDB, "migrations"); err != nil {
		return fmt.Errorf("migration failed: %w", err)
	}

	version, err := goose.GetDBVersionContext(ctx, sqlDB)
	if err != nil {
		return fmt.Errorf("could not read database version: %w", err)
	}
	slog.Info("migrations applied", "version", version)

	return nil
}

// MigrationsFS exposes the migrations to the test harness, which applies them
// to the template database.
func MigrationsFS() embed.FS { return migrationsFS }

// Querier is what *pgxpool.Pool and pgx.Tx have in common.
//
// It is the project's only interface, and it exists because it genuinely has
// two implementations: domain functions run inside a transaction when they
// arrive through /sync, and against the pool when they arrive through an
// administrative REST route.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// WithTx runs fn inside a transaction and guarantees rollback on error or panic.
//
// The deferred rollback is not decorative: without it, a panic inside fn would
// leave the transaction open, and an open transaction pins the snapshot's xmin,
// which FREEZES the sync feed for every device until
// idle_in_transaction_session_timeout fires.
func WithTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) (err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("could not begin transaction: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p) // re-raised: the recover middleware turns it into a 500
		}
		if err != nil {
			if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
				slog.Error("rollback failed", "error", rbErr)
			}
		}
	}()

	if err = fn(tx); err != nil {
		return err
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("could not commit transaction: %w", err)
	}
	return nil
}
