// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// Local ids for the internal-package Security detector tests.
const (
	secOrgID  = "11111111-1111-1111-1111-111111111111"
	secExecID = "22222222-2222-2222-2222-222222222222"
	secRepoID = "33333333-3333-3333-3333-333333333333"
)

// permissionsArtifact builds a committed PERMISSIONS carrier artifact at the settings child path.
func permissionsArtifact(id string) *model.HarnessArtifact {
	return &model.HarnessArtifact{
		ID:         id,
		IdeType:    commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		Type:       commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_PERMISSIONS,
		SourcePath: ".claude/settings.json#permissions",
	}
}

// assertSecurityFinding asserts a finding carries the shared Security-builder invariants: SECURITY
// dimension, confidence 1.0, VISIBLE, DETECTED source.
func assertSecurityFinding(t *testing.T, finding *model.PromptLibraryItem) {
	t.Helper()
	require.NotNil(t, finding.Dimension)
	require.Equal(t, promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_SECURITY, *finding.Dimension)
	require.NotNil(t, finding.RecommendationSource)
	require.Equal(t, promptlibraryv1.ArtifactRecommendationSource_ARTIFACT_RECOMMENDATION_SOURCE_DETECTED, *finding.RecommendationSource)
	require.InDelta(t, 1.0, finding.ConfidenceScore, 1e-9)
	require.Equal(t, promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE, finding.SurfaceState)
}

func TestAllowGrantCategory(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		entry string
		want  string
	}{
		{name: "bare Bash is unbounded", entry: "Bash", want: "unbounded"},
		{name: "Bash(*) is unbounded", entry: "Bash(*)", want: "unbounded"},
		{name: "Bash(*:*) is unbounded", entry: "Bash(*:*)", want: "unbounded"},
		{name: "Bash(:*) empty prefix is unbounded", entry: "Bash(:*)", want: "unbounded"},
		{name: "case-insensitive bare bash", entry: "bash", want: "unbounded"},
		{name: "bounded exec Bash(git status) is not flagged", entry: "Bash(git status)", want: ""},
		// The Claude Code bounded-grant syntax `Bash(<prefix>:*)` is a concrete command prefix with
		// wildcarded args — BOUNDED, not the over-broad grant the old last-colon split misread it as.
		{name: "bounded-grant Bash(git status:*) is not flagged", entry: "Bash(git status:*)", want: ""},
		{name: "bounded-grant Bash(npm run test:*) is not flagged", entry: "Bash(npm run test:*)", want: ""},
		{name: "bare Write is mutating", entry: "Write", want: "mutating grant"},
		{name: "constrained Write(src/**) is still mutating", entry: "Write(src/**)", want: "mutating grant"},
		{name: "bare Edit is mutating", entry: "Edit", want: "mutating grant"},
		{name: "constrained Edit(config.json) is still mutating", entry: "Edit(config.json)", want: "mutating grant"},
		{name: "bare MultiEdit is mutating", entry: "MultiEdit", want: "mutating grant"},
		// Empty parens (`Bash()`, `WebFetch()`) carry an argument section that constrains nothing, so the
		// grant is unbounded exactly as a bare tool is. These cover isUnboundedArg's empty-arg branch,
		// which the bare-tool cases cannot reach (bare tools short-circuit on !hasArg) — see
		// TestIsUnboundedArg_EmptyParens for the direct guard on that branch.
		{name: "empty-parens Bash() is unbounded", entry: "Bash()", want: "unbounded"},
		{name: "empty-parens WebFetch() is unbounded", entry: "WebFetch()", want: "unbounded"},
		{name: "bare WebFetch is unbounded", entry: "WebFetch", want: "unbounded"},
		{name: "constrained WebFetch is not flagged", entry: "WebFetch(domain:example.com)", want: ""},
		{name: "keyed wildcard WebFetch(domain:*) is unbounded", entry: "WebFetch(domain:*)", want: "unbounded"},
		{name: "keyed empty-value WebFetch(domain:) is unbounded", entry: "WebFetch(domain:)", want: "unbounded"},
		{name: "bare WebSearch is unbounded", entry: "WebSearch", want: "unbounded"},
		{name: "unlisted tool Read is never flagged", entry: "Read", want: ""},
		{name: "unlisted tool wildcard Read(*) is never flagged", entry: "Read(*)", want: ""},
		{name: "empty entry", entry: "", want: ""},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, allowGrantCategory(tt.entry))
		})
	}
}

