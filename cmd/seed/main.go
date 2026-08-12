// Command seed creates the first owner and the initial catalog.
//
// It is a bootstrap tool, not the way the catalog is maintained: adding and
// editing products happens in the app. This exists only so a brand new
// installation has somebody who can log in and something to sell.
//
// It is idempotent: running it twice changes nothing.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	_ "time/tzdata"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bryandrm/store-system/internal/auth"
	"github.com/bryandrm/store-system/internal/db"
)

// initialCatalog is what the stand actually sells. Prices are in cents.
var initialCatalog = []struct {
	name       string
	priceCents int64
}{
	{"Semillas", 25},
	{"Mani", 25},
	{"Mani dulce", 25},
	{"Mani japones", 50},
	{"Maranon", 100},
	{"Semillas de pepitoria", 25},
	{"Chocolates", 50},
	{"Churros", 50},
}

func main() {
	username := flag.String("username", "", "username for the initial owner (required)")
	password := flag.String("password", "", "password for the initial owner (required)")
	displayName := flag.String("name", "", "display name (defaults to the username)")
	skipCatalog := flag.Bool("skip-catalog", false, "create the user only, no products")
	flag.Parse()

	if *username == "" || *password == "" {
		fmt.Fprintln(os.Stderr, "usage: seed -username <user> -password <pass> [-name <name>]")
		os.Exit(2)
	}
	if len(*password) < 8 {
		fmt.Fprintln(os.Stderr, "error: the password must be at least 8 characters")
		os.Exit(2)
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		fmt.Fprintln(os.Stderr, "error: DATABASE_URL is required")
		os.Exit(2)
	}

	if err := run(databaseURL, *username, *password, *displayName, *skipCatalog); err != nil {
		slog.Error("seeding failed", "error", err)
		os.Exit(1)
	}
}

func run(databaseURL, username, password, displayName string, skipCatalog bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := db.NewPool(ctx, db.DefaultConfig(databaseURL))
	if err != nil {
		return err
	}
	defer pool.Close()

	if displayName == "" {
		displayName = username
	}

	return db.WithTx(ctx, pool, func(tx pgx.Tx) error {
		userID, created, err := ensureOwner(ctx, tx, username, password, displayName)
		if err != nil {
			return err
		}
		if created {
			slog.Info("owner created", "username", username, "user_id", userID)
		} else {
			slog.Info("the owner already existed, left untouched", "username", username)
		}

		if skipCatalog {
			return nil
		}
		return ensureCatalog(ctx, tx, userID)
	})
}

// ensureOwner creates the initial owner if the username is free.
//
// An existing user is never overwritten: silently resetting somebody's password
// because a command was re-run would be a very unpleasant surprise.
func ensureOwner(ctx context.Context, tx pgx.Tx, username, password, displayName string) (uuid.UUID, bool, error) {
	var existingID uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT id FROM users WHERE lower(username) = lower($1)`, username).Scan(&existingID)
	if err == nil {
		return existingID, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, fmt.Errorf("seed: could not look up the user: %w", err)
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return uuid.Nil, false, err
	}

	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx,
		`INSERT INTO users (id, username, display_name, password_hash, role)
		 VALUES ($1, $2, $3, $4, $5)`,
		id, username, displayName, hash, auth.RoleOwner); err != nil {
		return uuid.Nil, false, fmt.Errorf("seed: could not create the owner: %w", err)
	}

	return id, true, nil
}

// ensureCatalog inserts any missing product and gives it a starting price.
func ensureCatalog(ctx context.Context, tx pgx.Tx, ownerID uuid.UUID) error {
	for i, item := range initialCatalog {
		var productID uuid.UUID
		err := tx.QueryRow(ctx,
			`SELECT id FROM products WHERE lower(name) = lower($1)`, item.name).Scan(&productID)

		switch {
		case err == nil:
			slog.Info("product already existed", "name", item.name)
			continue
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("seed: could not look up product %q: %w", item.name, err)
		}

		productID = uuid.Must(uuid.NewV7())
		if _, err := tx.Exec(ctx,
			`INSERT INTO products (id, name, sort_order) VALUES ($1, $2, $3)`,
			productID, item.name, i); err != nil {
			return fmt.Errorf("seed: could not create product %q: %w", item.name, err)
		}

		// The price is a ledger entry, not a column, so seeding one means
		// opening its history rather than setting a field.
		if _, err := tx.Exec(ctx,
			`INSERT INTO product_prices (id, product_id, price_cents, effective_from, created_by_user_id)
			 VALUES ($1, $2, $3, now(), $4)`,
			uuid.Must(uuid.NewV7()), productID, item.priceCents, ownerID); err != nil {
			return fmt.Errorf("seed: could not set the price for %q: %w", item.name, err)
		}

		if err := db.RecordChange(ctx, tx, db.EntityProduct, productID, db.OpInsert,
			map[string]any{"id": productID, "name": item.name, "sort_order": i}); err != nil {
			return err
		}

		slog.Info("product created", "name", item.name, "price_cents", item.priceCents)
	}

	return nil
}
