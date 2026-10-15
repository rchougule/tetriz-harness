// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	daomocks "github.com/tetriz-ai/tetriz-harness/recommendations/dao/mocks"
	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/artifactslug"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

const (
	gateTestOrgID = "11111111-1111-1111-1111-111111111111"
	gateTestRepo  = "22222222-2222-2222-2222-222222222222"
)

// treeFixture is a RepoTree whose directory_structure grounds backend/main.go and
// backend/dao/repo.go (the lossless dir-grouped encoding).
func treeFixture() *promptlibraryv1.RepoTree {
	return &promptlibraryv1.RepoTree{
		DirectoryStructure: "backend: main.go\nbackend/dao: repo.go",
	}
}

// validRule is a project-scoped rule that passes every validator (no cited paths,
// no path-shaped body tokens), used as the baseline a case mutates one field of. The
// slug is derived from the title so two calls with the same title (modulo case)
// collide on slug — the duplicate/suppression cases rely on this.
func validRule(title string) DerivedArtifact {
	return DerivedArtifact{
		Title:           title,
		Slug:            slugFromTitle(title),
		Body:            "Wrap returned errors with context so call sites are debuggable.",
		Scope:           "project",
		Priority:        "high",
		Confidence:      0.8,
		EvidenceSummary: "42 sessions hit this",
	}
}