// TestIsUnboundedArg_EmptyParens is the direct guard on isUnboundedArg's empty-argument branch: an empty
// argument constraint leaves ANY unbounded-category tool unbounded — the exec-style tool (Bash) and the
// domain-grant tool (WebFetch) alike. It kills the flip-to-false mutation of the `if arg == ""` early
// return: with that branch returning false, Bash() and WebFetch() would both read as bounded and this
// test fails.
func TestIsUnboundedArg_EmptyParens(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		toolKey string
		arg     string
		want    bool
	}{
		{name: "exec-style Bash with empty arg is unbounded", toolKey: "bash", arg: "", want: true},
		{name: "exec-style Bash with whitespace-only arg is unbounded", toolKey: "bash", arg: "   ", want: true},
		{name: "domain-grant WebFetch with empty arg is unbounded", toolKey: "webfetch", arg: "", want: true},
		{name: "domain-grant WebSearch with empty arg is unbounded", toolKey: "websearch", arg: "", want: true},
		{name: "exec-style Bash with concrete prefix is bounded", toolKey: "bash", arg: "git status:*", want: false},
		{name: "domain-grant WebFetch with concrete value is bounded", toolKey: "webfetch", arg: "domain:example.com", want: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, isUnboundedArg(tt.toolKey, tt.arg))
		})
	}
}

func TestSecurityPermissionFindings(t *testing.T) {
	t.Parallel()
	const artifactID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	tests := []struct {
		name        string
		body        string
		noBody      bool // when true, the artifact's body was not fetched (absent from aggregate.bodies)
		wantGrants  int
		wantRefLike string // substring the single finding's referencePoint must carry (empty if none expected)
	}{
		{
			name:        "unbounded Bash grant flags one HIGH finding",
			body:        `{"permissions":{"allow":["Bash"],"deny":[]}}`,
			wantGrants:  1,
			wantRefLike: "allow Bash (unbounded)",
		},
		{
			// deny count deliberately not folded into the reference point — see securityPermissionFindings.
			name:        "deny rules do not change the over-broad grant reference point",
			body:        `{"permissions":{"allow":["Bash"],"deny":["Bash(rm -rf *)","Write(/etc/**)"]}}`,
			wantGrants:  1,
			wantRefLike: "allow Bash (unbounded)",
		},
		{
			name:        "constrained mutating Write grant still flags one HIGH finding",
			body:        `{"permissions":{"allow":["Write(src/**)"],"deny":[]}}`,
			wantGrants:  1,
			wantRefLike: "allow Write(src/**) (mutating grant)",
		},
		{
			name:       "bounded exec grant flags nothing",
			body:       `{"permissions":{"allow":["Bash(git status)"],"deny":[]}}`,
			wantGrants: 0,
		},
		{
			name:       "malformed body degrades to no findings",
			body:       `{"permissions":{"allow":`,
			wantGrants: 0,
		},
		{
			name:       "empty permissions block flags nothing",
			body:       `{"permissions":{}}`,
			wantGrants: 0,
		},
		{
			name:       "two over-broad grants flag two findings",
			body:       `{"permissions":{"allow":["Bash","WebFetch"],"deny":[]}}`,
			wantGrants: 2,
		},
		{
			name:       "no fetched body flags nothing",
			noBody:     true,
			wantGrants: 0,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			artifact := permissionsArtifact(artifactID)
			bodies := map[string]string{artifactID: tt.body}
			if tt.noBody {
				bodies = map[string]string{}
			}
			aggregate := harnessImproveAggregate{
				committed: []*model.HarnessArtifact{artifact},
				bodies:    bodies,
			}
			findings := securityPermissionFindings(context.Background(), secOrgID, secRepoID, secExecID, aggregate)
			require.Len(t, findings, tt.wantGrants)
			for _, finding := range findings {
				assertSecurityFinding(t, finding)
				require.Equal(t, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, finding.Scope)
				require.Equal(t, secRepoID, finding.ScopeValue)
				require.Equal(t, promptlibraryv1.ArtifactType_ARTIFACT_TYPE_PERMISSIONS, finding.ArtifactType)
				require.NotNil(t, finding.HarnessArtifactID)
				require.Equal(t, artifactID, *finding.HarnessArtifactID)
				require.Equal(t, int32(20), finding.Priority) // priorityHigh
				require.Contains(t, finding.Slug, ":security:permission-grant:")
			}
			if tt.wantRefLike != "" {
				require.Len(t, findings, 1)
				require.Equal(t, tt.wantRefLike, findings[0].Evidence.GetReferencePoint())
			}
		})
	}
}

