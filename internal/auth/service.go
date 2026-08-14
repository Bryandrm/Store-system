package auth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bryandrm/store-system/internal/httpx"
)

type ctxKey string

const ctxKeyIdentity ctxKey = "identity"

// IdentityFrom returns the authenticated identity. The bool is false on routes
// that did not go through Middleware.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(ctxKeyIdentity).(Identity)
	return id, ok
}

// MustIdentity is for handlers mounted behind Middleware, where the absence of
// an identity is a wiring bug rather than a runtime condition.
func MustIdentity(ctx context.Context) Identity {
	id, ok := IdentityFrom(ctx)
	if !ok {
		panic("auth: handler requires Middleware but none ran")
	}
	return id
}

// Service holds authentication state: the pool and the login rate limiters.
type Service struct {
	pool *pgxpool.Pool

	byIP       *loginLimiter
	byUsername *loginLimiter

	// trustProxy tells the service whether X-Forwarded-For can be believed.
	// It must be false when the API is reachable directly: otherwise anyone can
	// sidestep the per-IP limit by making up a header.
	trustProxy bool

	// dummyHash makes a failed lookup cost the same as a failed password. A
	// login that returns instantly for unknown users is a username oracle.
	dummyHash string
}

// NewService builds the auth service. Setting trustProxy is a deployment
// decision: true behind Caddy, false when the API is exposed directly.
func NewService(pool *pgxpool.Pool, trustProxy bool, limits Limits) (*Service, error) {
	dummy, err := HashPassword("this password is never valid for any account")
	if err != nil {
		return nil, err
	}

	return &Service{
		pool:       pool,
		byIP:       newLoginLimiter(time.Minute/time.Duration(limits.PerIPPerMinute), limits.PerIPPerMinute),
		byUsername: newLoginLimiter(time.Hour/time.Duration(limits.PerUsernamePerHour), limits.PerUsernamePerHour),
		trustProxy: trustProxy,
		dummyHash:  dummy,
	}, nil
}

// Middleware requires a valid bearer token.
func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			httpx.Fail(w, r, httpx.ErrUnauthenticated)
			return
		}

		id, err := Authenticate(r.Context(), s.pool, token)
		if err != nil {
			// These three map to distinct client behaviour. On expiry or
			// revocation the PWA pauses syncing but keeps the outbox and lets
			// the user go on selling, so collapsing them into one code would
			// make the client unable to tell "log in again" from "something
			// broke".
			switch {
			case errors.Is(err, ErrSessionExpired):
				httpx.Fail(w, r, httpx.ErrTokenExpired)
			case errors.Is(err, ErrSessionRevoked):
				httpx.Fail(w, r, httpx.ErrTokenRevoked)
			case errors.Is(err, ErrSessionNotFound):
				httpx.Fail(w, r, httpx.ErrUnauthenticated)
			default:
				httpx.Fail(w, r, err)
			}
			return
		}

		ctx := context.WithValue(r.Context(), ctxKeyIdentity, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireRole restricts a route to one role. This plus a single column is the
// entire authorization model.
func RequireRole(role string) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := IdentityFrom(r.Context())
			if !ok {
				httpx.Fail(w, r, httpx.ErrUnauthenticated)
				return
			}
			if id.Role != role {
				httpx.Fail(w, r, httpx.ErrForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

type loginRequest struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	DeviceLabel string `json:"device_label"`
}

func (r *loginRequest) validate() []httpx.FieldDetail {
	var problems []httpx.FieldDetail

	if strings.TrimSpace(r.Username) == "" {
		problems = append(problems, httpx.FieldDetail{Field: "username", Code: "REQUIRED"})
	}
	if r.Password == "" {
		problems = append(problems, httpx.FieldDetail{Field: "password", Code: "REQUIRED"})
	}
	if len(r.DeviceLabel) > 120 {
		problems = append(problems, httpx.FieldDetail{Field: "device_label", Code: "TOO_LONG"})
	}
	return problems
}

type loginResponse struct {
	Token       string `json:"token"`
	UserID      string `json:"user_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

// HandleLogin verifies credentials and issues a token.
//
// The token is returned exactly once, here. Only its SHA-256 is stored.
func (s *Service) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.byIP.allow(clientIP(r, s.trustProxy)) {
		httpx.Fail(w, r, httpx.ErrRateLimited)
		return
	}

	var req loginRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if problems := req.validate(); len(problems) > 0 {
		httpx.Fail(w, r, httpx.ErrValidation.WithDetails(problems...))
		return
	}

	username := strings.ToLower(strings.TrimSpace(req.Username))
	if !s.byUsername.allow(username) {
		httpx.Fail(w, r, httpx.ErrRateLimited)
		return
	}

	var (
		userID       uuid.UUID
		displayName  string
		role         string
		passwordHash string
		isActive     bool
	)
	err := s.pool.QueryRow(r.Context(),
		`SELECT id, display_name, role, password_hash, is_active
		 FROM users WHERE lower(username) = $1`, username,
	).Scan(&userID, &displayName, &role, &passwordHash, &isActive)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Spend the same work as a real verification so response time does not
		// reveal whether the username exists.
		_, _ = VerifyPassword(req.Password, s.dummyHash)
		httpx.Fail(w, r, errInvalidCredentials)
		return
	case err != nil:
		httpx.Fail(w, r, err)
		return
	}

	ok, err := VerifyPassword(req.Password, passwordHash)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	// A deactivated user is rejected with the same message as a bad password:
	// telling an attacker the account exists but is disabled is free information.
	if !ok || !isActive {
		httpx.Fail(w, r, errInvalidCredentials)
		return
	}

	deviceLabel := strings.TrimSpace(req.DeviceLabel)
	if deviceLabel == "" {
		deviceLabel = "dispositivo sin nombre"
	}

	// A single INSERT needs no transaction.
	token, err := CreateSession(r.Context(), s.pool, userID, deviceLabel)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	httpx.OK(w, r, http.StatusOK, "Sesion iniciada", loginResponse{
		Token:       token,
		UserID:      userID.String(),
		Username:    username,
		DisplayName: displayName,
		Role:        role,
	})
}

// HandleLogout revokes the presented token.
func (s *Service) HandleLogout(w http.ResponseWriter, r *http.Request) {
	token, ok := bearerToken(r)
	if !ok {
		httpx.Fail(w, r, httpx.ErrUnauthenticated)
		return
	}
	if err := RevokeSession(r.Context(), s.pool, token); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, "Sesion cerrada", nil)
}

type meResponse struct {
	UserID      string `json:"user_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

// HandleMe returns the current identity. The PWA calls it on boot to confirm a
// stored token is still good before trusting the local cache.
func (s *Service) HandleMe(w http.ResponseWriter, r *http.Request) {
	id := MustIdentity(r.Context())
	httpx.OK(w, r, http.StatusOK, "", meResponse{
		UserID:      id.UserID.String(),
		Username:    id.Username,
		DisplayName: id.DisplayName,
		Role:        id.Role,
	})
}

// errInvalidCredentials is deliberately identical for unknown user, wrong
// password and deactivated account.
var errInvalidCredentials = httpx.ErrUnauthenticated.
	WithMessage("Usuario o contrasena incorrectos")

// bearerToken pulls the token out of the Authorization header.
func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", false
	}
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	return token, token != ""
}

// clientIP resolves the address used for rate limiting.
//
// X-Forwarded-For is only consulted when the deployment says a proxy is in
// front, and then only its LAST entry, which is the one our own proxy appended.
// Trusting the first entry would let a client bypass the limit by sending a
// made-up header.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