// slugFromTitle is a test-only kebab-case slugifier: lowercase, runs of non-alnum
// collapse to a single "-", trimmed. Mirrors the canonical slug a real LLM would
// emit for a title so the dedup/suppression fixtures read naturally.
func slugFromTitle(title string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		case b.Len() > 0 && !prevDash:
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// stubNoPinnedSlugs satisfies the pinned-slug read every Grade performs. A pin suppresses
// re-derivation of that (type, slug), so a test not about pinning needs it to return
// nothing — the tests that ARE about it set their own expectation before this one.
func stubNoPinnedSlugs(m *daomocks.MockIPromptLibraryDAO) {
	m.EXPECT().
		ListPinnedSlugs(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, nil).AnyTimes()
}

func TestQualityGate_Grade(t *testing.T) {
	visible := promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE
	hidden := promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_HIDDEN_FAILED_EVAL

	type wantArtifact struct {
		state     promptlibraryv1.ArtifactSurfaceState
		validator string
	}
	tests := []struct {
		name         string
		result       *DerivationResult
		repoTree     *promptlibraryv1.RepoTree
		scopeValue   string
		securityOnly bool
		security     *promptlibraryv1.SecuritySummary
		dismissed    []string
		hidden       []model.ArtifactSlugRef
		daoCalled    bool
		want         []wantArtifact
		wantNoneVis  bool
	}{
		{
			name:        "skills-only response is valid (I2)",
			result:      &DerivationResult{Skills: []DerivedArtifact{validRule("Use the in-repo logger skill")}},
			scopeValue:  gateTestRepo,
			daoCalled:   true,
			want:        []wantArtifact{{state: visible}},
			wantNoneVis: false,
		},
		{
			name:        "empty response — nothing to grade, none visible",
			result:      &DerivationResult{},
			scopeValue:  gateTestRepo,
			daoCalled:   false,
			want:        nil,
			wantNoneVis: true,
		},
		{
			name: "org-scope artifact hidden (I3)",
			result: &DerivationResult{Rules: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Org-wide rule")
				r.Scope = "org"
				return r
			}()}},
			scopeValue:  gateTestRepo,
			daoCalled:   true,
			want:        []wantArtifact{{state: hidden, validator: validatorOrgScope}},
			wantNoneVis: true,
		},
		{
			name: "hallucinated cited path hidden (I12)",
			result: &DerivationResult{Rules: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Cite a fake file")
				r.CitedPaths = []string{"fake/ghost.go"}
				return r
			}()}},
			repoTree:    treeFixture(),
			scopeValue:  gateTestRepo,
			daoCalled:   true,
			want:        []wantArtifact{{state: hidden, validator: validatorPathsExist}},
			wantNoneVis: true,
		},
		{
			name: "valid cited path visible",
			result: &DerivationResult{Rules: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Cite a real file")
				r.CitedPaths = []string{"backend/main.go"}
				return r
			}()}},
			repoTree:   treeFixture(),
			scopeValue: gateTestRepo,
			daoCalled:  true,
			want:       []wantArtifact{{state: visible}},
		},
		{
			name: "real file cited with ./ /, trailing slash, or backslash still grounds",
			result: &DerivationResult{Rules: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Cite real files with messy prefixes")
				r.CitedPaths = []string{"./backend/main.go", "/backend/main.go", "backend/main.go/", `backend\dao\repo.go`}
				return r
			}()}},
			repoTree:   treeFixture(),
			scopeValue: gateTestRepo,
			daoCalled:  true,
			want:       []wantArtifact{{state: visible}},
		},
		{
			name: "cited path that canonicalizes to a non-existent file is hidden",
			result: &DerivationResult{Rules: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Cite a traversal onto a missing file")
				r.CitedPaths = []string{"backend/../nope.go"}
				return r
			}()}},
			repoTree:    treeFixture(),
			scopeValue:  gateTestRepo,
			daoCalled:   true,
			want:        []wantArtifact{{state: hidden, validator: validatorPathsExist}},
			wantNoneVis: true,
		},
		{
			name: "cited path with nil repo tree hidden (no tree → nothing grounds)",
			result: &DerivationResult{Rules: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Cite a path but no tree available")
				r.CitedPaths = []string{"backend/main.go"}
				return r
			}()}},
			repoTree:    nil,
			scopeValue:  gateTestRepo,
			daoCalled:   true,
			want:        []wantArtifact{{state: hidden, validator: validatorPathsExist}},
			wantNoneVis: true,
		},
		{
			name: "body path absent from cited_paths hidden (added requirement)",
			result: &DerivationResult{Rules: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Body cites a real path not in cited_paths")
				r.Body = "Apply this in backend/main.go before returning."
				r.CitedPaths = nil
				return r
			}()}},
			repoTree:    treeFixture(),
			scopeValue:  gateTestRepo,
			daoCalled:   true,
			want:        []wantArtifact{{state: hidden, validator: validatorPathsExist}},
			wantNoneVis: true,
		},
		{
			name: "non-canonical hallucinated body path is hidden",
			result: &DerivationResult{Rules: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Body cites a hallucinated path with a ./ prefix")
				r.Body = "The change belongs in ./backend/ghost.go per the analysis."
				return r
			}()}},
			repoTree:    treeFixture(),
			scopeValue:  gateTestRepo,
			daoCalled:   true,
			want:        []wantArtifact{{state: hidden, validator: validatorPathsExist}},
			wantNoneVis: true,
		},
		{
			name: "body path not in tree hidden (added requirement)",
			result: &DerivationResult{Rules: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Body cites a hallucinated path")
				r.Body = "The fix belongs in backend/ghost.go per the analysis."
				return r
			}()}},
			repoTree:    treeFixture(),
			scopeValue:  gateTestRepo,
			daoCalled:   true,
			want:        []wantArtifact{{state: hidden, validator: validatorPathsExist}},
			wantNoneVis: true,
		},
		{
			name: "root file cited only in body prose is grounded and hidden",
			result: &DerivationResult{Rules: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Mentions a root file without citing it")
				r.Body = "Document this convention in the README.md at the repo root."
				r.CitedPaths = nil
				return r
			}()}},
			repoTree:    &promptlibraryv1.RepoTree{DirectoryStructure: ".: README.md\nbackend: main.go"},
			scopeValue:  gateTestRepo,
			daoCalled:   true,
			want:        []wantArtifact{{state: hidden, validator: validatorPathsExist}},
			wantNoneVis: true,
		},
		{
			name: "body path present in cited_paths and tree visible",
			result: &DerivationResult{Rules: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Body and cited_paths agree")
				r.Body = "Apply this in backend/main.go before returning the error."
				r.CitedPaths = []string{"backend/main.go"}
				return r
			}()}},
			repoTree:   treeFixture(),
			scopeValue: gateTestRepo,
			daoCalled:  true,
			want:       []wantArtifact{{state: visible}},
		},
		{
			// A cited path naming a real repo directory grounds against the tree's
			// directory prefixes (the files-only index would otherwise miss it). treeFixture
			// grounds backend/main.go + backend/dao/repo.go, so "backend" and "backend/dao"
			// are real dirs; a trailing slash normalizes away.
			name: "cited directory grounds against real repo dirs",
			result: &DerivationResult{Rules: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Reuse shared code under a directory")
				r.CitedPaths = []string{"backend/", "backend/dao/"}
				return r
			}()}},
			repoTree:   treeFixture(),
			scopeValue: gateTestRepo,
			daoCalled:  true,
			want:       []wantArtifact{{state: visible}},
		},
		{
			// A directory that is not a prefix of any real tree file is still a
			// hallucination and stays hidden.
			name: "cited fake directory still hidden",
			result: &DerivationResult{Rules: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Cite a directory that does not exist")
				r.CitedPaths = []string{"backend/nonexistent"}
				return r
			}()}},
			repoTree:    treeFixture(),
			scopeValue:  gateTestRepo,
			daoCalled:   true,
			want:        []wantArtifact{{state: hidden, validator: validatorPathsExist}},
			wantNoneVis: true,
		},
		{
			// A hallucinated directory named only in body prose is now grounded: the
			// trailing "/" under a real top-level dir (backend) makes it a directory
			// candidate, and backend/ghost is not a real tree dir, so the artifact is
			// hidden. extractBodyDirs does not require it in cited_paths (unlike files).
			name: "prose-only hallucinated directory hidden",
			result: &DerivationResult{Rules: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Mention a fake directory in prose only")
				r.Body = "Reuse shared helpers under backend/ghost/ across services."
				r.CitedPaths = nil
				return r
			}()}},
			repoTree:    treeFixture(),
			scopeValue:  gateTestRepo,
			daoCalled:   true,
			want:        []wantArtifact{{state: hidden, validator: validatorPathsExist}},
			wantNoneVis: true,
		},
		{
			// A real directory named only in prose (not in cited_paths) stays visible —
			// directories are grounded file-unlike, so a legitimate location reference is
			// never hidden for missing a cited_paths re-declaration .
			name: "prose-only real directory reference stays visible",
			result: &DerivationResult{Rules: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Mention a real directory in prose only")
				r.Body = "Keep the query repositories under backend/dao/ consistent."
				r.CitedPaths = nil
				return r
			}()}},
			repoTree:   treeFixture(),
			scopeValue: gateTestRepo,
			daoCalled:  true,
			want:       []wantArtifact{{state: visible}},
		},
		{
			// A real top-level dir with a trailing slash resolves in validDirs and stays
			// visible.
			name: "prose-only real top-level directory stays visible",
			result: &DerivationResult{Rules: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Mention a real top-level directory in prose")
				r.Body = "Everything server-side lives under backend/ in this repo."
				r.CitedPaths = nil
				return r
			}()}},
			repoTree:   treeFixture(),
			scopeValue: gateTestRepo,
			daoCalled:  true,
			want:       []wantArtifact{{state: visible}},
		},
		{
			// Templates are exempt from prose grounding, directories included — the same
			// hallucinated backend/ghost/ body that hides a rule leaves a template visible.
			name: "template hallucinated prose directory stays visible",
			result: &DerivationResult{Templates: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Template mentions a fake directory")
				r.Body = "Reuse shared helpers under backend/ghost/ across services."
				r.CitedPaths = nil
				return r
			}()}},
			repoTree:   treeFixture(),
			scopeValue: gateTestRepo,
			daoCalled:  true,
			want:       []wantArtifact{{state: visible}},
		},
		{
			// A template body is an illustrative prompt scaffold, so its example paths are
			// not grounded — an absent example path does not hide the template.
			name: "template body example path not grounded",
			result: &DerivationResult{Templates: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Debugging prompt template")
				r.Body = "Describe the bug and list relevant files, e.g. docs/design/ghost.md, then ask."
				r.CitedPaths = nil
				return r
			}()}},
			repoTree:   treeFixture(),
			scopeValue: gateTestRepo,
			daoCalled:  true,
			want:       []wantArtifact{{state: visible}},
		},
		{
			// A body citing a real in-tree file that is missing from cited_paths hides a
			// RULE (I12, retained — see the rule case above), but leaves a TEMPLATE visible
			// because template bodies are not prose-grounded.
			name: "template body cites real file uncited is visible",
			result: &DerivationResult{Templates: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Template mentions a real file")
				r.Body = "Apply this in backend/main.go before returning."
				r.CitedPaths = nil
				return r
			}()}},
			repoTree:   treeFixture(),
			scopeValue: gateTestRepo,
			daoCalled:  true,
			want:       []wantArtifact{{state: visible}},
		},
		{
			// A template's structured cited_paths are still validated, so a template that
			// explicitly declares a fake file is hidden.
			name: "template with fake cited_paths still hidden",
			result: &DerivationResult{Templates: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Template cites a fake file")
				r.CitedPaths = []string{"fake/ghost.go"}
				return r
			}()}},
			repoTree:    treeFixture(),
			scopeValue:  gateTestRepo,
			daoCalled:   true,
			want:        []wantArtifact{{state: hidden, validator: validatorPathsExist}},
			wantNoneVis: true,
		},
		{
			name: "body URL is not treated as a path",
			result: &DerivationResult{Rules: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Reference a docs URL")
				r.Body = "See the guide at https://github.com/org/repo/blob/main/x.go for context."
				return r
			}()}},
			repoTree:   treeFixture(),
			scopeValue: gateTestRepo,
			daoCalled:  true,
			want:       []wantArtifact{{state: visible}},
		},
		{
			name: "body Go import path is not treated as a repo path",
			result: &DerivationResult{Rules: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Reference an import path")
				r.Body = "Import github.com/example/app/pkg/logger and use it."
				return r
			}()}},
			repoTree:   treeFixture(),
			scopeValue: gateTestRepo,
			daoCalled:  true,
			want:       []wantArtifact{{state: visible}},
		},
		{
			name: "body inline fraction with slash is not a path",
			result: &DerivationResult{Rules: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Reference a ratio")
				r.Body = "Keep the cache hit ratio a/b above the threshold at all times."
				return r
			}()}},
			repoTree:   treeFixture(),
			scopeValue: gateTestRepo,
			daoCalled:  true,
			want:       []wantArtifact{{state: visible}},
		},
		{
			name: "short body hidden",
			result: &DerivationResult{Rules: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Too short")
				r.Body = "do better"
				return r
			}()}},
			scopeValue:  gateTestRepo,
			daoCalled:   true,
			want:        []wantArtifact{{state: hidden, validator: validatorBodyMinLength}},
			wantNoneVis: true,
		},
		{
			name: "multiple failures report the earliest in order (body before scope)",
			result: &DerivationResult{Rules: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Short and org-scoped")
				r.Body = "too short"
				r.Scope = "org"
				return r
			}()}},
			scopeValue:  gateTestRepo,
			daoCalled:   true,
			want:        []wantArtifact{{state: hidden, validator: validatorBodyMinLength}},
			wantNoneVis: true,
		},
		{
			// Two rules whose titles differ only by case slugify to the same slug — the
			// second is a duplicate. Dedup is on the stable slug, not the descriptive title.
			name: "duplicate slug — first visible, second hidden",
			result: &DerivationResult{Rules: []DerivedArtifact{
				validRule("Wrap errors with context"),
				validRule("wrap errors with context"),
			}},
			scopeValue: gateTestRepo,
			daoCalled:  true,
			want: []wantArtifact{
				{state: visible},
				{state: hidden, validator: validatorDuplicateSlug},
			},
		},
		{
			// Slug missing → hidden. The strict prompt + Pydantic min_length make this
			// rare; the gate is the backstop that keeps an identity-less artifact off the list.
			name: "missing slug hidden",
			result: &DerivationResult{Rules: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Some rule with a blank slug")
				r.Slug = "   "
				return r
			}()}},
			scopeValue:  gateTestRepo,
			daoCalled:   true,
			want:        []wantArtifact{{state: hidden, validator: validatorSlugMissing}},
			wantNoneVis: true,
		},
		{
			// Non-empty but malformed slug (spaces survive normalization) → hidden by
			// the non-blocking slug_format gate; the kebab contract is enforced, not 502'd.
			name: "malformed rule slug (spaces) hidden",
			result: &DerivationResult{Rules: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Rule with a spaced slug")
				r.Slug = "go error wrapping"
				return r
			}()}},
			scopeValue:  gateTestRepo,
			daoCalled:   true,
			want:        []wantArtifact{{state: hidden, validator: validatorSlugFormat}},
			wantNoneVis: true,
		},
		{
			// A skill slug must be "/"-prefixed kebab; spaces in the body are malformed.
			name: "malformed skill slug hidden",
			result: &DerivationResult{Skills: []DerivedArtifact{func() DerivedArtifact {
				s := validRule("Skill with a spaced slug")
				s.Slug = "/Spec Driven Dev"
				return s
			}()}},
			scopeValue:  gateTestRepo,
			daoCalled:   true,
			want:        []wantArtifact{{state: hidden, validator: validatorSlugFormat}},
			wantNoneVis: true,
		},
		{
			// A slug that is valid kebab but longer than the prompt_library.slug column
			// (VARCHAR(255)) is hidden by slug_format — not surfaced visible — so the
			// truncated value can never overflow the column and fail the whole batch insert.
			name: "over-long slug hidden (would overflow the column)",
			result: &DerivationResult{Rules: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Rule with an absurdly long slug")
				r.Slug = strings.Repeat("a", maxSlugLen+5)
				return r
			}()}},
			scopeValue:  gateTestRepo,
			daoCalled:   true,
			want:        []wantArtifact{{state: hidden, validator: validatorSlugFormat}},
			wantNoneVis: true,
		},
		{
			name:        "slug matches dismissed",
			result:      &DerivationResult{Rules: []DerivedArtifact{validRule("Add error wrapping to handlers")}},
			scopeValue:  gateTestRepo,
			dismissed:   []string{"add-error-wrapping-to-handlers"},
			daoCalled:   true,
			want:        []wantArtifact{{state: hidden, validator: validatorNotDismissed}},
			wantNoneVis: true,
		},
		{
			name:        "slug matches recent-hidden",
			result:      &DerivationResult{Rules: []DerivedArtifact{validRule("Add structured error wrapping to all handlers")}},
			scopeValue:  gateTestRepo,
			hidden:      []model.ArtifactSlugRef{{ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE, Slug: "add-structured-error-wrapping-to-all-handlers"}},
			daoCalled:   true,
			want:        []wantArtifact{{state: hidden, validator: validatorNotDismissed}},
			wantNoneVis: true,
		},
		{
			// The recent-hidden suppression is keyed per (type, slug): a hidden HOOK with the
			// same slug must NOT suppress a re-derived RULE, so this rule stays VISIBLE. Guards
			// the cross-type dedup-poisoning class the new artifact types would otherwise widen.
			name:       "recent-hidden of a different type does not suppress",
			result:     &DerivationResult{Rules: []DerivedArtifact{validRule("Add structured error wrapping to all handlers")}},
			scopeValue: gateTestRepo,
			hidden:     []model.ArtifactSlugRef{{ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK, Slug: "add-structured-error-wrapping-to-all-handlers"}},
			daoCalled:  true,
			want:       []wantArtifact{{state: visible}},
		},
		{
			name:       "slug not returned by windowed read no longer suppresses",
			result:     &DerivationResult{Rules: []DerivedArtifact{validRule("Add error wrapping to handlers")}},
			scopeValue: gateTestRepo,
			dismissed:  nil,
			hidden:     nil,
			daoCalled:  true,
			want:       []wantArtifact{{state: visible}},
		},
		{
			// A real governance rule (addresses a slug the org has an incident of) is
			// VISIBLE on a securityOnly run even with an ungrounded cited path — the
			// path check is relaxed, and the security-evidence check passes.
			name: "securityOnly: governance rule skips path check, passes evidence",
			result: &DerivationResult{Rules: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Rotate the leaked key")
				r.CitedPaths = []string{"fake/ghost.go"}
				r.AddressesIssues = []string{"api-key-leak"}
				return r
			}()}},
			repoTree:     treeFixture(),
			scopeValue:   gateTestRepo,
			securityOnly: true,
			security:     &promptlibraryv1.SecuritySummary{ApiKeyLeak: 1},
			daoCalled:    true,
			want:         []wantArtifact{{state: visible}},
		},
		{
			// Boilerplate on a securityOnly run: a general rule with no security slug
			// is hidden.
			name: "securityOnly: non-security boilerplate hidden",
			result: &DerivationResult{Rules: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Add table-driven tests")
				r.AddressesIssues = []string{"missing-tests"}
				return r
			}()}},
			scopeValue:   gateTestRepo,
			securityOnly: true,
			security:     &promptlibraryv1.SecuritySummary{ApiKeyLeak: 1},
			daoCalled:    true,
			want:         []wantArtifact{{state: hidden, validator: validatorSecurityScope}},
			wantNoneVis:  true,
		},
		{
			// Empty addresses_issues on a securityOnly run cannot be confirmed as
			// security → hidden (the derive prompt requires security rules to tag it).
			name:         "securityOnly: empty addresses_issues hidden",
			result:       &DerivationResult{Rules: []DerivedArtifact{validRule("Untagged rule")}},
			scopeValue:   gateTestRepo,
			securityOnly: true,
			security:     &promptlibraryv1.SecuritySummary{ApiKeyLeak: 1},
			daoCalled:    true,
			want:         []wantArtifact{{state: hidden, validator: validatorSecurityScope}},
			wantNoneVis:  true,
		},
		{
			// Claims a security slug the org has NO incident of → graded against real
			// evidence (not the claim) → hidden.
			name: "securityOnly: security slug without matching evidence hidden",
			result: &DerivationResult{Rules: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Mask PII in prompts")
				r.AddressesIssues = []string{"pii-injection"}
				return r
			}()}},
			scopeValue:   gateTestRepo,
			securityOnly: true,
			security:     &promptlibraryv1.SecuritySummary{ApiKeyLeak: 1}, // no PII incident
			daoCalled:    true,
			want:         []wantArtifact{{state: hidden, validator: validatorSecurityScope}},
			wantNoneVis:  true,
		},
		{
			// Mixed slugs: addresses at least one real incident → VISIBLE.
			name: "securityOnly: mixed slugs with one real incident visible",
			result: &DerivationResult{Rules: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Guard credentials and tests")
				r.AddressesIssues = []string{"missing-tests", "api-key-leak"}
				return r
			}()}},
			scopeValue:   gateTestRepo,
			securityOnly: true,
			security:     &promptlibraryv1.SecuritySummary{ApiKeyLeak: 1},
			daoCalled:    true,
			want:         []wantArtifact{{state: visible}},
		},
		{
			// addresses_issues is free-text LLM output, so the gate normalizes
			// (lower + trim) before matching the kebab-case evidence slugs. A
			// mixed-case, space-padded tag must still match the org's real incident.
			name: "securityOnly: case/whitespace-variant slug still matches evidence",
			result: &DerivationResult{Rules: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Prevent leaked credentials")
				r.AddressesIssues = []string{"  API-Key-Leak "}
				return r
			}()}},
			scopeValue:   gateTestRepo,
			securityOnly: true,
			security:     &promptlibraryv1.SecuritySummary{ApiKeyLeak: 1},
			daoCalled:    true,
			want:         []wantArtifact{{state: visible}},
		},
		{
			// A TEMPLATE tagged with a real incident slug is STILL hidden on a
			// securityOnly run: the spec surfaces only security RULES, not templates
			// or skills, no matter how the model tags addresses_issues.
			name: "securityOnly: security-tagged template hidden (rules only)",
			result: &DerivationResult{Templates: []DerivedArtifact{func() DerivedArtifact {
				a := validRule("Security incident response template")
				a.AddressesIssues = []string{"api-key-leak"}
				return a
			}()}},
			scopeValue:   gateTestRepo,
			securityOnly: true,
			security:     &promptlibraryv1.SecuritySummary{ApiKeyLeak: 1},
			daoCalled:    true,
			want:         []wantArtifact{{state: hidden, validator: validatorSecurityScope}},
			wantNoneVis:  true,
		},
		{
			// A SKILL tagged with a real incident slug is likewise hidden on a
			// securityOnly run — same rules-only rule as the template case.
			name: "securityOnly: security-tagged skill hidden (rules only)",
			result: &DerivationResult{Skills: []DerivedArtifact{func() DerivedArtifact {
				a := validRule("Credential redaction skill")
				a.AddressesIssues = []string{"api-key-leak"}
				return a
			}()}},
			scopeValue:   gateTestRepo,
			securityOnly: true,
			security:     &promptlibraryv1.SecuritySummary{ApiKeyLeak: 1},
			daoCalled:    true,
			want:         []wantArtifact{{state: hidden, validator: validatorSecurityScope}},
			wantNoneVis:  true,
		},
		{
			// Two copies of the same NON-security artifact on a securityOnly run:
			// BOTH must be classified security_scope (so toModels drops them), NOT the
			// second as duplicate_slug — a duplicate_slug hide would persist and
			// poison the recent-hidden dedup for a later full run. Proves security_scope
			// is evaluated before the duplicate check.
			name: "securityOnly: duplicate general artifact both hidden as security_scope",
			result: &DerivationResult{Rules: []DerivedArtifact{
				validRule("Repeated general rule"),
				validRule("Repeated general rule"),
			}},
			scopeValue:   gateTestRepo,
			securityOnly: true,
			security:     &promptlibraryv1.SecuritySummary{ApiKeyLeak: 1},
			daoCalled:    true,
			want: []wantArtifact{
				{state: hidden, validator: validatorSecurityScope},
				{state: hidden, validator: validatorSecurityScope},
			},
			wantNoneVis: true,
		},
		{
			// A scoped-out general artifact must NOT reserve its title: a later
			// corrected security RULE sharing the title stays VISIBLE, not hidden as a
			// (persisted) duplicate. The scoped-out one is dropped as security_scope.
			name: "securityOnly: scoped-out artifact does not reserve title for a real security rule",
			result: &DerivationResult{Rules: []DerivedArtifact{
				func() DerivedArtifact {
					r := validRule("Guard credentials")
					r.AddressesIssues = []string{"missing-tests"} // no security evidence → scoped out
					return r
				}(),
				func() DerivedArtifact {
					r := validRule("Guard credentials") // same title, but a real security rule
					r.AddressesIssues = []string{"api-key-leak"}
					return r
				}(),
			}},
			scopeValue:   gateTestRepo,
			securityOnly: true,
			security:     &promptlibraryv1.SecuritySummary{ApiKeyLeak: 1},
			daoCalled:    true,
			want: []wantArtifact{
				{state: hidden, validator: validatorSecurityScope},
				{state: visible},
			},
		},
		{
			// Same untagged rule on a NON-securityOnly run is VISIBLE — the
			// security-scope gate is inert off the securityOnly path.
			name:       "non-securityOnly: empty addresses_issues visible (gate inert)",
			result:     &DerivationResult{Rules: []DerivedArtifact{validRule("Untagged general rule")}},
			scopeValue: gateTestRepo,
			daoCalled:  true,
			want:       []wantArtifact{{state: visible}},
		},
		{
			name:        "empty scope_value hidden",
			result:      &DerivationResult{Rules: []DerivedArtifact{validRule("Project rule with no repo id")}},
			scopeValue:  "",
			daoCalled:   true,
			want:        []wantArtifact{{state: hidden, validator: validatorProjectScope}},
			wantNoneVis: true,
		},
		{
			name: "out-of-range confidence is clamped, not hidden",
			result: &DerivationResult{Rules: []DerivedArtifact{func() DerivedArtifact {
				r := validRule("Confidence over one")
				r.Confidence = 1.5
				return r
			}()}},
			scopeValue: gateTestRepo,
			daoCalled:  true,
			want:       []wantArtifact{{state: visible}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			libraryDAO := daomocks.NewMockIPromptLibraryDAO(ctrl)
			stubNoPinnedSlugs(libraryDAO)
			if tc.daoCalled {
				libraryDAO.EXPECT().
					ListDismissedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, tc.scopeValue).
					Return(tc.dismissed, nil)
				libraryDAO.EXPECT().
					ListRecentHiddenFailedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, tc.scopeValue, gomock.Any(), 8).
					Return(tc.hidden, nil)
			}
			// Session floors 0 keep the Template session-signal check inert for this
			// table (it has its own focused test); the other validators are exercised here.
			gate := NewQualityGate(libraryDAO, func() int { return 8 }, func() int { return 0 }, func() int { return 0 }, func() int { return 5 }, func() int { return 4 })

			graded, err := gate.Grade(context.Background(), gateTestOrgID, tc.scopeValue, tc.result, tc.repoTree, tc.security, 0, 0, tc.securityOnly, false, commonv1.IdeType_IDE_TYPE_UNSPECIFIED, nil)
			require.NoError(t, err)
			require.Len(t, graded.Artifacts, len(tc.want))
			for i, want := range tc.want {
				assert.Equal(t, want.state, graded.Artifacts[i].SurfaceState, "artifact %d surface_state", i)
				assert.Equal(t, want.validator, graded.Artifacts[i].FailedValidator, "artifact %d failed_validator", i)
			}
			assert.Equal(t, tc.wantNoneVis, graded.NoneVisible())
		})
	}
}

