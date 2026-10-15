// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package testsupport

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unreachableDSN points at a closed port so the connectivity probe fails fast rather than hanging.
const unreachableDSN = "host=127.0.0.1 port=1 user=nobody dbname=nope sslmode=disable connect_timeout=1"

// TestResolveTestDB_SetButUnreachableIsFatal pins the contract behind OpenTestTx's skip-vs-fatal split:
// a configured-but-unreachable DSN must be a fatal outcome, never the clean skip reserved for "no DSN
// set". A suite someone has pointed at a database must not pass green having tested nothing.
func TestResolveTestDB_SetButUnreachableIsFatal(t *testing.T) {
	testCases := []struct {
		name        string
		postgresDSN string
		harnessDSN  string
		wantOutcome testDBOutcome
	}{
		{
			name:        "neither var set skips cleanly",
			postgresDSN: "",
			harnessDSN:  "",
			wantOutcome: outcomeSkipNoDSN,
		},
		{
			name:        "POSTGRES_TEST_DSN set but unreachable is fatal",
			postgresDSN: unreachableDSN,
			harnessDSN:  "",
			wantOutcome: outcomeFatal,
		},
		{
			name:        "HARNESS_TEST_DATABASE_URL set but unreachable is fatal",
			postgresDSN: "",
			harnessDSN:  unreachableDSN,
			wantOutcome: outcomeFatal,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("POSTGRES_TEST_DSN", testCase.postgresDSN)
			t.Setenv("HARNESS_TEST_DATABASE_URL", testCase.harnessDSN)
			gotOutcome, _, err := resolveTestDB()
			if gotOutcome != testCase.wantOutcome {
				t.Fatalf("resolveTestDB outcome = %d, want %d (err=%v)", gotOutcome, testCase.wantOutcome, err)
			}
			if testCase.wantOutcome == outcomeFatal && err == nil {
				t.Fatal("fatal outcome must carry the underlying error, got nil")
			}
		})
	}
}

// TestDBSuitesUseSharedOpener keeps the skip-vs-fatal rule in one place: no test file under
// recommendations/ may read the test DSN or open a database itself. A suite with its own opener can
// turn a configured but unusable DSN into a skip, so it passes green having tested nothing; every
// real-Postgres suite goes through OpenTestTx instead.
func TestDBSuitesUseSharedOpener(t *testing.T) {
	forbidden := []string{
		`os.Getenv("POSTGRES_TEST_DSN")`,
		`os.Getenv("HARNESS_TEST_DATABASE_URL")`,
		`os.LookupEnv("POSTGRES_TEST_DSN")`,
		`os.LookupEnv("HARNESS_TEST_DATABASE_URL")`,
		"gorm.Open(",
	}
	recommendationsDir, err := filepath.Abs(filepath.Dir(migrationsDir()))
	if err != nil {
		t.Fatalf("resolving the recommendations directory: %v", err)
	}
	ownDir := filepath.Join(recommendationsDir, "internal", "testsupport")
	walkErr := filepath.WalkDir(recommendationsDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path == ownDir {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, marker := range forbidden {
			if strings.Contains(string(body), marker) {
				rel, _ := filepath.Rel(recommendationsDir, path)
				t.Errorf("%s contains %s; open the test database with testsupport.OpenTestTx", rel, marker)
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walking %s: %v", recommendationsDir, walkErr)
	}
}
