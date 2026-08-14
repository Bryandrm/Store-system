// Command verify checks the database invariants that no CHECK constraint can
// express, and exits non-zero when any of them is violated.
//
//	DATABASE_URL=… go run ./cmd/verify
//
// Runs in CI after the integration tests, nightly in production before the
// backup, and — most importantly — against every restored backup. A backup that
// restores with broken invariants is worse than no backup, because counting
// rows makes it look fine.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	_ "time/tzdata"

	"github.com/bryandrm/store-system/internal/db"
	"github.com/bryandrm/store-system/internal/verify"
)

func main() {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		fmt.Fprintln(os.Stderr, "error: DATABASE_URL is required")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool, err := db.NewPool(ctx, db.DefaultConfig(databaseURL))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}
	defer pool.Close()

	violations, err := verify.Run(ctx, pool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}

	if len(violations) == 0 {
		fmt.Printf("OK  %d invariants hold\n", len(verify.Invariants))
		return
	}

	// Plain text rather than structured logging: a human reads this at 2am
	// while deciding whether a restored backup can be trusted.
	fmt.Fprintf(os.Stderr, "FAIL  %d of %d invariants violated\n\n",
		len(violations), len(verify.Invariants))

	for _, v := range violations {
		fmt.Fprintf(os.Stderr, "  %s  (%d row%s)\n", v.Invariant, v.Count, plural(v.Count))
		fmt.Fprintf(os.Stderr, "    %s\n", v.Description)
		for _, sample := range v.Samples {
			fmt.Fprintf(os.Stderr, "      %s\n", sample)
		}
		if v.Count > len(v.Samples) {
			fmt.Fprintf(os.Stderr, "      … and %d more\n", v.Count-len(v.Samples))
		}
		fmt.Fprintln(os.Stderr)
	}

	fmt.Fprintln(os.Stderr, "See docs/INTEGRITY.md for what to do about each one.")
	os.Exit(1)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