func TestOrgMcpFindings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		names         []string
		wantProdNames []string // sorted server names expected to be prod-pointed CRITICAL
		wantSprawl    []string // sorted bases expected to sprawl MEDIUM (env-variant lens)
		wantSprawlRef []string // exact referencePoints expected on the env-sprawl findings (optional)
		wantNearDup   []string // sorted bare bases expected to fire the bare-alias near-dup MEDIUM
	}{
		{
			name:  "two env variants of one base sprawl; the prod variant is also prod-pointed",
			names: []string{"gh-prod", "gh-dev"},
			// The `prod` segment makes gh-prod prod-pointed (segment-exact match), so the sprawl
			// and prod-pointed lenses fire together.
			wantProdNames: []string{"gh-prod"},
			wantSprawl:    []string{"gh"},
		},
		{
			name:          "three env variants name the base and sorted envs in the reference point",
			names:         []string{"gh-prod", "gh-dev", "gh-staging"},
			wantProdNames: []string{"gh-prod"},
			wantSprawl:    []string{"gh"},
			wantSprawlRef: []string{"base gh across 3 envs: dev, prod, staging"},
		},
		{
			name:          "a production-spelled server is prod-pointed",
			names:         []string{"db-production"},
			wantProdNames: []string{"db-production"},
			wantSprawl:    nil,
		},
		{
			name:          "distinct non-env suffixes do not read as one sprawled base",
			names:         []string{"jira-api", "jira-webhooks"},
			wantProdNames: nil,
			wantSprawl:    nil,
		},
		{
			// Underscore-delimited env variants group under the same base (mcpBaseAndEnv uses the same
			// `-`/`_`/`.` set as the prod-marker split), so `db_prod`/`db_dev` sprawl. `db_prod` is also
			// prod-pointed (its `prod` segment matches).
			name:          "underscore-delimited env variants sprawl",
			names:         []string{"db_prod", "db_dev"},
			wantProdNames: []string{"db_prod"},
			wantSprawl:    []string{"db"},
		},
		{
			name:          "a single non-sprawled non-prod name yields nothing",
			names:         []string{"gh"},
			wantProdNames: nil,
			wantSprawl:    nil,
		},
		{
			name:        "a bare name and its suffixed alias fire one near-dup",
			names:       []string{"slack", "slack-mcp-server"},
			wantNearDup: []string{"slack"},
		},
		{
			name:  "a bare name that is not a full leading segment does not alias",
			names: []string{"gh", "github-mcp"},
			// `gh` is a string prefix of `github-mcp` but not a full `gh-` leading segment, so the
			// conservative near-dup check does NOT fire.
			wantProdNames: nil,
			wantSprawl:    nil,
			wantNearDup:   nil,
		},
		{
			name:        "a bare name aliased by both an env variant and a suffixed name fires one near-dup",
			names:       []string{"slack", "slack-dev", "slack-mcp-server"},
			wantNearDup: []string{"slack"},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			agg := OrgSecurityAggregate{OrgID: secOrgID, McpServerNames: tt.names}
			findings := OrgMcpFindings(agg, secExecID)

			var gotProd, gotSprawl, gotSprawlRef, gotNearDup []string
			for _, finding := range findings {
				assertSecurityFinding(t, finding)
				require.Equal(t, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_ORG, finding.Scope)
				require.Empty(t, finding.ScopeValue)
				require.Equal(t, promptlibraryv1.ArtifactType_ARTIFACT_TYPE_MCP_SERVER, finding.ArtifactType)
				require.Nil(t, finding.HarnessArtifactID)
				referencePoint := finding.Evidence.GetReferencePoint()
				switch {
				case strings.Contains(finding.Slug, ":security:mcp-prod-pointed:"):
					require.Equal(t, int32(30), finding.Priority) // priorityCritical
					gotProd = append(gotProd, referencePoint)
				case strings.Contains(finding.Slug, ":security:mcp-sprawl:"):
					require.Equal(t, int32(10), finding.Priority) // priorityMedium
					// Both mcp-sprawl variants share the family slug; the referencePoint prefix
					// distinguishes the env-variant lens ("base ...") from the bare-alias lens ("bare ...").
					switch {
					case strings.HasPrefix(referencePoint, "base "):
						gotSprawl = append(gotSprawl, strings.Fields(referencePoint)[1])
						gotSprawlRef = append(gotSprawlRef, referencePoint)
					case strings.HasPrefix(referencePoint, "bare "):
						gotNearDup = append(gotNearDup, strings.Fields(referencePoint)[1])
					default:
						t.Fatalf("unexpected mcp-sprawl referencePoint %q", referencePoint)
					}
				default:
					t.Fatalf("unexpected finding slug %q", finding.Slug)
				}
			}
			require.ElementsMatch(t, tt.wantProdNames, gotProd)
			require.ElementsMatch(t, tt.wantSprawl, gotSprawl)
			require.ElementsMatch(t, tt.wantNearDup, gotNearDup)
			if tt.wantSprawlRef != nil {
				require.ElementsMatch(t, tt.wantSprawlRef, gotSprawlRef)
			}
		})
	}
}

