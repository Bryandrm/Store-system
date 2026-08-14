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
	if err := run(); err != nil {
		slog.Error("the server could not start", "error", err)
		os.Exit(1)
	}
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
