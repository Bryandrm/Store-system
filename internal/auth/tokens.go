package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bryandrm/store-system/internal/db"
)

// Session lifetime.
//
// 180 days with sliding renewal, and no rotation. This is not laziness: in an
// offline-first app a short-lived token expires on day two of a three-day
// stretch with no signal, and rotation would leave a device holding a token
// that was already superseded on the other phone. See ADR-002.
const (
	sessionLifetime  = 180 * 24 * time.Hour
	slidingThreshold = 90 * 24 * time.Hour
	tokenRandomBytes = 32
)

var (
	// ErrSessionNotFound covers an unknown, expired or revoked token. The
	// caller maps it onto the right error code; this layer does not guess.
	ErrSessionNotFound = errors.New("auth: session not found")
	ErrSessionExpired  = errors.New("auth: session expired")
	ErrSessionRevoked  = errors.New("auth: session revoked")
)

// Identity is who the request is acting as.
type Identity struct {
	UserID      uuid.UUID
	Username    string
	DisplayName string
	Role        string
}

// IsOwner reports whether the identity holds the owner role.
func (i Identity) IsOwner() bool { return i.Role == RoleOwner }

// The only two roles in the system. Authorization is one column and one
// middleware, not a policy layer: this is a store run by two people.
const (
	RoleOwner = "owner"
	RoleStaff = "staff"
)

// newToken produces an opaque bearer token and the digest that gets stored.
//
// The raw token is returned exactly once, at login. The database only ever
// holds its SHA-256, so a leaked database dump does not hand over live sessions.
// SHA-256 without a work factor is correct here: the token is 32 bytes of
// entropy from crypto/rand, so there is nothing to brute-force.
func newToken() (raw string, digest []byte, err error) {
	buf := make([]byte, tokenRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("auth: could not read random bytes: %w", err)
	}

	raw = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(raw))
	return raw, sum[:], nil
}

// tokenDigest hashes a presented token so it can be looked up.
func tokenDigest(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

// CreateSession issues a token for a user and stores its digest.
func CreateSession(ctx context.Context, q db.Querier, userID uuid.UUID, deviceLabel string) (string, error) {
	raw, digest, err := newToken()
	if err != nil {
		return "", err
	}

	_, err = q.Exec(ctx,
		`INSERT INTO sessions (token_sha256, user_id, device_label, expires_at)
		 VALUES ($1, $2, $3, now() + $4::interval)`,
		digest, userID, deviceLabel, sessionLifetime.String())
	if err != nil {
		return "", fmt.Errorf("auth: could not create session: %w", err)
	}

	return raw, nil
}

// Authenticate resolves a bearer token into an Identity.
//
// It also slides the expiry forward when the session is more than halfway
// through its life, so a device in daily use never gets logged out.
func Authenticate(ctx context.Context, q db.Querier, rawToken string) (Identity, error) {
	var (
		id        Identity
		expiresAt time.Time
		revokedAt *time.Time
		isActive  bool
	)

	err := q.QueryRow(ctx,
		`SELECT u.id, u.username, u.display_name, u.role, u.is_active,
		        s.expires_at, s.revoked_at
		 FROM sessions s
		 JOIN users u ON u.id = s.user_id
		 WHERE s.token_sha256 = $1`,
		tokenDigest(rawToken),
	).Scan(&id.UserID, &id.Username, &id.DisplayName, &id.Role, &isActive,
		&expiresAt, &revokedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return Identity{}, ErrSessionNotFound
	}
	if err != nil {
		return Identity{}, fmt.Errorf("auth: could not read session: %w", err)
	}

	if revokedAt != nil {
		return Identity{}, ErrSessionRevoked
	}
	if time.Now().After(expiresAt) {
		return Identity{}, ErrSessionExpired
	}
	// A deactivated user is treated as revoked rather than as a distinct case:
	// from the device's point of view the outcome is the same, and it keeps the
	// client from having to handle a fourth auth state.
	if !isActive {
		return Identity{}, ErrSessionRevoked
	}

	if time.Until(expiresAt) < slidingThreshold {
		// Best effort: failing to extend must not fail the request. Worst case
		// the session expires on schedule and the user logs in again.
		_, _ = q.Exec(ctx,
			`UPDATE sessions
			 SET expires_at = now() + $2::interval, last_seen_at = now()
			 WHERE token_sha256 = $1`,
			tokenDigest(rawToken), sessionLifetime.String())
	}

	return id, nil
}

// RevokeSession closes one session. Revoking is a plain write because the token
// is opaque: there is no denylist to maintain, which is the practical advantage
// over a JWT here.
func RevokeSession(ctx context.Context, q db.Querier, rawToken string) error {
	_, err := q.Exec(ctx,
		`UPDATE sessions SET revoked_at = now()
		 WHERE token_sha256 = $1 AND revoked_at IS NULL`,
		tokenDigest(rawToken))
	if err != nil {
		return fmt.Errorf("auth: could not revoke session: %w", err)
	}
	return nil
}
