// Package api assembles the HTTP router.
//
// It lives here rather than in cmd/storeapi so the integration tests can mount
// the REAL router. A test that reaches into domain functions directly is not
// testing what runs in production — the point of that layer is the wiring, and
// the wiring is this file.
package api

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bryandrm/store-system/internal/auth"
	"github.com/bryandrm/store-system/internal/httpx"
	"github.com/bryandrm/store-system/internal/sync"
)

// Deps is everything the router needs to exist.
type Deps struct {
	Pool           *pgxpool.Pool
	AllowedOrigins []string
	TrustProxy     bool
	LoginLimits    auth.Limits
}

// New builds the complete handler, middleware included.
func New(deps Deps) (http.Handler, error) {
	authService, err := auth.NewService(deps.Pool, deps.TrustProxy, deps.LoginLimits)
	if err != nil {
		return nil, err
	}
	syncHandler := sync.NewHandler(deps.Pool)

	mux := http.NewServeMux()

	// Health checks sit outside /api/v1, without auth and without the response
	// envelope, so a load balancer never has to parse JSON to decide.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		pingCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := deps.Pool.Ping(pingCtx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("database unavailable"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})

	// Method and path patterns come from stdlib routing (Go 1.22+). No router
	// dependency is needed for eleven endpoints.
	mux.HandleFunc("POST /api/v1/auth/login", authService.HandleLogin)
	mux.Handle("POST /api/v1/auth/logout",
		authService.Middleware(http.HandlerFunc(authService.HandleLogout)))
	mux.Handle("GET /api/v1/auth/me",
		authService.Middleware(http.HandlerFunc(authService.HandleMe)))

	mux.Handle("GET /api/v1/bootstrap",
		authService.Middleware(http.HandlerFunc(syncHandler.HandleBootstrap)))
	mux.Handle("POST /api/v1/sync",
		authService.Middleware(http.HandlerFunc(syncHandler.HandleSync)))

	return httpx.Chain(mux,
		httpx.WithRequestID,
		httpx.WithLogging,
		httpx.WithRecover,
		httpx.WithCORS(deps.AllowedOrigins),
	), nil
}