func TestQualityGate_Grade_DedupReadError(t *testing.T) {
	ctrl := gomock.NewController(t)
	libraryDAO := daomocks.NewMockIPromptLibraryDAO(ctrl)
	stubNoPinnedSlugs(libraryDAO)
	readErr := errors.New("db down")
	libraryDAO.EXPECT().
		ListDismissedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateTestRepo).
		Return(nil, readErr)
	gate := NewQualityGate(libraryDAO, func() int { return 8 }, func() int { return 0 }, func() int { return 0 }, func() int { return 5 }, func() int { return 4 })

	graded, err := gate.Grade(context.Background(), gateTestOrgID, gateTestRepo,
		&DerivationResult{Rules: []DerivedArtifact{validRule("Any rule")}}, nil, nil, 0, 0, false, false, commonv1.IdeType_IDE_TYPE_UNSPECIFIED, nil)
	require.Error(t, err)
	assert.Nil(t, graded)
}

// TestQualityGate_Grade_TemplateSessionSignal pins the decoupled gate: a TEMPLATE is
// hidden when the repo's session buckets fall below the low/high floors, while Rules
// and Skills at the same below-floor counts stay visible (they are VCS-derived).
func TestQualityGate_Grade_TemplateSessionSignal(t *testing.T) {
	visible := promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE
	hidden := promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_HIDDEN_FAILED_EVAL
	type wantArtifact struct {
		state     promptlibraryv1.ArtifactSurfaceState
		validator string
	}
	tests := []struct {
		name        string
		result      *DerivationResult
		sessionLow  int
		sessionHigh int
		want        []wantArtifact
	}{
		{
			name:        "template above both floors is visible",
			result:      &DerivationResult{Templates: []DerivedArtifact{validRule("Specify file paths in your prompt")}},
			sessionLow:  40,
			sessionHigh: 40,
			want:        []wantArtifact{{state: visible}},
		},
		{
			name:        "template below the low floor is hidden",
			result:      &DerivationResult{Templates: []DerivedArtifact{validRule("Specify file paths in your prompt")}},
			sessionLow:  5,
			sessionHigh: 40,
			want:        []wantArtifact{{state: hidden, validator: validatorTemplateSessionSignal}},
		},
		{
			name:        "template below the high floor is hidden",
			result:      &DerivationResult{Templates: []DerivedArtifact{validRule("Specify file paths in your prompt")}},
			sessionLow:  40,
			sessionHigh: 5,
			want:        []wantArtifact{{state: hidden, validator: validatorTemplateSessionSignal}},
		},
		{
			name:        "rule and skill below both floors stay visible (VCS-derived, unaffected)",
			result:      &DerivationResult{Rules: []DerivedArtifact{validRule("Wrap errors with context")}, Skills: []DerivedArtifact{validRule("Run the audit skill")}},
			sessionLow:  1,
			sessionHigh: 1,
			want:        []wantArtifact{{state: visible}, {state: visible}},
		},
		{
			// Two copies of the same below-floor template: BOTH must report
			// template_session_signal (so toModels drops them), NOT the second as
			// duplicate_slug — a duplicate_slug hide would persist and poison the
			// recent-hidden dedup, suppressing the template after the cohort recovers.
			// Proves the template-session gate is evaluated before the duplicate check
			// (mirrors the security_scope precedent).
			name: "duplicate below-floor template — both dropped as template_session_signal, not duplicate_slug",
			result: &DerivationResult{Templates: []DerivedArtifact{
				validRule("Specify file paths in your prompt"),
				validRule("Specify file paths in your prompt"),
			}},
			sessionLow:  5,
			sessionHigh: 40,
			want: []wantArtifact{
				{state: hidden, validator: validatorTemplateSessionSignal},
				{state: hidden, validator: validatorTemplateSessionSignal},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			libraryDAO := daomocks.NewMockIPromptLibraryDAO(ctrl)
			stubNoPinnedSlugs(libraryDAO)
			libraryDAO.EXPECT().
				ListDismissedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateTestRepo).
				Return(nil, nil)
			libraryDAO.EXPECT().
				ListRecentHiddenFailedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateTestRepo, gomock.Any(), 8).
				Return(nil, nil)
			gate := NewQualityGate(libraryDAO, func() int { return 8 }, func() int { return 30 }, func() int { return 30 }, func() int { return 5 }, func() int { return 4 })

			graded, err := gate.Grade(context.Background(), gateTestOrgID, gateTestRepo, tc.result, nil, nil, tc.sessionLow, tc.sessionHigh, false, false, commonv1.IdeType_IDE_TYPE_UNSPECIFIED, nil)
			require.NoError(t, err)
			require.Len(t, graded.Artifacts, len(tc.want))
			for i, want := range tc.want {
				assert.Equal(t, want.state, graded.Artifacts[i].SurfaceState, "artifact %d surface_state", i)
				assert.Equal(t, want.validator, graded.Artifacts[i].FailedValidator, "artifact %d failed_validator", i)
			}
		})
	}
}

// validHook is a project-scoped Claude-Code-shaped hook that passes every validator
// (valid JSON body agreeing with the structured event/matcher/command). A case mutates
// one field of it.
func validHook(slug, event string) DerivedArtifact {
	command := "gofmt -w ."
	matcher := "Edit|Write|MultiEdit"
	body := `{"hooks":{"` + event + `":[{"matcher":"` + matcher + `","hooks":[{"type":"command","command":"` + command + `"}]}]}}`
	return DerivedArtifact{
		Title:      "Format Go files on edit",
		Slug:       slug,
		Body:       body,
		Scope:      "project",
		Priority:   "high",
		Confidence: 0.8,
		Event:      event,
		Matcher:    matcher,
		Command:    command,
	}
}

// cursorHook is a Cursor-shaped (flat) hook whose body agrees with its event/command.
func cursorHook(slug, event string) DerivedArtifact {
	command := "npm run lint"
	body := `{"hooks":{"` + event + `":[{"command":"` + command + `"}]}}`
	return DerivedArtifact{
		Title:      "Lint on shell execution",
		Slug:       slug,
		Body:       body,
		Scope:      "project",
		Priority:   "high",
		Confidence: 0.8,
		Event:      event,
		Command:    command,
	}
}

// validAgent is a project-scoped agent that passes every validator (non-empty
// agent_name, no cited/prose repo paths to fail grounding).
func validAgent(slug, agentName string) DerivedArtifact {
	return DerivedArtifact{
		Title:       "Debugging specialist",
		Slug:        slug,
		Body:        "You are a debugging specialist. Investigate failing tests and trace the root cause.",
		Scope:       "project",
		Priority:    "high",
		Confidence:  0.8,
		AgentName:   agentName,
		Description: "Delegate when a test is failing and the cause is unclear.",
	}
}

