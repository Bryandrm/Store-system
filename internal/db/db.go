// Package db concentra el acceso a Postgres: el pool, las migraciones
// embebidas y el helper transaccional.
//
// Regla que sostiene la integridad del backend: TODA escritura pasa por
// WithTx. Nada de pool.Exec suelto en codigo de dominio. Una operacion es una
// transaccion, y si algo falla no queda nada a medias.
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

// Config son los parametros del pool.
//
// MaxConns es deliberadamente bajo: cada backend de Postgres es un proceso de
// 5-10 MB de RSS y la e2-micro tiene 1 GB en total. Un max_connections
// descuidado es la forma mas comun de tumbar por OOM una caja chica.
type Config struct {
	URL             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
}

// DefaultConfig devuelve los valores pensados para la e2-micro.
func DefaultConfig(url string) Config {
	return Config{
		URL:             url,
		MaxConns:        8,
		MinConns:        2,
		MaxConnLifetime: 30 * time.Minute,
		MaxConnIdleTime: 5 * time.Minute,
	}
}

// NewPool abre el pool y verifica que la base responda.
func NewPool(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("URL de base invalida: %w", err)
	}

	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime

	// Nada en el sistema puede depender de la zona horaria del servidor: todo
	// se guarda en UTC y se renderiza en America/El_Salvador en el cliente.
	poolCfg.ConnConfig.RuntimeParams["timezone"] = "UTC"

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("no se pudo crear el pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("la base no responde: %w", err)
	}

	return pool, nil
}

// Migrate aplica las migraciones embebidas en el binario.
//
// Corre al arrancar la API, con una sola replica, y deja registrada la version
// en el log. No hay job de migracion separado: un binario, un despliegue.
func Migrate(ctx context.Context, url string) error {
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("dialecto de goose: %w", err)
	}

	// goose usa database/sql; stdlib.OpenDB envuelve la config de pgx sin
	// necesidad de un driver aparte.
	connCfg, err := pgx.ParseConfig(url)
	if err != nil {
		return fmt.Errorf("URL de base invalida: %w", err)
	}
	sqlDB := stdlib.OpenDB(*connCfg)
	defer sqlDB.Close()

	if err := goose.UpContext(ctx, sqlDB, "migrations"); err != nil {
		return fmt.Errorf("fallo al migrar: %w", err)
	}

	version, err := goose.GetDBVersionContext(ctx, sqlDB)
	if err != nil {
		return fmt.Errorf("no se pudo leer la version de la base: %w", err)
	}
	slog.Info("migraciones aplicadas", "version", version)

	return nil
}

// MigrationsFS expone las migraciones para el harness de tests, que las aplica
// sobre la base plantilla.
func MigrationsFS() embed.FS { return migrationsFS }

// Querier es lo que comparten *pgxpool.Pool y pgx.Tx.
//
// Es la unica interfaz del proyecto, y existe porque tiene dos
// implementaciones reales: las funciones de dominio corren dentro de una
// transaccion cuando entran por /sync, y sueltas contra el pool cuando entran
// por una ruta REST administrativa.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// WithTx ejecuta fn dentro de una transaccion y garantiza el rollback ante
// error o panic.
//
// El rollback en el defer no es decorativo: sin el, un panic dentro de fn
// dejaria la transaccion abierta, y una transaccion abierta fija el xmin del
// snapshot, lo que CONGELA el feed de sincronizacion para todos los
// dispositivos hasta que salte idle_in_transaction_session_timeout.
func WithTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) (err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("no se pudo iniciar la transaccion: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p) // se re-lanza: el middleware de recover lo convierte en 500
		}
		if err != nil {
			if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
				slog.Error("fallo el rollback", "error", rbErr)
			}
		}
	}()

	if err = fn(tx); err != nil {
		return err
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("no se pudo confirmar la transaccion: %w", err)
	}
	return nil
}
