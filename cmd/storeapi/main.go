// Command storeapi is the store-system HTTP server.
//
// This file is wiring only: config, pool, migrations, server lifecycle. The
// routes themselves live in internal/api so the integration tests can mount the
// same router the server runs.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	// Embeds the timezone database so a distroless image can resolve
	// America/El_Salvador without a tzdata layer.
	_ "time/tzdata"

	"github.com/bryandrm/store-system/internal/api"
	"github.com/bryandrm/store-system/internal/auth"
	"github.com/bryandrm/store-system/internal/config"
	"github.com/bryandrm/store-system/internal/db"
)

func main() {
	// The container healthcheck runs this binary against itself.
	//
	// distroless has no shell and no curl, which is the point of using it, so
	// the process has to be able to check its own liveness. The alternative
	// would be adding a shell to the runtime image purely so Docker can call
	// wget, which gives back exactly what distroless was chosen to remove.
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(healthcheck())
	}

	// Applies migrations and exits. The server does this on startup anyway, so
	// this exists for the cases where starting a server is the wrong tool:
	// preparing a database in CI, and running migrations by hand before a
	// deploy when a change needs checking before traffic reaches it.
	if len(os.Args) > 1 && os.Args[1] == "-migrate-only" {
		if err := migrateOnly(); err != nil {
			slog.Error("migration failed", "error", err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil {
		slog.Error("the server could not start", "error", err)
		os.Exit(1)
	}
}

// migrateOnly applies migrations and returns.
//
// It reads the environment directly rather than going through config.Load,
// which requires ALLOWED_ORIGINS and the rest of the serving configuration.
// Demanding a CORS origin in order to run a migration would be nonsense, and
// the kind of nonsense that gets worked around with a dummy value.
func migrateOnly() error {
	setupLogging(false)

	url := os.Getenv("MIGRATION_DATABASE_URL")
	if url == "" {
		url = os.Getenv("DATABASE_URL")
	}
	if url == "" {
		return errors.New("MIGRATION_DATABASE_URL or DATABASE_URL is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	return db.Migrate(ctx, url)
}

// healthcheck returns 0 when the local server answers /healthz.
func healthcheck() int {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1" + addr + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	setupLogging(cfg.Production)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Migrations run at startup with a single replica, and the resulting
	// version is logged. No separate migration job: one binary, one deploy.
	if err := db.Migrate(ctx, cfg.MigrationURL); err != nil {
		return err
	}

	pool, err := db.NewPool(ctx, db.DefaultConfig(cfg.DatabaseURL))
	if err != nil {
		return err
	}
	defer pool.Close()

	handler, err := api.New(api.Deps{
		Pool:           pool,
		AllowedOrigins: cfg.AllowedOrigins,
		TrustProxy:     cfg.TrustProxy,
		LoginLimits: auth.Limits{
			PerIPPerMinute:     cfg.LoginPerIPPerMinute,
			PerUsernamePerHour: cfg.LoginPerUsernamePerHour,
		},
	})
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:    cfg.Addr,
		Handler: handler,
		// A phone on bad mobile data can be slow; these are generous but not
		// unbounded, because an unbounded read is a slow-loris invitation.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("server listening",
			"addr", cfg.Addr,
			"production", cfg.Production,
			"allowed_origins", cfg.AllowedOrigins)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return err
	}

	slog.Info("shutdown complete")
	return nil
}

func setupLogging(production bool) {
	var handler slog.Handler
	if production {
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	} else {
		handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug})
	}
	slog.SetDefault(slog.New(handler))
}
