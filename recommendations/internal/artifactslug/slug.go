// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package artifactslug derives the stable kebab-case slug an artifact is identified by.
// The identity is frozen once rows carrying these slugs exist: changing the derivation
// strands every persisted slug and mints a second identity on the next run.
package artifactslug

import "strings"

// Kebab lowercases s and collapses each run of non-alphanumeric characters to a single
// dash, trimmed at both ends. An input carrying no alphanumerics yields "". A filename
// like "error_wrapping" or "pr review" therefore maps to "error-wrapping"/"pr-review",
// so the (type, slug) identity is stable across spacing and separator variations.
func Kebab(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteByte(byte(r))
			lastDash = false
		case b.Len() > 0 && !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
