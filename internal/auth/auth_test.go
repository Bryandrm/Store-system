package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bryandrm/store-system/internal/auth"
	"github.com/bryandrm/store-system/internal/testdb"
)

// seedUser inserts a user and returns its id.
func seedUser(t *testing.T, tdb *testdb.DB, username, password, role string) uuid.UUID {
	t.Helper()

	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("could not hash password: %v", err)
	}

	id := uuid.Must(uuid.NewV7())
	_, err = tdb.App.Exec(context.Background(),
		`INSERT INTO users (id, username, display_name, password_hash, role)
		 VALUES ($1, $2, $3, $4, $5)`,
		id, username, username, hash, role)
	if err != nil {
		t.Fatalf("could not insert user: %v", err)
	}
	return id
}

func TestHashPasswordRoundTrip(t *testing.T) {
	t.Parallel()

	const password = "mani japones 2026"

	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	ok, err := auth.VerifyPassword(password, hash)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Error("the correct password did not verify")
	}

	ok, err = auth.VerifyPassword("wrong password", hash)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if ok {
		t.Error("an incorrect password verified")
	}
}

// TestHashPasswordUsesFreshSalt catches the classic mistake of a fixed salt,
// which would let identical passwords be spotted across accounts.
func TestHashPasswordUsesFreshSalt(t *testing.T) {
	t.Parallel()

	a, err := auth.HashPassword("same password")
	if err != nil {
		t.Fatal(err)
	}
	b, err := auth.HashPassword("same password")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("two hashes of the same password are identical; the salt is not random")
	}
}

func TestVerifyPasswordRejectsMalformedHash(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{
		"", "not a hash", "$argon2id$", "$bcrypt$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA",
	} {
		if _, err := auth.VerifyPassword("x", bad); err == nil {
			t.Errorf("a malformed hash (%q) was accepted", bad)
		}
	}
}

func TestAuthenticateHappyPath(t *testing.T) {
	t.Parallel()

	tdb := testdb.New(t)
	ctx := context.Background()
	userID := seedUser(t, tdb, "bryan", "clave-de-prueba", auth.RoleOwner)

	token, err := auth.CreateSession(ctx, tdb.App, userID, "telefono de prueba")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	id, err := auth.Authenticate(ctx, tdb.App, token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.UserID != userID {
		t.Errorf("user id = %s, want %s", id.UserID, userID)
	}
	if !id.IsOwner() {
		t.Errorf("role = %q, want owner", id.Role)
	}
}

// TestSessionTokenIsNeverStoredRaw is the property that makes a database dump
// harmless: it must contain no material that can be replayed as a token.
func TestSessionTokenIsNeverStoredRaw(t *testing.T) {
	t.Parallel()

	tdb := testdb.New(t)
	ctx := context.Background()
	userID := seedUser(t, tdb, "bryan", "clave-de-prueba", auth.RoleOwner)

	token, err := auth.CreateSession(ctx, tdb.App, userID, "telefono")
	if err != nil {
		t.Fatal(err)
	}

	var matches int
	err = tdb.Admin.QueryRow(ctx,
		`SELECT count(*) FROM sessions WHERE encode(token_sha256, 'escape') = $1`,
		token).Scan(&matches)
	if err != nil {
		t.Fatal(err)
	}
	if matches != 0 {
		t.Error("the raw token appears in the sessions table")
	}
}

func TestAuthenticateRejectsUnknownToken(t *testing.T) {
	t.Parallel()

	tdb := testdb.New(t)

	_, err := auth.Authenticate(context.Background(), tdb.App, "a-token-that-was-never-issued")
	if !errors.Is(err, auth.ErrSessionNotFound) {
		t.Errorf("want ErrSessionNotFound, got %v", err)
	}
}

func TestAuthenticateRejectsRevokedSession(t *testing.T) {
	t.Parallel()

	tdb := testdb.New(t)
	ctx := context.Background()
	userID := seedUser(t, tdb, "bryan", "clave", auth.RoleOwner)

	token, err := auth.CreateSession(ctx, tdb.App, userID, "telefono")
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.RevokeSession(ctx, tdb.App, token); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}

	_, err = auth.Authenticate(ctx, tdb.App, token)
	if !errors.Is(err, auth.ErrSessionRevoked) {
		t.Errorf("want ErrSessionRevoked, got %v", err)
	}
}

func TestAuthenticateRejectsExpiredSession(t *testing.T) {
	t.Parallel()

	tdb := testdb.New(t)
	ctx := context.Background()
	userID := seedUser(t, tdb, "bryan", "clave", auth.RoleOwner)

	token, err := auth.CreateSession(ctx, tdb.App, userID, "telefono")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := tdb.Admin.Exec(ctx,
		`UPDATE sessions SET expires_at = now() - interval '1 day'`); err != nil {
		t.Fatal(err)
	}

	_, err = auth.Authenticate(ctx, tdb.App, token)
	if !errors.Is(err, auth.ErrSessionExpired) {
		t.Errorf("want ErrSessionExpired, got %v", err)
	}
}