func TestOrgCoverageGapFindings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		agg          OrgSecurityAggregate
		wantFinding  bool
		wantUncoverd string // expected referencePoint when a finding is emitted
	}{
		{
			name: "telemetry present and one repo uncovered fires one finding",
			agg: OrgSecurityAggregate{
				OrgID:              secOrgID,
				SecurityFlagCounts: map[string]int{"api-key-leak": 3},
				HookCoverageByRepo: map[string]bool{"repoA": false, "repoB": true},
			},
			wantFinding:  true,
			wantUncoverd: "repoA",
		},
		{
			name: "all repos covered emits nothing (finding closes)",
			agg: OrgSecurityAggregate{
				OrgID:              secOrgID,
				SecurityFlagCounts: map[string]int{"api-key-leak": 3},
				HookCoverageByRepo: map[string]bool{"repoA": true, "repoB": true},
			},
			wantFinding: false,
		},
		{
			name: "empty coverage map emits nothing (no repos to evaluate)",
			agg: OrgSecurityAggregate{
				OrgID:              secOrgID,
				SecurityFlagCounts: map[string]int{"api-key-leak": 3},
				HookCoverageByRepo: map[string]bool{},
			},
			wantFinding: false,
		},
		{
			name: "no telemetry of any kind emits nothing even with an uncovered repo",
			agg: OrgSecurityAggregate{
				OrgID:              secOrgID,
				SecurityFlagCounts: map[string]int{"api-key-leak": 0},
				HookCoverageByRepo: map[string]bool{"repoA": false},
			},
			wantFinding: false,
		},
		{
			name: "MCP usage without security flags does not fire (MCP has its own detector)",
			agg: OrgSecurityAggregate{
				OrgID:              secOrgID,
				McpServerNames:     []string{"gh-prod"},
				HookCoverageByRepo: map[string]bool{"repoA": false},
			},
			wantFinding: false,
		},
		{
			name: "no security flags does not fire (permission inventory is its own per-repo detector)",
			agg: OrgSecurityAggregate{
				OrgID:              secOrgID,
				HookCoverageByRepo: map[string]bool{"repoA": false, "repoC": false},
			},
			wantFinding: false,
		},
		{
			name: "security flags with multiple uncovered repos names the sorted set",
			agg: OrgSecurityAggregate{
				OrgID:              secOrgID,
				SecurityFlagCounts: map[string]int{"api-key-leak": 2},
				HookCoverageByRepo: map[string]bool{"repoA": false, "repoC": false, "repoB": true},
			},
			wantFinding:  true,
			wantUncoverd: "repoA, repoC",
		},
		{
			name: "resolved repo names are surfaced (sorted), not raw ids",
			agg: OrgSecurityAggregate{
				OrgID:              secOrgID,
				SecurityFlagCounts: map[string]int{"api-key-leak": 2},
				HookCoverageByRepo: map[string]bool{"repoA": false, "repoB": false},
				RepoNamesByID:      map[string]string{"repoA": "acme/web", "repoB": "acme/api"},
			},
			wantFinding: true,
			// Sorted by the resolved NAME, not the id order: acme/api (repoB) before acme/web (repoA).
			wantUncoverd: "acme/api, acme/web",
		},
		{
			name: "an unresolved repo id falls back to the id, mixed with resolved names",
			agg: OrgSecurityAggregate{
				OrgID:              secOrgID,
				SecurityFlagCounts: map[string]int{"api-key-leak": 2},
				HookCoverageByRepo: map[string]bool{"repoA": false, "repoZ": false},
				RepoNamesByID:      map[string]string{"repoA": "acme/api"},
			},
			wantFinding:  true,
			wantUncoverd: "acme/api, repoZ",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			findings := OrgCoverageGapFindings(tt.agg, secExecID)
			if !tt.wantFinding {
				require.Empty(t, findings)
				return
			}
			require.Len(t, findings, 1)
			finding := findings[0]
			assertSecurityFinding(t, finding)
			require.Equal(t, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_ORG, finding.Scope)
			require.Empty(t, finding.ScopeValue)
			require.Equal(t, promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK, finding.ArtifactType)
			require.Nil(t, finding.HarnessArtifactID)
			require.Equal(t, int32(30), finding.Priority) // priorityCritical
			require.Contains(t, finding.Slug, ":security:prompt-coverage-gap:")
			require.Equal(t, tt.wantUncoverd, finding.Evidence.GetReferencePoint())
		})
	}
}

