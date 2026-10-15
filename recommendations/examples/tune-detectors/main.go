// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Command tune-detectors runs the DETECTED on-disk scan twice over the same repository: once with the
// built-in detector settings and once with a recommendations.DetectorConfig that adds a vendor-surface
// marker. The sample repository's CLAUDE.md points the agent at .windsurfrules, which is not a built-in
// marker, so only the tuned scan reports it. It uses public imports only; set POSTGRES_TEST_DSN to a
// Postgres you can write to.
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

const vendorSurfaceTitle = "File points Claude at another tool's config"

func main() {
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "set POSTGRES_TEST_DSN to a Postgres you can write to")
		os.Exit(1)
	}

	_, thisFile, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(thisFile), "..", "..", "ondisk", "testdata", "custom-marker-repo")

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "open database:", err)
		os.Exit(1)
	}
	if err := recommendations.RunMigrations(db); err != nil {
		fmt.Fprintln(os.Stderr, "run migrations:", err)
		os.Exit(1)
	}

	// Built-in settings: a nil DetectorConfig.
	report(db, "built-in markers", ondisk.Options{Dir: repo})

	// Start from the defaults and change one knob. Every field left unset keeps its built-in value.
	cfg := recommendations.DefaultDetectorConfig()
	cfg.VendorSurfaceMarkers = append(cfg.VendorSurfaceMarkers, ".windsurfrules")
	report(db, "with .windsurfrules added", ondisk.Options{Dir: repo, DetectorConfig: &cfg})
}

func report(db *gorm.DB, label string, opts ondisk.Options) {
	result, err := ondisk.Scan(context.Background(), db, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scan:", err)
		os.Exit(1)
	}
	var vendorSurface []ondisk.Finding
	for _, f := range result.Findings {
		if f.Title == vendorSurfaceTitle {
			vendorSurface = append(vendorSurface, f)
		}
	}
	fmt.Printf("%s: %d vendor-surface finding(s)\n", label, len(vendorSurface))
	for _, f := range vendorSurface {
		fmt.Printf("  [%s] %s (priority %d)\n", f.Dimension, f.Title, f.Priority)
	}
}