// TestQualityGate_Grade_HooksAgents grades the two new artifact types: the type-specific
// validators (event / tool / body copy-safety / agent_name), the hook path-grounding
// exemption, the advisory-vs-blocking event checks, and the type-scope drops.
func TestQualityGate_Grade_HooksAgents(t *testing.T) {
	visible := promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE
	hidden := promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_HIDDEN_FAILED_EVAL

	type wantArtifact struct {
		state     promptlibraryv1.ArtifactSurfaceState
		validator string
	}
	tests := []struct {
		name            string
		result          *DerivationResult
		repoTree        *promptlibraryv1.RepoTree
		newTypesEnabled bool
		resolvedTool    commonv1.IdeType
		want            []wantArtifact
		wantNoneVis     bool
	}{
		{
			name:            "valid hook is visible",
			result:          &DerivationResult{Hooks: []DerivedArtifact{validHook("format-on-edit", "PostToolUse")}},
			newTypesEnabled: true,
			resolvedTool:    commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
			want:            []wantArtifact{{state: visible}},
		},
		{
			name:            "valid agent is visible",
			result:          &DerivationResult{Agents: []DerivedArtifact{validAgent("debugger", "debugger")}},
			newTypesEnabled: true,
			resolvedTool:    commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
			want:            []wantArtifact{{state: visible}},
		},
		{
			name: "hook missing event hidden",
			result: &DerivationResult{Hooks: []DerivedArtifact{func() DerivedArtifact {
				h := validHook("format-on-edit", "PostToolUse")
				h.Event = ""
				return h
			}()}},
			newTypesEnabled: true,
			resolvedTool:    commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
			want:            []wantArtifact{{state: hidden, validator: validatorHookEvent}},
			wantNoneVis:     true,
		},
		{
			name: "agent missing agent_name hidden",
			result: &DerivationResult{Agents: []DerivedArtifact{func() DerivedArtifact {
				a := validAgent("debugger", "debugger")
				a.AgentName = ""
				return a
			}()}},
			newTypesEnabled: true,
			resolvedTool:    commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
			want:            []wantArtifact{{state: hidden, validator: validatorAgentName}},
			wantNoneVis:     true,
		},
		{
			name: "agent missing description hidden",
			result: &DerivationResult{Agents: []DerivedArtifact{func() DerivedArtifact {
				a := validAgent("debugger", "debugger")
				a.Description = "" // every tool's agent format requires a description
				return a
			}()}},
			newTypesEnabled: true,
			resolvedTool:    commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
			want:            []wantArtifact{{state: hidden, validator: validatorAgentDescription}},
			wantNoneVis:     true,
		},
		{
			name: "agent with no instruction prose hidden (shared body-min-length)",
			result: &DerivationResult{Agents: []DerivedArtifact{func() DerivedArtifact {
				a := validAgent("debugger", "debugger")
				a.Body = "" // no system prompt — the agent body IS the prose, so body-min-length hides it
				return a
			}()}},
			newTypesEnabled: true,
			resolvedTool:    commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
			want:            []wantArtifact{{state: hidden, validator: validatorBodyMinLength}},
			wantNoneVis:     true,
		},
		{
			name: "hook with no command hidden (nothing to synthesize)",
			result: &DerivationResult{Hooks: []DerivedArtifact{func() DerivedArtifact {
				h := validHook("format-on-edit", "PostToolUse")
				h.Command = "" // no payload for Go to serialize into the tool's body shape
				return h
			}()}},
			newTypesEnabled: true,
			resolvedTool:    commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
			want:            []wantArtifact{{state: hidden, validator: validatorHookBody}},
			wantNoneVis:     true,
		},
		{
			// The LLM-authored body is no longer graded — Go synthesizes the tool-correct body from
			// the structured event/matcher/command. Even an EMPTY LLM body is VISIBLE (a hook
			// is exempt from body-min-length; its command is the payload), proving the exemption.
			name: "LLM hook body ignored — empty body still visible",
			result: &DerivationResult{Hooks: []DerivedArtifact{func() DerivedArtifact {
				h := validHook("format-on-edit", "PostToolUse")
				h.Body = "" // Go synthesizes the body from event/matcher/command
				return h
			}()}},
			newTypesEnabled: true,
			resolvedTool:    commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
			want:            []wantArtifact{{state: visible}},
		},
		{
			name:            "valid-for-cursor event on cursor is clean",
			result:          &DerivationResult{Hooks: []DerivedArtifact{cursorHook("lint-on-shell", "beforeShellExecution")}},
			newTypesEnabled: true,
			resolvedTool:    commonv1.IdeType_IDE_TYPE_CURSOR,
			want:            []wantArtifact{{state: visible}},
		},
		{
			// A cross-tool event (Claude's SubagentStop — genuinely absent from Cursor's vocabulary,
			// known to a different mapped tool) is inert on Cursor, so it is suppressed rather than
			// kept-and-metered. (PreToolUse is NOT used here: it is a native Cursor event too.)
			name:            "cross-tool event suppressed (Claude event on Cursor)",
			result:          &DerivationResult{Hooks: []DerivedArtifact{cursorHook("lint-on-shell", "SubagentStop")}},
			newTypesEnabled: true,
			resolvedTool:    commonv1.IdeType_IDE_TYPE_CURSOR,
			want:            []wantArtifact{{state: hidden, validator: validatorHookEventToolScoped}},
			wantNoneVis:     true,
		},
		{
			// The mismatch is symmetric: a Cursor event on a Claude repo is equally inert.
			name:            "cross-tool event suppressed (Cursor event on Claude)",
			result:          &DerivationResult{Hooks: []DerivedArtifact{validHook("lint-on-shell", "beforeShellExecution")}},
			newTypesEnabled: true,
			resolvedTool:    commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
			want:            []wantArtifact{{state: hidden, validator: validatorHookEventToolScoped}},
			wantNoneVis:     true,
		},
		{
			// An event unknown across EVERY mapped tool is genuine vocabulary drift, not a
			// cross-tool mismatch — kept VISIBLE, and it bumps the drift counter (asserted separately).
			name:            "globally-unknown event kept visible (genuine drift)",
			result:          &DerivationResult{Hooks: []DerivedArtifact{cursorHook("lint-on-shell", "onFooBarNovel")}},
			newTypesEnabled: true,
			resolvedTool:    commonv1.IdeType_IDE_TYPE_CURSOR,
			want:            []wantArtifact{{state: visible}}, // advisory: kept, counter bumps (asserted separately)
		},
		{
			// Antigravity has no allowlist entry (drifts on every event), so it CANNOT be judged
			// for a cross-tool mismatch — a common event name on it stays advisory, never blocked.
			// Regression guard: without the mapped-tool guard this hook would false-block.
			name:            "cross-tool-known event on unmapped Antigravity stays visible",
			result:          &DerivationResult{Hooks: []DerivedArtifact{cursorHook("guard", "PreToolUse")}},
			newTypesEnabled: true,
			resolvedTool:    commonv1.IdeType_IDE_TYPE_ANTIGRAVITY,
			want:            []wantArtifact{{state: visible}},
		},
		{
			// Copilot spells the same event camelCase (preToolUse); the Claude-cased PreToolUse
			// lowercases to Copilot's own token, so it is KNOWN — no block, no drift counter.
			name:            "casing alias: Claude-cased event known on Copilot",
			result:          &DerivationResult{Hooks: []DerivedArtifact{cursorHook("guard", "PreToolUse")}},
			newTypesEnabled: true,
			resolvedTool:    commonv1.IdeType_IDE_TYPE_COPILOT,
			want:            []wantArtifact{{state: visible}},
		},
		{
			// Stop is shared (Claude Stop / Cursor stop / Stop) with the same semantic; on
			// Cursor it is that tool's OWN event, so it is known — never a cross-tool block.
			name:            "shared token Stop is known on Cursor",
			result:          &DerivationResult{Hooks: []DerivedArtifact{cursorHook("on-stop", "Stop")}},
			newTypesEnabled: true,
			resolvedTool:    commonv1.IdeType_IDE_TYPE_CURSOR,
			want:            []wantArtifact{{state: visible}},
		},
		{
			name:            "kiro lifecycle event suppressed",
			result:          &DerivationResult{Hooks: []DerivedArtifact{validHook("secret-protect", "PreToolUse")}},
			newTypesEnabled: true,
			resolvedTool:    commonv1.IdeType_IDE_TYPE_KIRO,
			want:            []wantArtifact{{state: hidden, validator: validatorHookEventToolScoped}},
			wantNoneVis:     true,
		},
		{
			// Kiro's vocabulary is closed to file events, so a lifecycle event beyond the
			// PreToolUse case above (Kiro runs none of them) is suppressed too.
			name:            "kiro non-file lifecycle event suppressed",
			result:          &DerivationResult{Hooks: []DerivedArtifact{cursorHook("post-edit", "PostToolUse")}},
			newTypesEnabled: true,
			resolvedTool:    commonv1.IdeType_IDE_TYPE_KIRO,
			want:            []wantArtifact{{state: hidden, validator: validatorHookEventToolScoped}},
			wantNoneVis:     true,
		},
		{
			name:            "kiro file-event hook is visible",
			result:          &DerivationResult{Hooks: []DerivedArtifact{cursorHook("fmt-on-save", "PostFileSave")}},
			newTypesEnabled: true,
			resolvedTool:    commonv1.IdeType_IDE_TYPE_KIRO,
			want:            []wantArtifact{{state: visible}},
		},
		{
			// A command carrying JSON-special chars (a quote) appears escaped in the body;
			// the copy-safety check must match the escaped form, not false-hide a valid hook.
			name: "hook command with json special chars stays visible",
			result: &DerivationResult{Hooks: []DerivedArtifact{func() DerivedArtifact {
				h := validHook("guard-secrets", "PostToolUse")
				h.Matcher = ""
				// Both a quote (JSON-escaped in the body) and shell operators &&/< (which a
				// literal body carries verbatim, but json.Marshal would HTML-escape) must match.
				h.Command = `go build && echo "ok" < /dev/null`
				h.Body = `{"hooks":{"PostToolUse":[{"command":"go build && echo \"ok\" < /dev/null"}]}}`
				return h
			}()}},
			newTypesEnabled: true,
			resolvedTool:    commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
			want:            []wantArtifact{{state: visible}},
		},
		{
			name: "hook exempt from repo-source path grounding",
			result: &DerivationResult{Hooks: []DerivedArtifact{func() DerivedArtifact {
				h := validHook("format-on-edit", "PostToolUse")
				h.CitedPaths = []string{"fake/ghost.go"} // would hide a rule; a hook is exempt
				return h
			}()}},
			repoTree:        treeFixture(),
			newTypesEnabled: true,
			resolvedTool:    commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
			want:            []wantArtifact{{state: visible}},
		},
		{
			name: "agent citing absent path hidden",
			result: &DerivationResult{Agents: []DerivedArtifact{func() DerivedArtifact {
				a := validAgent("debugger", "debugger")
				a.CitedPaths = []string{"fake/ghost.go"}
				return a
			}()}},
			repoTree:        treeFixture(),
			newTypesEnabled: true,
			resolvedTool:    commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
			want:            []wantArtifact{{state: hidden, validator: validatorPathsExist}},
			wantNoneVis:     true,
		},
		{
			name:            "hook with no resolved tool hidden",
			result:          &DerivationResult{Hooks: []DerivedArtifact{validHook("format-on-edit", "PostToolUse")}},
			newTypesEnabled: true,
			resolvedTool:    commonv1.IdeType_IDE_TYPE_UNSPECIFIED,
			want:            []wantArtifact{{state: hidden, validator: validatorHookTool}},
			wantNoneVis:     true,
		},
		{
			// Symmetric with the tool-less hook: no resolved tool → no agent-dir
			// destination exists, so a named agent must still be hidden.
			name:            "agent with no resolved tool hidden",
			result:          &DerivationResult{Agents: []DerivedArtifact{validAgent("debugger", "debugger")}},
			newTypesEnabled: true,
			resolvedTool:    commonv1.IdeType_IDE_TYPE_UNSPECIFIED,
			want:            []wantArtifact{{state: hidden, validator: validatorAgentTool}},
			wantNoneVis:     true,
		},
		{
			// A tool-less run drops every hook transiently as hook_tool BEFORE judging its
			// event, so even an empty-event hook is hook_tool (not the persisted hook_event) —
			// a later tool-resolving run must re-judge and surface a good hook.
			name: "tool-less hook with empty event is hook_tool not hook_event",
			result: &DerivationResult{Hooks: []DerivedArtifact{func() DerivedArtifact {
				h := validHook("format-on-edit", "PostToolUse")
				h.Event = ""
				return h
			}()}},
			newTypesEnabled: true,
			resolvedTool:    commonv1.IdeType_IDE_TYPE_UNSPECIFIED,
			want:            []wantArtifact{{state: hidden, validator: validatorHookTool}},
			wantNoneVis:     true,
		},
		{
			// In-run duplicate detection is per (type, slug): a rule and a hook that share a
			// slug are distinct artifacts, so BOTH stay visible (neither is flagged as a
			// duplicate of the other).
			name: "rule and hook sharing a slug are not cross-type duplicates",
			result: &DerivationResult{
				Rules: []DerivedArtifact{validRule("Shared name across types")},
				Hooks: []DerivedArtifact{validHook(slugFromTitle("Shared name across types"), "PostToolUse")},
			},
			newTypesEnabled: true,
			resolvedTool:    commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
			want: []wantArtifact{
				{state: visible},
				{state: visible},
			},
		},
		{
			name: "org-scope hook hidden",
			result: &DerivationResult{Hooks: []DerivedArtifact{func() DerivedArtifact {
				h := validHook("format-on-edit", "PostToolUse")
				h.Scope = "org"
				return h
			}()}},
			newTypesEnabled: true,
			resolvedTool:    commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
			want:            []wantArtifact{{state: hidden, validator: validatorOrgScope}},
			wantNoneVis:     true,
		},
		{
			name: "newTypesEnabled false drops hooks and agents, leaves rule",
			result: &DerivationResult{
				Rules:  []DerivedArtifact{validRule("Wrap errors with context")},
				Hooks:  []DerivedArtifact{validHook("format-on-edit", "PostToolUse")},
				Agents: []DerivedArtifact{validAgent("debugger", "debugger")},
			},
			newTypesEnabled: false,
			resolvedTool:    commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
			want: []wantArtifact{
				{state: visible},
				{state: hidden, validator: validatorNewTypesDisabled},
				{state: hidden, validator: validatorNewTypesDisabled},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			libraryDAO := daomocks.NewMockIPromptLibraryDAO(ctrl)
			stubNoPinnedSlugs(libraryDAO)
			libraryDAO.EXPECT().
				ListDismissedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateTestRepo).
				Return(nil, nil)
			libraryDAO.EXPECT().
				ListRecentHiddenFailedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateTestRepo, gomock.Any(), 8).
				Return(nil, nil)
			// Session floors 0 keep the template session-signal check inert; caps high so
			// this table never trims.
			gate := NewQualityGate(libraryDAO, func() int { return 8 }, func() int { return 0 }, func() int { return 0 }, func() int { return 50 }, func() int { return 50 })

			graded, err := gate.Grade(context.Background(), gateTestOrgID, gateTestRepo, tc.result, tc.repoTree, nil, 0, 0, false, tc.newTypesEnabled, tc.resolvedTool, nil)
			require.NoError(t, err)
			require.Len(t, graded.Artifacts, len(tc.want))
			for i, want := range tc.want {
				assert.Equal(t, want.state, graded.Artifacts[i].SurfaceState, "artifact %d surface_state", i)
				assert.Equal(t, want.validator, graded.Artifacts[i].FailedValidator, "artifact %d failed_validator", i)
			}
			assert.Equal(t, tc.wantNoneVis, graded.NoneVisible())
		})
	}
}

