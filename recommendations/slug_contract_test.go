// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSecuritySlugConstsMatchPythonTaxonomy guards the cross-language contract for the security
// taxonomy. The slug consts here (and the SecuritySummary proto fields) must stay in lockstep with the
// kebab-case slugs the tetriz-py derive prompt names, which is where the model is told which defensive
// rule to mint for each detected leak. A rename on either side silently breaks evidence matching:
// securitySlugsWithEvidence would stop recognising the org's real incidents and the security-only gate
// would hide every artifact. This pins the Go consts to the derive prompt so that drift fails here, not
// in a live derive run.
func TestSecuritySlugConstsMatchPythonTaxonomy(t *testing.T) {
	t.Parallel()
	pySlugs := pythonSecuritySlugs(t)
	for _, slug := range []string{slugAPIKeyLeak, slugPIIInjection, slugDBCredentialLeak} {
		require.Containsf(t, pySlugs, slug,
			"Go security slug const %q is not named in the tetriz-py derive prompt — the taxonomy drifted; "+
				"reconcile aggregator.go consts, the SecuritySummary proto, and derive_org_artifacts.py", slug)
	}
}

// pythonSecuritySlugs reads the tetriz-py derive prompt and returns the set of kebab-case slugs it
// names in backticks (for example `api-key-leak`). Resolved relative to THIS file via runtime.Caller so
// it is independent of the test's working directory: one parent hop walks recommendations → repo root,
// then descends into tetriz-py.
func pythonSecuritySlugs(t *testing.T) map[string]struct{} {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller(0) failed")
	promptPath := filepath.Join(filepath.Dir(thisFile), "..",
		"tetriz-py", "app", "llm", "prompts", "derive_org_artifacts.py")
	src, err := os.ReadFile(promptPath)
	require.NoErrorf(t, err, "%s must be readable to verify the slug contract — path drift?", promptPath)

	// Matches a backtick-wrapped kebab-case slug: `api-key-leak`, `db-credential-leak`.
	re := regexp.MustCompile("`([a-z0-9]+(?:-[a-z0-9]+)+)`")
	slugs := make(map[string]struct{})
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		slugs[m[1]] = struct{}{}
	}
	require.NotEmpty(t, slugs, "no kebab-case slugs parsed from %s", promptPath)
	return slugs
}