// permissionsBodyWithAllow marshals a committed PERMISSIONS body carrying the given allow list.
func permissionsBodyWithAllow(t *testing.T, allow []string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"permissions": map[string]any{"allow": allow, "deny": []string{}},
	})
	require.NoError(t, err)
	return string(body)
}

// TestSecurityPermissionFindings_GrantCapAndDedup: over-broad allow grants are deduped to one finding
// per distinct grant and capped at maxSecurityGrantFindings, so a crafted settings.json cannot fan
// out into an unbounded finding set.
func TestSecurityPermissionFindings_GrantCapAndDedup(t *testing.T) {
	t.Parallel()
	const artifactID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

	t.Run("distinct over-broad grants beyond the cap yield exactly the cap", func(t *testing.T) {
		t.Parallel()
		grants := make([]string, 0, maxSecurityGrantFindings+11)
		for i := 0; i < maxSecurityGrantFindings+11; i++ {
			// A distinct key with a wildcard value is over-broad (keyed-wildcard), so each grant is a
			// distinct over-broad entry.
			grants = append(grants, fmt.Sprintf("WebFetch(k%d:*)", i))
		}
		aggregate := harnessImproveAggregate{
			committed: []*model.HarnessArtifact{permissionsArtifact(artifactID)},
			bodies:    map[string]string{artifactID: permissionsBodyWithAllow(t, grants)},
		}
		findings := securityPermissionFindings(context.Background(), secOrgID, secRepoID, secExecID, aggregate)
		require.Len(t, findings, maxSecurityGrantFindings)
	})

	t.Run("the same over-broad grant repeated collapses to one finding", func(t *testing.T) {
		t.Parallel()
		aggregate := harnessImproveAggregate{
			committed: []*model.HarnessArtifact{permissionsArtifact(artifactID)},
			bodies: map[string]string{artifactID: permissionsBodyWithAllow(t,
				[]string{"Bash", "Bash", "Bash", "Bash", "Bash"})},
		}
		findings := securityPermissionFindings(context.Background(), secOrgID, secRepoID, secExecID, aggregate)
		require.Len(t, findings, 1)
	})
}