// TestQualityGate_Grade_UnknownEventBumpsCounter pins the contract: an event unknown across
// EVERY mapped tool (genuine vocabulary drift, not a cross-tool mismatch) keeps the hook
// VISIBLE but bumps the dedicated drift counter (separate from the gate-failure metric). A
// globally-unknown event is used deliberately — a cross-tool-known event now blocks instead.
func TestQualityGate_Grade_UnknownEventBumpsCounter(t *testing.T) {
	ctrl := gomock.NewController(t)
	libraryDAO := daomocks.NewMockIPromptLibraryDAO(ctrl)
	stubNoPinnedSlugs(libraryDAO)
	libraryDAO.EXPECT().ListDismissedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateTestRepo).Return(nil, nil)
	libraryDAO.EXPECT().ListRecentHiddenFailedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateTestRepo, gomock.Any(), 8).Return(nil, nil)
	gate := NewQualityGate(libraryDAO, func() int { return 8 }, func() int { return 0 }, func() int { return 0 }, func() int { return 50 }, func() int { return 50 })

	before := testutil.ToFloat64(hookEventUnknownTotal.WithLabelValues(commonv1.IdeType_IDE_TYPE_CURSOR.String()))
	graded, err := gate.Grade(context.Background(), gateTestOrgID, gateTestRepo,
		&DerivationResult{Hooks: []DerivedArtifact{cursorHook("lint-on-shell", "onFooBarNovel")}},
		nil, nil, 0, 0, false, true, commonv1.IdeType_IDE_TYPE_CURSOR, nil)
	require.NoError(t, err)
	require.Len(t, graded.Artifacts, 1)
	assert.Equal(t, promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE, graded.Artifacts[0].SurfaceState)
	after := testutil.ToFloat64(hookEventUnknownTotal.WithLabelValues(commonv1.IdeType_IDE_TYPE_CURSOR.String()))
	assert.Equal(t, 1.0, after-before, "globally-unknown event bumps the drift counter by 1")
}

// TestQualityGate_Grade_CrossToolEventBumpsBlockedCounter pins the observability of the
// cross-tool block: a suppressed cross-tool event is metric-exempt from the failure metric
// (a transient tool-scoped drop), so it must bump the dedicated cross-tool-blocked counter —
// the signal that a tool's allowlist may need a now-valid event added. It must NOT bump the
// drift counter (that one is only for events unknown across every tool).
func TestQualityGate_Grade_CrossToolEventBumpsBlockedCounter(t *testing.T) {
	ctrl := gomock.NewController(t)
	libraryDAO := daomocks.NewMockIPromptLibraryDAO(ctrl)
	stubNoPinnedSlugs(libraryDAO)
	libraryDAO.EXPECT().ListDismissedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateTestRepo).Return(nil, nil)
	libraryDAO.EXPECT().ListRecentHiddenFailedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateTestRepo, gomock.Any(), 8).Return(nil, nil)
	gate := NewQualityGate(libraryDAO, func() int { return 8 }, func() int { return 0 }, func() int { return 0 }, func() int { return 50 }, func() int { return 50 })

	blockedBefore := testutil.ToFloat64(hookEventCrossToolBlockedTotal.WithLabelValues(commonv1.IdeType_IDE_TYPE_CURSOR.String()))
	driftBefore := testutil.ToFloat64(hookEventUnknownTotal.WithLabelValues(commonv1.IdeType_IDE_TYPE_CURSOR.String()))
	graded, err := gate.Grade(context.Background(), gateTestOrgID, gateTestRepo,
		&DerivationResult{Hooks: []DerivedArtifact{cursorHook("lint-on-shell", "SubagentStop")}}, // Claude-only event on a Cursor repo
		nil, nil, 0, 0, false, true, commonv1.IdeType_IDE_TYPE_CURSOR, nil)
	require.NoError(t, err)
	require.Len(t, graded.Artifacts, 1)
	assert.Equal(t, promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_HIDDEN_FAILED_EVAL, graded.Artifacts[0].SurfaceState)
	blockedAfter := testutil.ToFloat64(hookEventCrossToolBlockedTotal.WithLabelValues(commonv1.IdeType_IDE_TYPE_CURSOR.String()))
	driftAfter := testutil.ToFloat64(hookEventUnknownTotal.WithLabelValues(commonv1.IdeType_IDE_TYPE_CURSOR.String()))
	assert.Equal(t, 1.0, blockedAfter-blockedBefore, "cross-tool event bumps the cross-tool-blocked counter by 1")
	assert.Equal(t, 0.0, driftAfter-driftBefore, "cross-tool event must NOT bump the drift counter")
}

// TestQualityGate_Grade_CrossToolDropDoesNotReserveSlug guards the transient-drop-vs-duplicate
// interaction in BOTH processing orders: a cross-tool hook is a transient drop, so it must be
// excluded from dedup on both sides — it must neither reserve its slug (which would stamp a
// same-slug VALID sibling duplicate_slug, leaving no visible hook) nor be read as a duplicate of
// an already-seen valid sibling (which would report the persisted duplicate_slug instead of the
// transient hook_event_tool_scoped, re-persisting it and poisoning the recent-hidden dedup). The
// valid hook must surface, and the cross-tool hook must report the transient validator either way.
func TestQualityGate_Grade_CrossToolDropDoesNotReserveSlug(t *testing.T) {
	// SubagentStop is Claude-only (a cross-tool event on Cursor); beforeShellExecution is native Cursor.
	crossTool := func() DerivedArtifact { return cursorHook("guard-shell", "SubagentStop") }
	valid := func() DerivedArtifact { return cursorHook("guard-shell", "beforeShellExecution") }
	tests := []struct {
		name  string
		hooks []DerivedArtifact
	}{
		{name: "cross-tool first, valid second", hooks: []DerivedArtifact{crossTool(), valid()}},
		{name: "valid first, cross-tool second", hooks: []DerivedArtifact{valid(), crossTool()}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			libraryDAO := daomocks.NewMockIPromptLibraryDAO(ctrl)
			stubNoPinnedSlugs(libraryDAO)
			libraryDAO.EXPECT().ListDismissedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateTestRepo).Return(nil, nil)
			libraryDAO.EXPECT().ListRecentHiddenFailedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateTestRepo, gomock.Any(), 8).Return(nil, nil)
			gate := NewQualityGate(libraryDAO, func() int { return 8 }, func() int { return 0 }, func() int { return 0 }, func() int { return 50 }, func() int { return 50 })

			graded, err := gate.Grade(context.Background(), gateTestOrgID, gateTestRepo,
				&DerivationResult{Hooks: tc.hooks},
				nil, nil, 0, 0, false, true, commonv1.IdeType_IDE_TYPE_CURSOR, nil)
			require.NoError(t, err)
			require.Len(t, graded.Artifacts, 2)

			var visibleCount int
			for _, a := range graded.Artifacts {
				if a.SurfaceState == promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE {
					visibleCount++
					assert.Equal(t, "beforeShellExecution", a.Artifact.Event, "the visible hook is the valid Cursor one")
				} else {
					// The cross-tool hook is dropped as a TRANSIENT tool-scoped drop, never a persisted duplicate_slug.
					assert.Equal(t, validatorHookEventToolScoped, a.FailedValidator, "the cross-tool hook is a transient drop, not a duplicate")
				}
			}
			assert.Equal(t, 1, visibleCount, "the valid same-slug hook must surface regardless of order")
		})
	}
}

// TestQualityGate_Grade_PinnedPreserved covers the suppression a pin owes: an admin pinned a
// recommendation, so the pinned row is already holding its place on the board and a re-derived
// copy would surface beside it as a duplicate. Three properties, each of which the fix would be
// wrong without: the duplicate is dropped; the drop is TRANSIENT (so an unpin makes the slug
// derivable again instead of leaving it suppressed by the recent-hidden dedup); and the
// suppression is keyed per (type, slug), so a pinned rule cannot silence a same-slug hook.
func TestQualityGate_Grade_PinnedPreserved(t *testing.T) {
	pinnedRule := []model.ArtifactSlugRef{{
		ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE,
		Slug:         slugFromTitle("Wrap errors with context"),
	}}
	tests := []struct {
		name          string
		pinned        []model.ArtifactSlugRef
		result        *DerivationResult
		wantValidator string
		wantVisible   bool
	}{
		{
			name:          "a re-derived pinned rule is dropped so the board keeps one row",
			pinned:        pinnedRule,
			result:        &DerivationResult{Rules: []DerivedArtifact{validRule("Wrap errors with context")}},
			wantValidator: validatorPinnedPreserved,
		},
		{
			name:        "an unrelated rule still surfaces",
			pinned:      pinnedRule,
			result:      &DerivationResult{Rules: []DerivedArtifact{validRule("Name migrations after their table")}},
			wantVisible: true,
		},
		{
			// The per-type key: a pinned RULE must not suppress a different type that
			// happens to share the slug — they are distinct artifacts.
			name: "a pinned rule does not suppress a same-slug artifact of another type",
			pinned: []model.ArtifactSlugRef{{
				ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL,
				Slug:         slugFromTitle("Wrap errors with context"),
			}},
			result:      &DerivationResult{Rules: []DerivedArtifact{validRule("Wrap errors with context")}},
			wantVisible: true,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			libraryDAO := daomocks.NewMockIPromptLibraryDAO(ctrl)
			libraryDAO.EXPECT().ListDismissedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateTestRepo).Return(nil, nil)
			libraryDAO.EXPECT().ListRecentHiddenFailedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateTestRepo, gomock.Any(), 8).Return(nil, nil)
			libraryDAO.EXPECT().ListPinnedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateTestRepo).Return(tt.pinned, nil)
			gate := NewQualityGate(libraryDAO, func() int { return 8 }, func() int { return 0 }, func() int { return 0 }, func() int { return 50 }, func() int { return 50 })

			graded, err := gate.Grade(context.Background(), gateTestOrgID, gateTestRepo, tt.result,
				nil, nil, 0, 0, false, true, commonv1.IdeType_IDE_TYPE_UNSPECIFIED, nil)
			require.NoError(t, err)
			require.Len(t, graded.Artifacts, 1)

			if tt.wantVisible {
				assert.Equal(t, promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE, graded.Artifacts[0].SurfaceState)
				assert.Empty(t, graded.Artifacts[0].FailedValidator)
				return
			}
			assert.Equal(t, tt.wantValidator, graded.Artifacts[0].FailedValidator)
			assert.True(t, isTransientDrop(graded.Artifacts[0].FailedValidator),
				"a pinned-preserved drop must be transient — persisting it would keep the slug suppressed by the recent-hidden dedup long after an unpin")
		})
	}
}

