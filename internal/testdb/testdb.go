// Package testdb gives every test a real, throwaway Postgres database.
//
// No mocks, by explicit decision: mocking the database would make the project's
// most valuable tests (feed convergence, idempotency under concurrent
// transactions) exercise the mock instead of the system.
//
// How it works: once per run it creates store_test_template and migrates it.
// Each test then runs CREATE DATABASE ... TEMPLATE, which Postgres resolves by
// copying files: ~30 ms, full isolation.
//
// What it deliberately does NOT do: wrap each test in a transaction and roll
// back. That would be faster, but it would make the tests that need real,
// separate, concurrently committing transactions impossible to write, and those
// are exactly the ones that justify the whole design.
package testdb

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bryandrm/store-system/internal/db"
)

const (
	templateName = "store_test_template"

	// store_app's password in tests. In production the infrastructure assigns
	// it; here it is needed because the migration creates the role NOLOGIN on
	// purpose, so no secret lives in the repository.
	testAppPassword = "test_app_pwd"
)

// defaultAdminURL points at the Postgres from compose.dev.yml.
const defaultAdminURL = "postgres://store_migrator:dev_only_no_usar_en_produccion@localhost:5433/postgres?sslmode=disable"

var (
	setupOnce sync.Once
	setupErr  error
)

// DB is the pair of connections a test receives.
type DB struct {
	// App connects as store_app: the SAME role the API uses in production.
	//
	// This is deliberate. If tests ran as superuser, a missing GRANT would go
	// unnoticed until deploy, and an improper UPDATE would never be caught
	// because a superuser is allowed to do it.
	App *pgxpool.Pool

	// Admin connects as store_migrator. It is used only to set up scenarios and
	// for assertions that need to see more than the application can.
	Admin *pgxpool.Pool

	// Name is the throwaway database name, handy when debugging a test.
	Name string

	// AppURL lets a test open standalone connections when it needs genuinely
	// concurrent transactions (the feed convergence case).
	AppURL string
}

// New creates a throwaway database for this test and drops it afterwards.
//
//	func TestSomething(t *testing.T) {
//	    tdb := testdb.New(t)
//	    // use tdb.App as if it were the production pool
//	}
func New(t *testing.T) *DB {
	t.Helper()

	setupOnce.Do(func() { setupErr = ensureTemplate() })
	if setupErr != nil {
		t.Fatalf("could not prepare the template database: %v\n\n"+
			"Is Postgres running?  docker compose -f compose.dev.yml up -d", setupErr)
	}

	ctx := context.Background()
	name := fmt.Sprintf("store_test_%d_%d", time.Now().UnixNano()%1_000_000, rand.Uint32())

	admin, err := pgxpool.New(ctx, adminURL())
	if err != nil {
		t.Fatalf("could not connect as admin: %v", err)
	}
	defer admin.Close()

	if _, err := admin.Exec(ctx,
		fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, templateName)); err != nil {
		t.Fatalf("could not create database %s: %v", name, err)
	}

	appURL := urlForDatabase(adminURL(), name, "store_app", testAppPassword)
	adminDBURL := urlForDatabase(adminURL(), name, "", "")

	appPool, err := pgxpool.New(ctx, appURL)
	if err != nil {
		t.Fatalf("could not open the application pool: %v", err)
	}
	adminPool, err := pgxpool.New(ctx, adminDBURL)
	if err != nil {
		appPool.Close()
		t.Fatalf("could not open the admin pool: %v", err)
	}

	t.Cleanup(func() {
		appPool.Close()
		adminPool.Close()

		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		cleaner, err := pgxpool.New(cleanupCtx, adminURL())
		if err != nil {
			t.Logf("could not connect to clean up %s: %v", name, err)
			return
		}
		defer cleaner.Close()

		// Kill stragglers: a test that opened standalone connections and did
		// not close them would block the DROP and leave garbage between runs.
		_, _ = cleaner.Exec(cleanupCtx,
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			 WHERE datname = $1 AND pid <> pg_backend_pid()`, name)

		if _, err := cleaner.Exec(cleanupCtx,
			fmt.Sprintf("DROP DATABASE IF EXISTS %s", name)); err != nil {
			t.Logf("could not drop database %s: %v", name, err)
		}
	})

	return &DB{App: appPool, Admin: adminPool, Name: name, AppURL: appURL}
}

// ensureTemplate creates the template database and migrates it. Runs once per
// test process.
func ensureTemplate() error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	admin, err := pgxpool.New(ctx, adminURL())
	if err != nil {
		return fmt.Errorf("admin connection: %w", err)
	}
	defer admin.Close()

	// Rebuilt on every run: a stale template carrying an older schema is a
	// source of phantom failures that are painful to diagnose.
	_, _ = admin.Exec(ctx,
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		 WHERE datname = $1 AND pid <> pg_backend_pid()`, templateName)
	if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+templateName); err != nil {
		return fmt.Errorf("could not drop the previous template: %w", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+templateName); err != nil {
		return fmt.Errorf("could not create the template: %w", err)
	}

	templateURL := urlForDatabase(adminURL(), templateName, "", "")
	if err := db.Migrate(ctx, templateURL); err != nil {
		return fmt.Errorf("migrating the template: %w", err)
	}

	// The migration creates store_app NOLOGIN on purpose (credentials come from
	// the infrastructure). Tests have to grant it access.
	if _, err := admin.Exec(ctx,
		fmt.Sprintf("ALTER ROLE store_app LOGIN PASSWORD '%s'", testAppPassword)); err != nil {
		return fmt.Errorf("could not enable store_app: %w", err)
	}

	return nil
}

func adminURL() string {
	if v := os.Getenv("TEST_DATABASE_URL"); v != "" {
		return v
	}
	return defaultAdminURL
}

// urlForDatabase rewrites the URL to point at another database and, optionally,
// authenticate as another user.
func urlForDatabase(base, database, user, password string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	u.Path = "/" + strings.TrimPrefix(database, "/")
	if user != "" {
		u.User = url.UserPassword(user, password)
	}
	return u.String()
}