// TestSecurityPermissionFindings_DefaultMode: permissions.defaultMode is flagged independently of the
// allow list — bypassPermissions (CRITICAL) and acceptEdits (MEDIUM) fire, default/plan/empty do not,
// and a weakening mode coexists with its own over-broad grants.
func TestSecurityPermissionFindings_DefaultMode(t *testing.T) {
	t.Parallel()
	const artifactID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	tests := []struct {
		name         string
		body         string
		wantFindings int
		wantPriority int32  // asserted when exactly one finding is expected
		wantRef      string // asserted when exactly one finding is expected
	}{
		{
			name:         "bypassPermissions fires one CRITICAL",
			body:         `{"permissions":{"defaultMode":"bypassPermissions"}}`,
			wantFindings: 1,
			wantPriority: 30, // priorityCritical
			wantRef:      "bypassPermissions",
		},
		{
			name:         "bypassPermissions match is case-insensitive",
			body:         `{"permissions":{"defaultMode":"BypassPermissions"}}`,
			wantFindings: 1,
			wantPriority: 30,
			wantRef:      "BypassPermissions",
		},
		{
			name:         "acceptEdits fires one MEDIUM",
			body:         `{"permissions":{"defaultMode":"acceptEdits"}}`,
			wantFindings: 1,
			wantPriority: 10, // priorityMedium
			wantRef:      "acceptEdits",
		},
		{name: "default mode fires nothing", body: `{"permissions":{"defaultMode":"default"}}`, wantFindings: 0},
		{name: "plan mode fires nothing", body: `{"permissions":{"defaultMode":"plan"}}`, wantFindings: 0},
		{name: "empty mode fires nothing", body: `{"permissions":{"defaultMode":""}}`, wantFindings: 0},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			aggregate := harnessImproveAggregate{
				committed: []*model.HarnessArtifact{permissionsArtifact(artifactID)},
				bodies:    map[string]string{artifactID: tt.body},
			}
			findings := securityPermissionFindings(context.Background(), secOrgID, secRepoID, secExecID, aggregate)
			require.Len(t, findings, tt.wantFindings)
			if tt.wantFindings != 1 {
				return
			}
			finding := findings[0]
			assertSecurityFinding(t, finding)
			require.Equal(t, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, finding.Scope)
			require.Equal(t, secRepoID, finding.ScopeValue)
			require.Equal(t, promptlibraryv1.ArtifactType_ARTIFACT_TYPE_PERMISSIONS, finding.ArtifactType)
			require.NotNil(t, finding.HarnessArtifactID)
			require.Equal(t, tt.wantPriority, finding.Priority)
			require.Contains(t, finding.Slug, ":security:permission-default-mode:")
			require.Equal(t, tt.wantRef, finding.Evidence.GetReferencePoint())
		})
	}
}

// TestSecurityPermissionFindings_DefaultModeAndGrantsCoexist: a weakening defaultMode and an over-broad
// grant on the same body each yield their own finding.
func TestSecurityPermissionFindings_DefaultModeAndGrantsCoexist(t *testing.T) {
	t.Parallel()
	const artifactID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	aggregate := harnessImproveAggregate{
		committed: []*model.HarnessArtifact{permissionsArtifact(artifactID)},
		bodies:    map[string]string{artifactID: `{"permissions":{"defaultMode":"bypassPermissions","allow":["Bash"],"deny":[]}}`},
	}
	findings := securityPermissionFindings(context.Background(), secOrgID, secRepoID, secExecID, aggregate)
	require.Len(t, findings, 2)
	var haveDefaultMode, haveGrant bool
	for _, finding := range findings {
		assertSecurityFinding(t, finding)
		switch {
		case strings.Contains(finding.Slug, ":security:permission-default-mode:"):
			haveDefaultMode = true
			require.Equal(t, int32(30), finding.Priority) // priorityCritical
		case strings.Contains(finding.Slug, ":security:permission-grant:"):
			haveGrant = true
			require.Equal(t, int32(20), finding.Priority) // priorityHigh
		default:
			t.Fatalf("unexpected finding slug %q", finding.Slug)
		}
	}
	require.True(t, haveDefaultMode, "expected a permission-default-mode finding")
	require.True(t, haveGrant, "expected an over-broad grant finding")
}

// TestIsProdPointedServer: prod-pointed detection is segment-exact, so substring false-positives like
// nonproduction-mcp do not fire while db-prod / db-production do.
func TestIsProdPointedServer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want bool
	}{
		{name: "nonproduction-mcp", want: false},
		{name: "reproduction-service", want: false},
		{name: "db-prod", want: true},
		{name: "db-production", want: true},
		{name: "gh-staging", want: false},
		{name: "prd-gateway", want: true},
		{name: "prod_db", want: true},
		{name: "gh", want: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, isProdPointedServer(tt.name))
		})
	}
}