// TestQualityGate_Grade_PinnedPreservedYieldsToRealDefect pins the precedence the other way from
// the transient drops above: a re-derived copy of a pinned artifact that is ALSO malformed must
// report its real defect and persist, feeding the dedup. Labelling it pinned_preserved would drop
// it silently and lose the signal that the LLM is emitting a broken artifact.
func TestQualityGate_Grade_PinnedPreservedYieldsToRealDefect(t *testing.T) {
	ctrl := gomock.NewController(t)
	libraryDAO := daomocks.NewMockIPromptLibraryDAO(ctrl)
	libraryDAO.EXPECT().ListDismissedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateTestRepo).Return(nil, nil)
	libraryDAO.EXPECT().ListRecentHiddenFailedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateTestRepo, gomock.Any(), 8).Return(nil, nil)
	libraryDAO.EXPECT().ListPinnedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateTestRepo).
		Return([]model.ArtifactSlugRef{{ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE, Slug: slugFromTitle("Wrap errors with context")}}, nil)
	gate := NewQualityGate(libraryDAO, func() int { return 8 }, func() int { return 0 }, func() int { return 0 }, func() int { return 50 }, func() int { return 50 })

	broken := validRule("Wrap errors with context")
	broken.Body = "too short" // a genuine quality failure, malformed regardless of the pin

	graded, err := gate.Grade(context.Background(), gateTestOrgID, gateTestRepo,
		&DerivationResult{Rules: []DerivedArtifact{broken}},
		nil, nil, 0, 0, false, true, commonv1.IdeType_IDE_TYPE_UNSPECIFIED, nil)
	require.NoError(t, err)
	require.Len(t, graded.Artifacts, 1)
	assert.Equal(t, validatorBodyMinLength, graded.Artifacts[0].FailedValidator,
		"a malformed artifact must record its real defect, not be written off as a pin")
	assert.False(t, isTransientDrop(graded.Artifacts[0].FailedValidator), "a real quality failure persists and feeds the dedup")
}

// TestQualityGate_Grade_CrossToolDropBeatsRecentHidden pins the precedence of the transient
// tool-context drop over the persisting recent-hidden suppression: a cross-tool hook whose slug is
// already in the recent-hidden set must report the TRANSIENT hook_event_tool_scoped (dropped, not
// persisted), NOT not_dismissed (which would re-persist the hide and renew the recent-hidden entry,
// keeping the slug suppressed even after a later run resolves the owning tool — the poison this avoids).
func TestQualityGate_Grade_CrossToolDropBeatsRecentHidden(t *testing.T) {
	ctrl := gomock.NewController(t)
	libraryDAO := daomocks.NewMockIPromptLibraryDAO(ctrl)
	stubNoPinnedSlugs(libraryDAO)
	libraryDAO.EXPECT().ListDismissedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateTestRepo).Return(nil, nil)
	// A prior run recent-hid this slug for HOOK.
	libraryDAO.EXPECT().ListRecentHiddenFailedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateTestRepo, gomock.Any(), 8).
		Return([]model.ArtifactSlugRef{{ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK, Slug: "guard-shell"}}, nil)
	gate := NewQualityGate(libraryDAO, func() int { return 8 }, func() int { return 0 }, func() int { return 0 }, func() int { return 50 }, func() int { return 50 })

	graded, err := gate.Grade(context.Background(), gateTestOrgID, gateTestRepo,
		&DerivationResult{Hooks: []DerivedArtifact{cursorHook("guard-shell", "SubagentStop")}}, // cross-tool AND recent-hidden
		nil, nil, 0, 0, false, true, commonv1.IdeType_IDE_TYPE_CURSOR, nil)
	require.NoError(t, err)
	require.Len(t, graded.Artifacts, 1)
	assert.Equal(t, promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_HIDDEN_FAILED_EVAL, graded.Artifacts[0].SurfaceState)
	assert.Equal(t, validatorHookEventToolScoped, graded.Artifacts[0].FailedValidator, "the transient cross-tool drop takes precedence over not_dismissed")
}

// TestQualityGate_Grade_TypeScopeDropNotCounted asserts a type-scope drop (a feature-disabled
// org dropping a HOOK as new_types_disabled) does NOT increment the quality-gate failure metric
// — those drops fire on every ordinary feature-off run and would swamp the dashboard.
func TestQualityGate_Grade_TypeScopeDropNotCounted(t *testing.T) {
	ctrl := gomock.NewController(t)
	libraryDAO := daomocks.NewMockIPromptLibraryDAO(ctrl)
	stubNoPinnedSlugs(libraryDAO)
	libraryDAO.EXPECT().ListDismissedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateTestRepo).Return(nil, nil)
	libraryDAO.EXPECT().ListRecentHiddenFailedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateTestRepo, gomock.Any(), 8).Return(nil, nil)
	gate := NewQualityGate(libraryDAO, func() int { return 8 }, func() int { return 0 }, func() int { return 0 }, func() int { return 50 }, func() int { return 50 })

	before := testutil.ToFloat64(qualityGateFailuresTotal.WithLabelValues(validatorNewTypesDisabled, artifactTypeLabel(promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK)))
	_, err := gate.Grade(context.Background(), gateTestOrgID, gateTestRepo,
		&DerivationResult{Hooks: []DerivedArtifact{validHook("format-on-edit", "PostToolUse")}},
		nil, nil, 0, 0, false, false /*newTypesEnabled — the feature off → new_types_disabled drop*/, commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, nil)
	require.NoError(t, err)
	after := testutil.ToFloat64(qualityGateFailuresTotal.WithLabelValues(validatorNewTypesDisabled, artifactTypeLabel(promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK)))
	assert.Equal(t, 0.0, after-before, "a type-scope drop must not count as a quality-gate failure")
}

// TestQualityGate_Grade_HookAgentSessionSignal pins the per-artifact session-visibility gate for
// Hooks and Agents (feature on; the review floor already admitted the repo at admission). Below either
// low/high session-bucket floor the hook/agent is HIDDEN as hook_session_signal / agent_session_signal
// (drop-not-persist — it re-surfaces once the buckets fill); at/above both floors it is VISIBLE. It
// mirrors the Template session-signal gate, reusing the same minLowBucket/minHighBucket floors.
func TestQualityGate_Grade_HookAgentSessionSignal(t *testing.T) {
	visible := promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE
	hidden := promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_HIDDEN_FAILED_EVAL
	tests := []struct {
		name                    string
		result                  *DerivationResult
		sessionLow, sessionHigh int
		wantState               promptlibraryv1.ArtifactSurfaceState
		wantValidator           string
	}{
		{name: "hook hidden below low bucket", result: &DerivationResult{Hooks: []DerivedArtifact{validHook("format-on-edit", "PostToolUse")}}, sessionLow: 10, sessionHigh: 50, wantState: hidden, wantValidator: validatorHookSessionSignal},
		{name: "hook hidden below high bucket", result: &DerivationResult{Hooks: []DerivedArtifact{validHook("format-on-edit", "PostToolUse")}}, sessionLow: 50, sessionHigh: 10, wantState: hidden, wantValidator: validatorHookSessionSignal},
		{name: "hook visible when both buckets met", result: &DerivationResult{Hooks: []DerivedArtifact{validHook("format-on-edit", "PostToolUse")}}, sessionLow: 50, sessionHigh: 50, wantState: visible},
		{name: "agent hidden below floor", result: &DerivationResult{Agents: []DerivedArtifact{validAgent("debugger", "debugger")}}, sessionLow: 10, sessionHigh: 10, wantState: hidden, wantValidator: validatorAgentSessionSignal},
		{name: "agent visible when both buckets met", result: &DerivationResult{Agents: []DerivedArtifact{validAgent("debugger", "debugger")}}, sessionLow: 50, sessionHigh: 50, wantState: visible},
		{
			// A MALFORMED hook on a low-session repo must report its persisting malformation
			// (hook_event), NOT be masked as the transient hook_session_signal — the structural
			// checks win over the session-visibility gate.
			name: "malformed hook (empty event) below floor reports hook_event, not session",
			result: &DerivationResult{Hooks: []DerivedArtifact{func() DerivedArtifact {
				h := validHook("format-on-edit", "PostToolUse")
				h.Event = ""
				return h
			}()}},
			sessionLow: 10, sessionHigh: 10, wantState: hidden, wantValidator: validatorHookEvent,
		},
		{
			// Same precedence for agents: a malformed agent below the session floor reports
			// agent_description, not agent_session_signal.
			name: "malformed agent (empty description) below floor reports agent_description, not session",
			result: &DerivationResult{Agents: []DerivedArtifact{func() DerivedArtifact {
				a := validAgent("debugger", "debugger")
				a.Description = ""
				return a
			}()}},
			sessionLow: 10, sessionHigh: 10, wantState: hidden, wantValidator: validatorAgentDescription,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			libraryDAO := daomocks.NewMockIPromptLibraryDAO(ctrl)
			stubNoPinnedSlugs(libraryDAO)
			libraryDAO.EXPECT().ListDismissedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateTestRepo).Return(nil, nil)
			libraryDAO.EXPECT().ListRecentHiddenFailedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateTestRepo, gomock.Any(), 8).Return(nil, nil)
			// Low/high bucket floors of 30; caps high so this table never trims.
			gate := NewQualityGate(libraryDAO, func() int { return 8 }, func() int { return 30 }, func() int { return 30 }, func() int { return 50 }, func() int { return 50 })

			graded, err := gate.Grade(context.Background(), gateTestOrgID, gateTestRepo, tc.result, nil, nil,
				tc.sessionLow, tc.sessionHigh, false /*securityOnly*/, true /*newTypesEnabled*/, commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, nil)
			require.NoError(t, err)
			require.Len(t, graded.Artifacts, 1)
			assert.Equal(t, tc.wantState, graded.Artifacts[0].SurfaceState)
			assert.Equal(t, tc.wantValidator, graded.Artifacts[0].FailedValidator)
		})
	}
}

// TestQualityGate_Grade_SecurityOnlyHooksMatrix pins the contract: a securityOnly run
// surfaces the secret HOOK (evidence-matched + resolved tool) but keeps every other type
// scoped out.
func TestQualityGate_Grade_SecurityOnlyHooksMatrix(t *testing.T) {
	visible := promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE
	hidden := promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_HIDDEN_FAILED_EVAL
	security := &promptlibraryv1.SecuritySummary{ApiKeyLeak: 1}

	securityHook := func() DerivedArtifact {
		h := validHook("protect-api-keys", "PreToolUse")
		h.AddressesIssues = []string{"api-key-leak"}
		return h
	}

	tests := []struct {
		name          string
		artifact      DerivedArtifact
		artifactType  promptlibraryv1.ArtifactType
		resolvedTool  commonv1.IdeType
		wantState     promptlibraryv1.ArtifactSurfaceState
		wantValidator string
	}{
		{
			name:          "security hook with evidence and resolved tool is visible",
			artifact:      securityHook(),
			artifactType:  promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK,
			resolvedTool:  commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
			wantState:     visible,
			wantValidator: "",
		},
		{
			name: "non-security hook is scoped out",
			artifact: func() DerivedArtifact {
				h := validHook("format-on-edit", "PostToolUse")
				h.AddressesIssues = nil
				return h
			}(),
			artifactType:  promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK,
			resolvedTool:  commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
			wantState:     hidden,
			wantValidator: validatorSecurityScope,
		},
		{
			name:          "security hook with no resolved tool hidden by gate",
			artifact:      securityHook(),
			artifactType:  promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK,
			resolvedTool:  commonv1.IdeType_IDE_TYPE_UNSPECIFIED,
			wantState:     hidden,
			wantValidator: validatorHookTool,
		},
		{
			name: "agent is scoped out even with security evidence",
			artifact: func() DerivedArtifact {
				a := validAgent("secret-agent", "secret-agent")
				a.AddressesIssues = []string{"api-key-leak"}
				return a
			}(),
			artifactType:  promptlibraryv1.ArtifactType_ARTIFACT_TYPE_AGENT,
			resolvedTool:  commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
			wantState:     hidden,
			wantValidator: validatorSecurityScope,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			libraryDAO := daomocks.NewMockIPromptLibraryDAO(ctrl)
			stubNoPinnedSlugs(libraryDAO)
			libraryDAO.EXPECT().ListDismissedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateTestRepo).Return(nil, nil)
			libraryDAO.EXPECT().ListRecentHiddenFailedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateTestRepo, gomock.Any(), 8).Return(nil, nil)
			// Non-zero session floors (30/30) with the sessionLow/High=0 a securityOnly run carries
			// (BuildContext omits the SessionSummary): the securityOnly session-gate bypass must keep
			// the evidence-matched secret hook VISIBLE — without it the hook would be hidden as
			// hook_session_signal. This makes the case a regression guard for that bypass.
			gate := NewQualityGate(libraryDAO, func() int { return 8 }, func() int { return 30 }, func() int { return 30 }, func() int { return 50 }, func() int { return 50 })

			result := &DerivationResult{}
			switch tc.artifactType {
			case promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK:
				result.Hooks = []DerivedArtifact{tc.artifact}
			case promptlibraryv1.ArtifactType_ARTIFACT_TYPE_AGENT:
				result.Agents = []DerivedArtifact{tc.artifact}
			}
			graded, err := gate.Grade(context.Background(), gateTestOrgID, gateTestRepo, result, nil, security, 0, 0, true, true, tc.resolvedTool, nil)
			require.NoError(t, err)
			require.Len(t, graded.Artifacts, 1)
			assert.Equal(t, tc.wantState, graded.Artifacts[0].SurfaceState)
			assert.Equal(t, tc.wantValidator, graded.Artifacts[0].FailedValidator)
		})
	}
}

