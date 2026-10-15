// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func repoPathSet(paths ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		set[p] = struct{}{}
	}
	return set
}

// TestPatchBrokenPath covers the deterministic broken-path patcher: a unique-basename
// match and a single extension-drift variant produce an in-place rename; zero or two-plus
// candidates fall back to advisory (no patch). It also asserts the reference is rewritten
// exactly, leaving the rest of the body untouched.
func TestPatchBrokenPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		brokenRef        string
		body             string
		repoPaths        map[string]struct{}
		wantAutoFixable  bool
		wantResolvedPath string
		wantPatched      string
	}{
		{
			name:             "unique basename match renames in place",
			brokenRef:        "docs/old/guide.md",
			body:             "See docs/old/guide.md for details.",
			repoPaths:        repoPathSet("docs/new/guide.md", "README.md"),
			wantAutoFixable:  true,
			wantResolvedPath: "docs/new/guide.md",
			wantPatched:      "See docs/new/guide.md for details.",
		},
		{
			name:             "single extension-drift variant renames in place",
			brokenRef:        "src/components/Foo.ts",
			body:             "import Foo from 'src/components/Foo.ts'",
			repoPaths:        repoPathSet("src/components/Foo.tsx", "src/components/Bar.ts"),
			wantAutoFixable:  true,
			wantResolvedPath: "src/components/Foo.tsx",
			wantPatched:      "import Foo from 'src/components/Foo.tsx'",
		},
		{
			name:            "ambiguous — two basename matches falls back to advisory",
			brokenRef:       "config.json",
			body:            "load config.json at boot",
			repoPaths:       repoPathSet("backend/config.json", "client/config.json"),
			wantAutoFixable: false,
		},
		{
			name:            "no match falls back to advisory",
			brokenRef:       "docs/missing.md",
			body:            "see docs/missing.md",
			repoPaths:       repoPathSet("docs/present.md", "README.md"),
			wantAutoFixable: false,
		},
		{
			name:            "basename plus extension-drift are two distinct candidates — advisory",
			brokenRef:       "app/util.ts",
			body:            "app/util.ts",
			repoPaths:       repoPathSet("lib/util.ts", "app/util.tsx"),
			wantAutoFixable: false,
		},
		{
			// Extension-drift resolves Foo.ts→src/Foo.tsx; the standalone occurrence is
			// rewritten while "Foo.tsx" (which merely CONTAINS "Foo.ts") is left intact — a
			// raw substring replace would corrupt it to "Foo.tsxx".
			name:             "whole-token replace leaves a longer path containing the ref intact",
			brokenRef:        "Foo.ts",
			body:             "import 'Foo.ts'; the component is Foo.tsx",
			repoPaths:        repoPathSet("src/Foo.tsx"),
			wantAutoFixable:  true,
			wantResolvedPath: "src/Foo.tsx",
			wantPatched:      "import 'src/Foo.tsx'; the component is Foo.tsx",
		},
		{
			// Unique-basename resolves config.json→backend/config.json; the standalone ref is
			// rewritten while the already-correct "backend/config.json" is untouched — a raw
			// substring replace would double the prefix to "backend/backend/config.json".
			name:             "whole-token replace does not double a prefix already present in body",
			brokenRef:        "config.json",
			body:             "load config.json (the real path is backend/config.json)",
			repoPaths:        repoPathSet("backend/config.json"),
			wantAutoFixable:  true,
			wantResolvedPath: "backend/config.json",
			wantPatched:      "load backend/config.json (the real path is backend/config.json)",
		},
		{
			// A candidate resolves, but the ref never appears as a whole token in the body
			// (only inside a longer path): no genuine replacement, so advisory — never a
			// no-op patch reported as auto-fixable.
			name:            "resolvable ref absent as a whole token falls back to advisory",
			brokenRef:       "guide.md",
			body:            "see docs/old/guide.md for details",
			repoPaths:       repoPathSet("docs/new/guide.md"),
			wantAutoFixable: false,
		},
		{
			name:            "empty repo path set falls back to advisory",
			brokenRef:       "foo.ts",
			body:            "foo.ts",
			repoPaths:       repoPathSet(),
			wantAutoFixable: false,
		},
		{
			// Compound extension: stems differ (foo.d vs foo), so extension-drift does NOT
			// fire — a deliberate conservative miss (advisory), never a wrong rename.
			name:            "compound-extension mismatch is a conservative advisory miss",
			brokenRef:       "foo.ts",
			body:            "foo.ts",
			repoPaths:       repoPathSet("foo.d.ts"),
			wantAutoFixable: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			patched, resolvedPath, autoFixable := PatchBrokenPath(tt.brokenRef, tt.body, tt.repoPaths)
			assert.Equal(t, tt.wantAutoFixable, autoFixable)
			if !tt.wantAutoFixable {
				assert.Empty(t, patched)
				assert.Empty(t, resolvedPath)
				return
			}
			assert.Equal(t, tt.wantResolvedPath, resolvedPath)
			// Exact body equality is the "replaced only the whole-token occurrence" proof —
			// it fails if a substring inside a longer path were also rewritten.
			assert.Equal(t, tt.wantPatched, patched)
		})
	}
}