// TestAuthenticateRejectsDeactivatedUser: deactivating a user must cut off the
// sessions they already hold, not just stop new logins.
func TestAuthenticateRejectsDeactivatedUser(t *testing.T) {
	t.Parallel()

	tdb := testdb.New(t)
	ctx := context.Background()
	userID := seedUser(t, tdb, "pareja", "clave", auth.RoleStaff)

	token, err := auth.CreateSession(ctx, tdb.App, userID, "telefono")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tdb.Admin.Exec(ctx,
		`UPDATE users SET is_active = FALSE WHERE id = $1`, userID); err != nil {
		t.Fatal(err)
	}

	_, err = auth.Authenticate(ctx, tdb.App, token)
	if !errors.Is(err, auth.ErrSessionRevoked) {
		t.Errorf("want ErrSessionRevoked, got %v", err)
	}
}

// TestAuthenticateSlidesExpiry: a device in daily use must never be logged out.
// The session is pushed back to just under the sliding threshold, so a single
// authentication has to extend it.
func TestAuthenticateSlidesExpiry(t *testing.T) {
	t.Parallel()

	tdb := testdb.New(t)
	ctx := context.Background()
	userID := seedUser(t, tdb, "bryan", "clave", auth.RoleOwner)

	token, err := auth.CreateSession(ctx, tdb.App, userID, "telefono")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := tdb.Admin.Exec(ctx,
		`UPDATE sessions SET expires_at = now() + interval '10 days'`); err != nil {
		t.Fatal(err)
	}

	var before time.Time
	if err := tdb.Admin.QueryRow(ctx, `SELECT expires_at FROM sessions`).Scan(&before); err != nil {
		t.Fatal(err)
	}

	if _, err := auth.Authenticate(ctx, tdb.App, token); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	var after time.Time
	if err := tdb.Admin.QueryRow(ctx, `SELECT expires_at FROM sessions`).Scan(&after); err != nil {
		t.Fatal(err)
	}

	if !after.After(before) {
		t.Errorf("expiry was not extended: before=%s after=%s", before, after)
	}
	if time.Until(after) < 100*24*time.Hour {
		t.Errorf("expiry extended only to %s; expected roughly 180 days out", after)
	}
}

// TestRevokeSessionIsIdempotent: two devices can log the same session out at
// once, and the second call must not fail or move revoked_at.
func TestRevokeSessionIsIdempotent(t *testing.T) {
	t.Parallel()

	tdb := testdb.New(t)
	ctx := context.Background()
	userID := seedUser(t, tdb, "bryan", "clave", auth.RoleOwner)

	token, err := auth.CreateSession(ctx, tdb.App, userID, "telefono")
	if err != nil {
		t.Fatal(err)
	}

	if err := auth.RevokeSession(ctx, tdb.App, token); err != nil {
		t.Fatal(err)
	}
	var first time.Time
	if err := tdb.Admin.QueryRow(ctx, `SELECT revoked_at FROM sessions`).Scan(&first); err != nil {
		t.Fatal(err)
	}

	if err := auth.RevokeSession(ctx, tdb.App, token); err != nil {
		t.Fatalf("second revocation failed: %v", err)
	}
	var second time.Time
	if err := tdb.Admin.QueryRow(ctx, `SELECT revoked_at FROM sessions`).Scan(&second); err != nil {
		t.Fatal(err)
	}

	if !first.Equal(second) {
		t.Errorf("the second revocation moved revoked_at: %s -> %s", first, second)
	}
}

// TestSessionsAreIndependent: revoking one device must not log out the other.
// Getting this wrong would mean losing a phone forces the partner out too.
func TestSessionsAreIndependent(t *testing.T) {
	t.Parallel()

	tdb := testdb.New(t)
	ctx := context.Background()
	userID := seedUser(t, tdb, "bryan", "clave", auth.RoleOwner)

	tokenA, err := auth.CreateSession(ctx, tdb.App, userID, "telefono A")
	if err != nil {
		t.Fatal(err)
	}
	tokenB, err := auth.CreateSession(ctx, tdb.App, userID, "telefono B")
	if err != nil {
		t.Fatal(err)
	}

	if err := auth.RevokeSession(ctx, tdb.App, tokenA); err != nil {
		t.Fatal(err)
	}

	if _, err := auth.Authenticate(ctx, tdb.App, tokenA); !errors.Is(err, auth.ErrSessionRevoked) {
		t.Errorf("device A: want ErrSessionRevoked, got %v", err)
	}
	if _, err := auth.Authenticate(ctx, tdb.App, tokenB); err != nil {
		t.Errorf("device B should still be valid, got %v", err)
	}
}
