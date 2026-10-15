// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package testsupport holds the shared test helpers for the recommendations engine and its DAO
// layer: opening an isolated, rolled-back transaction against a throwaway Postgres and loading the
// migration SQL into it. It is imported only by _test.go files. The real-Postgres suites skip
// cleanly when neither POSTGRES_TEST_DSN nor HARNESS_TEST_DATABASE_URL is set, so a plain
// `go test ./...` on a machine with no database still passes.
package testsupport

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// testDBOutcome classifies what OpenTestTx must do for the current environment. Splitting the decision
// out of OpenTestTx lets it be asserted directly, without a *testing.T, so the set-but-unreachable path
// (which FATALs a real test) is itself under test.
type testDBOutcome int

const (
	// outcomeSkipNoDSN: neither DSN var is set — the real-DB suite is opt-in, skip cleanly.
	outcomeSkipNoDSN testDBOutcome = iota
	// outcomeFatal: a DSN is set but the database could not be opened or reached — a false green.
	outcomeFatal
	// outcomeReady: a usable connection is open.
	outcomeReady
)

// resolveTestDB reads the test DSN from the environment and tries to reach the database. It never
// touches *testing.T, so the three outcomes are unit-testable. The DSN is read from POSTGRES_TEST_DSN,
// falling back to HARNESS_TEST_DATABASE_URL (POSTGRES_TEST_DSN wins when both are set); gorm's pgx
// driver accepts both the keyword ("host=... dbname=...") and URL ("postgres://...") forms.
func resolveTestDB() (testDBOutcome, *gorm.DB, error) {
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		dsn = os.Getenv("HARNESS_TEST_DATABASE_URL")
	}
	if dsn == "" {
		return outcomeSkipNoDSN, nil, nil
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return outcomeFatal, nil, fmt.Errorf("opening the test DB: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return outcomeFatal, nil, fmt.Errorf("resolving sql.DB: %w", err)
	}
	if err := sqlDB.Ping(); err != nil {
		return outcomeFatal, nil, fmt.Errorf("the test DB is unreachable: %w", err)
	}
	return outcomeReady, db, nil
}

// migrationsDir resolves the recommendations/migrations directory relative to this source file, so
// the loader works regardless of which package's test is running (the test's working directory is
// its own package dir, which differs between recommendations/ and recommendations/dao/).
func migrationsDir() string {
	_, thisFile, _, _ := runtime.Caller(0)
	// thisFile = <repo>/recommendations/internal/testsupport/testsupport.go
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
}

// OpenTestTx opens a Postgres connection from the test DSN, begins a transaction, and registers a
// rollback at test cleanup so the test leaves no state behind. The DSN is read from POSTGRES_TEST_DSN,
// falling back to HARNESS_TEST_DATABASE_URL (POSTGRES_TEST_DSN wins when both are set) so the suite
// runs under the control plane's test DSN as well as the local/examples convention; gorm's pgx
// driver accepts both the keyword ("host=... dbname=...") and URL ("postgres://...") DSN forms. It
// SKIPs the test only when neither DSN var is set — the real-DB suites are opt-in, never gated behind
// a build tag that nothing builds. When a DSN IS set but the server cannot be opened or reached, it
// FATALs instead of skipping: a suite someone has explicitly pointed at a database must not pass green
// having silently tested nothing against an unreachable or mistyped target.
func OpenTestTx(t *testing.T) *gorm.DB {
	t.Helper()
	outcome, db, err := resolveTestDB()
	switch outcome {
	case outcomeSkipNoDSN:
		t.Skip("neither POSTGRES_TEST_DSN nor HARNESS_TEST_DATABASE_URL set; skipping real-Postgres test")
	case outcomeFatal:
		t.Fatalf("test DSN is set but the test DB is not usable: %v", err)
	}
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatalf("beginning tx: %v", tx.Error)
	}
	t.Cleanup(func() { tx.Rollback() })
	// `go test ./...` runs packages in parallel against one database, and their fixtures share ids, so
	// two suites writing the same rows can deadlock. One transaction-scoped lock makes every real-DB
	// test take its turn; the rollback releases it.
	if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", testDBLockKey).Error; err != nil {
		t.Fatalf("taking the test DB lock: %v", err)
	}
	return tx
}

// testDBLockKey is the advisory lock every real-DB test holds for its transaction.
const testDBLockKey = 0x7465747269

// LoadMigrations applies the named migration files into tx, statement by statement (pgx's prepared
// path rejects a multi-statement string). Line comments are stripped first because the migrations'
// comment prose contains semicolons, which would otherwise split a statement mid-comment.
func LoadMigrations(t *testing.T, tx *gorm.DB, names ...string) {
	t.Helper()
	dir := migrationsDir()
	for _, name := range names {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("reading migration %s: %v", name, err)
		}
		var stripped strings.Builder
		for _, line := range strings.Split(string(body), "\n") {
			if idx := strings.Index(line, "--"); idx >= 0 {
				line = line[:idx]
			}
			stripped.WriteString(line)
			stripped.WriteString("\n")
		}
		for _, stmt := range strings.Split(stripped.String(), ";") {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if err := tx.Exec(stmt).Error; err != nil {
				t.Fatalf("applying migration %s: %v", name, err)
			}
		}
	}
}
