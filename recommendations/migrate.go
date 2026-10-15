// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"embed"
	"fmt"
	"sort"
	"strings"

	"gorm.io/gorm"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// RunMigrations applies every bundled schema migration in filename order against db. Every statement is
// guarded with IF NOT EXISTS, so applying the set to a database that already holds the schema is a no-op —
// this is what lets a second scan against the same database succeed. Each file is applied
// statement-by-statement (line comments stripped first, because the SQL prose contains semicolons), since
// the driver's prepared path rejects a multi-statement string.
func RunMigrations(db *gorm.DB) error {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("listing migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		body, readErr := migrationsFS.ReadFile("migrations/" + name)
		if readErr != nil {
			return fmt.Errorf("reading migration %s: %w", name, readErr)
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
			if err := db.Exec(stmt).Error; err != nil {
				return fmt.Errorf("applying migration %s: %w", name, err)
			}
		}
	}
	return nil
}
