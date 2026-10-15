// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Command scan-from-go runs the DETECTED on-disk scan from your own Go program instead of the CLI. It
// opens the database named by POSTGRES_TEST_DSN, applies the bundled schema, scans a repository's
// committed harness with ondisk.Scan, and prints each finding and the scan's detector coverage. Use
// this when you want the findings as Go values — to filter, route or store them — rather than as CLI
// text. The scan needs a local Postgres because the engine persists its findings; set POSTGRES_TEST_DSN
// to one you can write to (for example the database the repository's docker-compose starts).
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/tetriz-ai/tetriz-harness/recommendations"
	"github.com/tetriz-ai/tetriz-harness/recommendations/ondisk"
)

func main() {
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "set POSTGRES_TEST_DSN to a Postgres you can write to")
		os.Exit(1)
	}

	// The bundled sample harness lives beside the on-disk reader; resolve it relative to this file so
	// the example runs from any working directory.
	_, thisFile, _, _ := runtime.Caller(0)
	sampleRepo := filepath.Join(filepath.Dir(thisFile), "..", "..", "ondisk", "testdata", "sample-repo")

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "open database:", err)
		os.Exit(1)
	}
	if err := recommendations.RunMigrations(db); err != nil {
		fmt.Fprintln(os.Stderr, "run migrations:", err)
		os.Exit(1)
	}

	result, err := ondisk.Scan(context.Background(), db, ondisk.Options{Dir: sampleRepo})
	if err != nil {
		fmt.Fprintln(os.Stderr, "scan:", err)
		os.Exit(1)
	}

	fmt.Printf("DETECTED findings: %d\n", len(result.Findings))
	for _, f := range result.Findings {
		fmt.Printf("  [%s] %s (priority %d)\n", f.Dimension, f.Title, f.Priority)
	}
	fmt.Printf("ran %d detector families; skipped %d for lack of on-disk input\n",
		len(result.Coverage.Ran), len(result.Coverage.Skipped))
}