// TestQualityGate_Grade_TypeCap pins the contract: an over-cap run keeps the top-N
// hooks by priority and drops the rest as a transient (never-failed) type_cap_exceeded.
func TestQualityGate_Grade_TypeCap(t *testing.T) {
	ctrl := gomock.NewController(t)
	libraryDAO := daomocks.NewMockIPromptLibraryDAO(ctrl)
	stubNoPinnedSlugs(libraryDAO)
	libraryDAO.EXPECT().ListDismissedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateTestRepo).Return(nil, nil)
	libraryDAO.EXPECT().ListRecentHiddenFailedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateTestRepo, gomock.Any(), 8).Return(nil, nil)
	// Cap hooks at 2.
	gate := NewQualityGate(libraryDAO, func() int { return 8 }, func() int { return 0 }, func() int { return 0 }, func() int { return 2 }, func() int { return 50 })

	high := validHook("format-critical", "PostToolUse")
	high.Priority = "critical"
	mid := validHook("format-high", "PostToolUse")
	mid.Priority = "high"
	low := validHook("format-low", "PostToolUse")
	low.Priority = "low"
	graded, err := gate.Grade(context.Background(), gateTestOrgID, gateTestRepo,
		&DerivationResult{Hooks: []DerivedArtifact{low, high, mid}},
		nil, nil, 0, 0, false, true, commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, nil)
	require.NoError(t, err)
	require.Len(t, graded.Artifacts, 3)

	// The two highest-priority hooks (critical, high) stay visible; the low one is
	// trimmed as a transient cap drop, never FAILED.
	byslug := map[string]GradedArtifact{}
	for _, artifact := range graded.Artifacts {
		byslug[artifact.Artifact.Slug] = artifact
	}
	assert.Equal(t, promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE, byslug["format-critical"].SurfaceState)
	assert.Equal(t, promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE, byslug["format-high"].SurfaceState)
	assert.Equal(t, promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_HIDDEN_FAILED_EVAL, byslug["format-low"].SurfaceState)
	assert.Equal(t, validatorTypeCapExceeded, byslug["format-low"].FailedValidator)
}

func TestExtractBodyPaths(t *testing.T) {
	// Grounded in a realistic tree whose top-level dirs include common English-word
	// dir names (the namespace prose idioms draw from) plus a dotfile dir.
	topLevelDirs := map[string]struct{}{
		"backend": {}, "internal": {}, "docs": {}, "client": {}, ".github": {},
	}
	// Real tree files — the membership floor for grounding root + extensionless files.
	validPaths := map[string]struct{}{
		"backend/main.go": {}, "backend/dao/repo.go": {}, "backend/Dockerfile": {},
		"README.md": {}, "go.mod": {}, ".github/workflows/ci.yml": {},
	}
	tests := []struct {
		name string
		body string
		want []string
	}{
		{name: "plain file path under real dir", body: "Apply in backend/main.go now.", want: []string{"backend/main.go"}},
		{name: "backtick-wrapped path", body: "See `backend/dao/repo.go` for the query.", want: []string{"backend/dao/repo.go"}},
		{name: "trailing sentence period stripped", body: "It lives in backend/main.go.", want: []string{"backend/main.go"}},
		{name: "real dotfile dir is grounded", body: "Edit .github/workflows/ci.yml to add the job.", want: []string{".github/workflows/ci.yml"}},
		// Tree-membership branch: real root + extensionless files cited in prose.
		{name: "root file grounded by tree membership", body: "Update the README.md and go.mod before release.", want: []string{"README.md", "go.mod"}},
		{name: "extensionless file under real dir grounded", body: "Add the stage to backend/Dockerfile now.", want: []string{"backend/Dockerfile"}},
		{name: "url ignored", body: "Docs: https://github.com/org/repo/blob/main/x.go here.", want: nil},
		{name: "go import path ignored", body: "Import github.com/example/app/x.go please.", want: nil},
		{name: "directory without extension ignored", body: "Look in the backend/handlers directory.", want: nil},
		{name: "bare filename not in tree ignored", body: "The file config.yaml at the root.", want: nil},
		{name: "dedup repeated path", body: "backend/main.go and again backend/main.go.", want: []string{"backend/main.go"}},
		// Regression: prose with a slash must NOT be extracted (it would spuriously
		// hide a good artifact). First-segment-not-a-real-dir cases:
		{name: "http version not a path", body: "Prefer HTTP/1.1 over HTTP/2 here.", want: nil},
		{name: "key-value prose not a path", body: "It maps to key/value.pairs in the map.", want: nil},
		{name: "status codes not a path", body: "The endpoint returns 200/404.json codes.", want: nil},
		{name: "scoped npm package not a path", body: "Install @org/pkg/index.js to start.", want: nil},
		// Real-dir-prefixed prose killed by the citable-extension gate:
		{name: "version under real dir not a path", body: "The backend/v2.0 release is out.", want: nil},
		{name: "version under real dir not a path two", body: "Our docs/v1.2 guide explains it.", want: nil},
		{name: "word pair under real dir not a path", body: "An internal/external.split of concerns.", want: nil},
		// Hallucination under a real dir with a real extension IS extracted (then
		// fails tree grounding downstream); also the accepted architectural residual.
		{name: "hallucinated path under real dir extracted", body: "The fix belongs in backend/ghost.go per the analysis.", want: []string{"backend/ghost.go"}},
		{name: "real dir plus real extension is extracted (accepted residual)", body: "Use a client/server.go model.", want: []string{"client/server.go"}},
		// Non-canonical hallucination spellings are normalized before the heuristic,
		// so they can't bypass the first-segment check.
		{name: "dot-slash hallucination caught after normalize", body: "The fix is in ./backend/ghost.go per analysis.", want: []string{"backend/ghost.go"}},
		{name: "trailing-slash hallucination caught after normalize", body: "Put it in backend/ghost.go/ now.", want: []string{"backend/ghost.go"}},
		// Hallucinated extensionless well-known file under a real dir is caught.
		{name: "hallucinated extensionless file caught", body: "Add a stage to backend/Makefile please.", want: []string{"backend/Makefile"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.ElementsMatch(t, tc.want, extractBodyPaths(tc.body, topLevelDirs, validPaths))
		})
	}
}

func TestExtractBodyDirs(t *testing.T) {
	topLevelDirs := map[string]struct{}{
		"backend": {}, "internal": {}, "docs": {}, "client": {}, ".github": {},
	}
	tests := []struct {
		name string
		body string
		want []string
	}{
		{name: "dir under real top dir extracted", body: "Reuse helpers under backend/pkg/ across services.", want: []string{"backend/pkg"}},
		{name: "real top-level dir with trailing slash extracted", body: "Everything lives under backend/ here.", want: []string{"backend"}},
		{name: "dotfile dir extracted", body: "Workflows live in .github/workflows/ now.", want: []string{".github/workflows"}},
		{name: "backtick-wrapped dir", body: "See `backend/dao/` for the repos.", want: []string{"backend/dao"}},
		{name: "trailing sentence period keeps slash", body: "Put it under backend/pkg/.", want: []string{"backend/pkg"}},
		{name: "dedup repeated dir", body: "backend/pkg/ and again backend/pkg/.", want: []string{"backend/pkg"}},
		// No trailing slash — ambiguous, never extracted (the core false-positive guard).
		{name: "no trailing slash not a dir", body: "Look in the backend/handlers directory.", want: nil},
		// A slash-compound under a real top dir WITH a trailing slash is a directory
		// claim (grounded, hidden downstream if unreal — same signal as backend/ghost/);
		// the ordinary prose form of the compound carries no trailing slash and is safe.
		{name: "compound with trailing slash is a dir claim", body: "Keep changes across backend/frontend/ aligned.", want: []string{"backend/frontend"}},
		{name: "compound without trailing slash not a dir", body: "The backend/frontend split matters here.", want: nil},
		{name: "http version not a dir", body: "Prefer HTTP/1.1 over HTTP/2 here.", want: nil},
		{name: "import path not a dir", body: "Import github.com/example/app and use it.", want: nil},
		// Trailing slash but first segment is not a real top-level dir — not extracted.
		{name: "ratio with trailing slash not a dir", body: "The a/b/ split is irrelevant.", want: nil},
		{name: "fake top-level dir not extracted", body: "Store it under ghost/foo/ somewhere.", want: nil},
		// Trailing slash on a file-shaped token is a file reference, not a dir.
		{name: "file with trailing slash not a dir", body: "The fix is in backend/main.go/ here.", want: nil},
		{name: "url ignored", body: "Docs: https://github.com/org/repo/tree/main/ here.", want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.ElementsMatch(t, tc.want, extractBodyDirs(tc.body, topLevelDirs))
		})
	}
}

func TestNormalizeRepoPath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "already clean", in: "backend/main.go", want: "backend/main.go"},
		{name: "dot-slash prefix", in: "./backend/main.go", want: "backend/main.go"},
		{name: "leading slash", in: "/backend/main.go", want: "backend/main.go"},
		{name: "trailing slash", in: "backend/main.go/", want: "backend/main.go"},
		{name: "backslash separators", in: `backend\main.go`, want: "backend/main.go"},
		{name: "double slash", in: "backend//main.go", want: "backend/main.go"},
		{name: "parent traversal canonicalized, not a real entry", in: "backend/../etc/passwd", want: "etc/passwd"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, normalizeRepoPath(tc.in))
		})
	}
}

func TestNormalizeSlug(t *testing.T) {
	rule := promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE
	skill := promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL
	template := promptlibraryv1.ArtifactType_ARTIFACT_TYPE_TEMPLATE
	tests := []struct {
		name         string
		slug         string
		artifactType promptlibraryv1.ArtifactType
		want         string
	}{
		{name: "rule trim+lowercase", slug: "  Go-Error-Wrapping ", artifactType: rule, want: "go-error-wrapping"},
		{name: "template behaves like rule (no slash prefix)", slug: "  Commit-Hygiene ", artifactType: template, want: "commit-hygiene"},
		{name: "rule strips a stray leading slash", slug: "/go-error-wrapping", artifactType: rule, want: "go-error-wrapping"},
		{name: "skill keeps single leading slash", slug: "/Spec-Driven-Dev", artifactType: skill, want: "/spec-driven-dev"},
		{name: "skill adds missing leading slash", slug: "spec-driven-dev", artifactType: skill, want: "/spec-driven-dev"},
		{name: "skill collapses duplicate leading slashes", slug: "///spec-driven-dev", artifactType: skill, want: "/spec-driven-dev"},
		{name: "blank is empty (gate hides)", slug: "   ", artifactType: rule, want: ""},
		{name: "slash-only is empty (gate hides)", slug: "/", artifactType: skill, want: ""},
		{name: "over-long rule slug truncated to column width", slug: strings.Repeat("a", maxSlugLen+50), artifactType: rule, want: strings.Repeat("a", maxSlugLen)},
		{name: "over-long skill slug truncated incl. slash prefix", slug: strings.Repeat("a", maxSlugLen+50), artifactType: skill, want: "/" + strings.Repeat("a", maxSlugLen-1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, normalizeSlug(tc.slug, tc.artifactType))
		})
	}
}

func TestDecodeRepoTreePaths(t *testing.T) {
	tests := []struct {
		name string
		tree *promptlibraryv1.RepoTree
		want []string
	}{
		{name: "nil tree", tree: nil, want: nil},
		{name: "empty structure", tree: &promptlibraryv1.RepoTree{}, want: nil},
		{
			name: "root and nested dirs",
			tree: &promptlibraryv1.RepoTree{DirectoryStructure: ".: README.md go.mod\nbackend: main.go\nbackend/dao: repo.go dao.go"},
			want: []string{"README.md", "go.mod", "backend/main.go", "backend/dao/repo.go", "backend/dao/dao.go"},
		},
		{
			name: "standalone full-path lines carry space-containing basenames",
			tree: &promptlibraryv1.RepoTree{DirectoryStructure: "backend: main.go\ndocs/user guide.md\nmy notes.md"},
			want: []string{"backend/main.go", "docs/user guide.md", "my notes.md"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DecodeRepoTreePaths(tc.tree)
			gotPaths := make([]string, 0, len(got))
			for path := range got {
				gotPaths = append(gotPaths, path)
			}
			assert.ElementsMatch(t, tc.want, gotPaths)
		})
	}
}

func TestDecodeRepoTreePaths_RoundTripsEncode(t *testing.T) {
	original := []string{
		"backend/main.go", "backend/dao/repo.go", "README.md", "cmd/server/run.go",
		"docs/user guide.md", "my release notes.md", // space-containing basenames round-trip via standalone lines
	}
	structure, truncated := encodeDirectoryGrouped(original, 0)
	require.False(t, truncated)

	decoded := DecodeRepoTreePaths(&promptlibraryv1.RepoTree{DirectoryStructure: structure})
	gotPaths := make([]string, 0, len(decoded))
	for path := range decoded {
		gotPaths = append(gotPaths, path)
	}
	assert.ElementsMatch(t, original, gotPaths)
}

func TestLLMClient_DeriveArtifacts(t *testing.T) {
	const deriveJSON = `{"rules":[{"title":"Wrap errors","body":"Wrap returned errors with context.","scope":"project","priority":"high","confidence":0.9,"cited_paths":["backend/main.go"]}],"templates":[],"skills":[]}`

	tests := []struct {
		name       string
		orgID      string
		handler    http.HandlerFunc
		serverless bool
		wantErr    bool
		wantRules  int
	}{
		{
			name:  "happy path parses result",
			orgID: gateTestOrgID,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"result":` + strconv.Quote(deriveJSON) + `,"task_type":"derive_org_artifacts","model":"gemini","provider":"google"}`))
			},
			wantRules: 1,
		},
		{
			name:  "non-2xx is an error",
			orgID: gateTestOrgID,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`{"detail":"derive output invalid"}`))
			},
			wantErr: true,
		},
		{
			name:  "malformed result json is an error",
			orgID: gateTestOrgID,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"result":"not-json","task_type":"derive_org_artifacts","model":"m","provider":"p"}`))
			},
			wantErr: true,
		},
		{
			name:       "empty orgID is rejected before any call",
			orgID:      "",
			serverless: true,
			wantErr:    true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			baseURL := "http://unused.invalid"
			if !tc.serverless {
				server := httptest.NewServer(tc.handler)
				defer server.Close()
				baseURL = server.URL
			}
			client := NewLLMClient(baseURL, nil, nil)
			result, rawOutput, err := client.DeriveArtifacts(context.Background(),
				&promptlibraryv1.AggregatedContext{OrgId: tc.orgID}, tc.orgID, "exec-1")
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Len(t, result.Rules, tc.wantRules)
			// The audit blob is the normalized Result bytes verbatim — the exact JSON
			// tetriz-py returned, not a re-marshal of the parsed struct.
			assert.JSONEq(t, deriveJSON, string(rawOutput))
		})
	}
}

// TestValidateHookBody pins the exported edit-time copy-safety check: a hook body
// must be valid JSON and must still reference the declared event + matcher (the firing
// trigger). It intentionally does NOT check the command — the proto documents command as
// admin-edited, so requiring the old command would reject a legitimate command edit.
func TestValidateHookBody(t *testing.T) {
	t.Parallel()
	const validBody = `{"hooks":{"PostToolUse":[{"matcher":"Edit","hooks":[{"type":"command","command":"gofmt -w $FILE"}]}]}}`
	tests := []struct {
		name    string
		body    string
		event   string
		matcher string
		wantErr bool
	}{
		{"valid JSON referencing event+matcher", validBody, "PostToolUse", "Edit", false},
		{"empty matcher still valid", `{"hooks":{"SessionStart":[{"note":"x"}]}}`, "SessionStart", "", false},
		{"not JSON", "PostToolUse Edit gofmt -w $FILE not json", "PostToolUse", "Edit", true},
		{"valid JSON dropping the event is rejected", `{"hooks":{"SessionStart":[{"note":"x"}]}}`, "PostToolUse", "Edit", true},
		{"valid JSON dropping the matcher is rejected", `{"hooks":{"PostToolUse":[{"note":"x"}]}}`, "PostToolUse", "Edit", true},
		// An admin may edit the command inside the body; the command is NOT checked (the proto
		// documents it as admin-edited), so a changed command with the same event+matcher passes.
		{"command changed but event+matcher kept is allowed", `{"hooks":{"PostToolUse":[{"matcher":"Edit","hooks":[{"type":"command","command":"prettier -w $FILE"}]}]}}`, "PostToolUse", "Edit", false},
		// A non-object body (JSON string / array) that merely embeds the tokens is not a
		// loadable hook config, so it is rejected even though it is "valid JSON".
		{"JSON string embedding the tokens is rejected", `"PostToolUse Edit gofmt -w $FILE"`, "PostToolUse", "Edit", true},
		{"JSON array embedding the tokens is rejected", `["PostToolUse","Edit","gofmt -w $FILE"]`, "PostToolUse", "Edit", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateHookBody(tc.body, tc.event, tc.matcher)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestQualityGate_Grade_CommittedArtifact covers the additive-dedup gate backstop:
// a net-new suggestion whose canonical slug matches an already-committed VCS artifact is
// dropped as validatorCommittedArtifact (transient), while a non-matching suggestion is visible.
func TestQualityGate_Grade_CommittedArtifact(t *testing.T) {
	visible := promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE
	hidden := promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_HIDDEN_FAILED_EVAL
	type wantArtifact struct {
		state     promptlibraryv1.ArtifactSurfaceState
		validator string
	}
	tests := []struct {
		name      string
		result    *DerivationResult
		committed map[CommittedKey]struct{}
		want      []wantArtifact
	}{
		{
			// A committed rule of the same name → the re-suggestion is dropped as
			// committed_artifact, not persisted (transient), so it never poisons the
			// recent-hidden dedup once the repo drops that rule.
			name:      "rule matching a committed artifact is dropped",
			result:    &DerivationResult{Rules: []DerivedArtifact{validRule("Wrap errors with context")}},
			committed: map[CommittedKey]struct{}{{Type: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE, Slug: normalizeSlug(slugFromTitle("Wrap errors with context"), promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE)}: {}},
			want:      []wantArtifact{{state: hidden, validator: validatorCommittedArtifact}},
		},
		{
			// A committed SKILL matches the "/"-prefixed derived skill slug — the committed
			// canonicalizer maps HarnessArtifactType.SKILL to the ArtifactType SKILL that
			// normalizeSlug "/"-prefixes, so the two slug spaces coincide.
			name:      "skill matching a committed skill is dropped",
			result:    &DerivationResult{Skills: []DerivedArtifact{validRule("Run the audit skill")}},
			committed: map[CommittedKey]struct{}{{Type: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL, Slug: normalizeSlug(slugFromTitle("Run the audit skill"), promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL)}: {}},
			want:      []wantArtifact{{state: hidden, validator: validatorCommittedArtifact}},
		},
		{
			name:      "non-matching suggestion stays visible",
			result:    &DerivationResult{Rules: []DerivedArtifact{validRule("Wrap errors with context")}},
			committed: map[CommittedKey]struct{}{{Type: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE, Slug: normalizeSlug(slugFromTitle("Some other committed rule"), promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE)}: {}},
			want:      []wantArtifact{{state: visible}},
		},
		{
			name:      "empty committed set is a no-op (recommender path)",
			result:    &DerivationResult{Rules: []DerivedArtifact{validRule("Wrap errors with context")}},
			committed: nil,
			want:      []wantArtifact{{state: visible}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			libraryDAO := daomocks.NewMockIPromptLibraryDAO(ctrl)
			stubNoPinnedSlugs(libraryDAO)
			libraryDAO.EXPECT().
				ListDismissedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateTestRepo).
				Return(nil, nil)
			libraryDAO.EXPECT().
				ListRecentHiddenFailedSlugs(gomock.Any(), gateTestOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateTestRepo, gomock.Any(), 8).
				Return(nil, nil)
			gate := NewQualityGate(libraryDAO, func() int { return 8 }, func() int { return 30 }, func() int { return 30 }, func() int { return 5 }, func() int { return 4 })

			graded, err := gate.Grade(context.Background(), gateTestOrgID, gateTestRepo, tc.result, nil, nil, 40, 40, false, false, commonv1.IdeType_IDE_TYPE_UNSPECIFIED, tc.committed)
			require.NoError(t, err)
			require.Len(t, graded.Artifacts, len(tc.want))
			for i, want := range tc.want {
				assert.Equal(t, want.state, graded.Artifacts[i].SurfaceState, "artifact %d surface_state", i)
				assert.Equal(t, want.validator, graded.Artifacts[i].FailedValidator, "artifact %d failed_validator", i)
			}
		})
	}
	// The committed drop MUST be transient — toModels drops it rather than persisting a
	// HIDDEN_FAILED_EVAL row, so a stale committed match can't suppress the slug via the
	// recent-hidden dedup after the repo removes that artifact.
	assert.True(t, isTransientDrop(validatorCommittedArtifact), "committed_artifact must be a transient drop")
}

// TestCommittedArtifactType_SlugSpaceIntersects is the load-bearing correctness proof for:
// the committed canonicalizer (kebab + normalizeSlug) lands in the SAME (type, slug) space as a
// derived suggestion, whereas artifactslug.PromptLibraryForm (which appends a sha256 digest) does
// not — so using PromptLibraryForm would make the dedup inert (never a match).
func TestCommittedArtifactType_SlugSpaceIntersects(t *testing.T) {
	// The committed type maps to the promptlibrary type for the same primitive, so the dedup is
	// keyed by (type, slug) — a committed rule never suppresses a derived hook of the same name.
	typeMap := map[commonv1.HarnessArtifactType]promptlibraryv1.ArtifactType{
		commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL,
		commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE:  promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE,
		commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_HOOK:  promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK,
		commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_AGENT: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_AGENT,
	}
	for committed, want := range typeMap {
		assert.Equal(t, want, committedArtifactType(committed), "committed %v must map type-for-type", committed)
	}
	// Types the derive flow never produces map to UNSPECIFIED and are excluded from the set, so a
	// committed command/CLAUDE.md can share a name with a derived artifact without suppressing it.
	for _, noDeriveEquiv := range []commonv1.HarnessArtifactType{
		commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_COMMAND,
		commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_CLAUDE_MD,
		commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_PERMISSIONS,
	} {
		assert.Equal(t, promptlibraryv1.ArtifactType_ARTIFACT_TYPE_UNSPECIFIED, committedArtifactType(noDeriveEquiv),
			"%v has no derive equivalent and must be excluded (UNSPECIFIED)", noDeriveEquiv)
	}

	// A committed skill named "pr" canonicalizes to the exact derived skill slug "/pr" …
	committedSkillSlug := normalizeSlug(artifactslug.Kebab("pr"), committedArtifactType(commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL))
	derivedSkillSlug := normalizeSlug("pr", promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL)
	assert.Equal(t, "/pr", committedSkillSlug)
	assert.Equal(t, derivedSkillSlug, committedSkillSlug, "committed canonicalizer must equal the derived slug form")

	// … and an underscore/space committed filename kebab-normalizes to the LLM's kebab slug, so
	// the dedup is not silently bypassed for non-kebab-named committed files.
	assert.Equal(t, "error-wrapping", normalizeSlug(artifactslug.Kebab("error_wrapping"), promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE))
	assert.Equal(t, "/pr-review", normalizeSlug(artifactslug.Kebab("pr_review"), promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL))
	// The kebab canonicalizer landing in the derived slug space above is the load-bearing
	// correctness property (marketplace catalog identity is out of scope for this engine).
}

// TestAddCommittedAvoidHint covers the prompt-hint half: committed slugs are appended
// (sorted, deduped) to AdminFeedback.DismissedRuleSlugs — the LLM avoid-list — creating the
// AdminFeedback message when absent, and a no-op for an empty set.
func TestAddCommittedAvoidHint(t *testing.T) {
	t.Run("empty set is a no-op and never allocates AdminFeedback", func(t *testing.T) {
		agg := &promptlibraryv1.AggregatedContext{}
		addCommittedAvoidHint(agg, nil)
		assert.Nil(t, agg.GetAdminFeedback())
	})
	t.Run("appends sorted, deduped, creating AdminFeedback when absent", func(t *testing.T) {
		agg := &promptlibraryv1.AggregatedContext{}
		addCommittedAvoidHint(agg, map[CommittedKey]struct{}{{Type: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL, Slug: "/pr"}: {}, {Type: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE, Slug: "go-error-wrapping"}: {}})
		assert.Equal(t, []string{"/pr", "go-error-wrapping"}, agg.GetAdminFeedback().GetDismissedRuleSlugs())
	})
	t.Run("unions with existing dismissed slugs without duplicating", func(t *testing.T) {
		agg := &promptlibraryv1.AggregatedContext{
			AdminFeedback: &promptlibraryv1.AdminFeedback{DismissedRuleSlugs: []string{"already-dismissed"}},
		}
		addCommittedAvoidHint(agg, map[CommittedKey]struct{}{{Type: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE, Slug: "already-dismissed"}: {}, {Type: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL, Slug: "/committed-skill"}: {}})
		assert.Equal(t, []string{"already-dismissed", "/committed-skill"}, agg.GetAdminFeedback().GetDismissedRuleSlugs())
	})
}
