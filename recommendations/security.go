// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"go.uber.org/zap"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/logger"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// Security is the deterministic (content-only, no LLM) lens over an org's committed permission and
// MCP surface. It splits by grain: the
// permission-inventory sub-detector is PER-REPO (PROJECT scope) and wired into detectAndGrade, while
// the MCP-sprawl / coverage-gap sub-detectors are ORG-scoped — pure functions over an org-aggregate
// contract that a later org-relational scan slice populates and runs ONCE per org (wiring them
// per-repo would duplicate one org finding across every repo scan). Every finding carries dimension
// SECURITY, confidence 1.0 (deterministic), surface_state VISIBLE, recommendation_source DETECTED.
//
// The marker sets and heuristics below are engineer-chosen calibration seeds (no in-repo reference
// implementation): they are live and drive real findings, tuned for precision over recall — an
// ambiguous grant or name is never flagged. Re-derive them against real org data before they are
// trusted (and likely home them in the config package).

// Security finding families — the sub-detector that fired, threaded into the slug and used to pick
// the human title. Stable identifiers, not user copy.
const (
	securityFamilyPermissionGrant       = "permission-grant"
	securityFamilyPermissionDefaultMode = "permission-default-mode"
	securityFamilyMcpProdPointed        = "mcp-prod-pointed"
	securityFamilyMcpSprawl             = "mcp-sprawl"
	securityFamilyCoverageGap           = "prompt-coverage-gap"
)

// maxSecurityGrantFindings caps how many distinct over-broad allow-grant findings ONE PERMISSIONS
// body contributes, mirroring the package's other per-file bounds (MaxSettingsChildren,
// maxRepoPathsEntries): a crafted settings.json with thousands of over-broad grants must not fan out
// into an unbounded row set that duplicates near-identical findings or blows past Postgres's bind
// limit and fails the whole persist batch. Excess distinct grants are dropped (in sorted order, so
// the retained subset is deterministic), never an error.
const maxSecurityGrantFindings = 50

// The permission-inventory lens reports two distinct `allow`-grant categories, both HIGH but with
// different firing rules (spec harness-improve-detectors "mutating grants, unbounded-exec
// entries"). Keys are lower-cased for a case-insensitive match against the grant's tool token.
// PROVISIONAL seed.
//
// securityMutatingAllowTools are file-write tools: ANY committed grant of one is review-worthy
// regardless of an argument constraint, since even `Write(src/**)` hands the agent unattended write
// access to a whole subtree.
var securityMutatingAllowTools = map[string]struct{}{
	"write":     {},
	"edit":      {},
	"multiedit": {},
}

// securityUnboundedAllowTools are exec/network tools reported ONLY when the grant is unbounded (a
// bare tool or a wildcard arg): a bounded `Bash(git status:*)` / `WebFetch(domain:example.com)` is a
// deliberately narrowed capability and not flagged.
var securityUnboundedAllowTools = map[string]struct{}{
	"bash":      {},
	"webfetch":  {},
	"websearch": {},
}

// securityExecStyleAllowTools are the exec-style entries in securityUnboundedAllowTools whose Claude
// Code bounded-grant syntax is `Bash(<prefix>:*)` — a concrete command prefix with wildcarded args.
// Such a grant is unbounded only when the PREFIX before the first `:` is empty or wildcard-only
// (`Bash`, `Bash(*)`, `Bash(:*)`); a concrete prefix (`Bash(git status:*)`) is bounded. The remaining
// securityUnboundedAllowTools (WebFetch/WebSearch) are domain-grant tools whose `key:value` arg is
// unbounded when the VALUE after the last `:` is wildcard. See isUnboundedArg.
var securityExecStyleAllowTools = map[string]struct{}{
	"bash": {},
}

// securityMcpProdMarkers are the case-insensitive name SEGMENTS that read as a server pointing at a
// production backend. A server is prod-pointed iff one of its `-`/`_`/`.`-delimited segments is
// exactly one of these (see isProdPointedServer). Segment-exact, not substring, so `nonproduction-mcp`
// / `reproduction-service` do NOT match while `db-prod` does — and a lone `db-prod` is caught here
// even though the sprawl lens also treats `prod` as an env variant.
var securityMcpProdMarkers = map[string]struct{}{
	"prod":       {},
	"prd":        {},
	"production": {},
}

// securityMcpEnvSuffixes is the CANONICAL closed set of environment suffixes a server name is grouped
// across for the sprawl lens (`gh-prod`/`gh-dev` → base `gh`). ONLY a trailing segment in this set
// reads as an env variant (see isEnvVariantSuffix) — an arbitrary `-<word>` suffix does not, so
// distinct servers like `jira-api`/`jira-webhooks` are never mistaken for one sprawled base.
var securityMcpEnvSuffixes = map[string]struct{}{
	"dev":        {},
	"staging":    {},
	"stage":      {},
	"prod":       {},
	"production": {},
	"test":       {},
	"qa":         {},
	"local":      {},
}

// bodyRequiredForSecurity reports whether an artifact's body is input to the per-repo Security
// permission-inventory lens — the committed PERMISSIONS carrier (settings.json). Hooks are matched by
// Name/presence and carry no body, so they are NOT included here.
func bodyRequiredForSecurity(a *model.HarnessArtifact) bool {
	return a.Type == commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_PERMISSIONS
}

// permissionsSettings is the local parse shape of a committed PERMISSIONS body — the same
// settings.json carrier the desktop/VCS ingest fans out. Only the permissions block is read here.
type permissionsSettings struct {
	Permissions permissionBlock `json:"permissions"`
}

type permissionBlock struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
	Ask   []string `json:"ask"`
	// DefaultMode is the settings-wide permission posture. `bypassPermissions` disables ALL prompts
	// (strictly worse than any single over-broad grant); `acceptEdits` auto-approves edits;
	// `default`/`plan`/empty keep prompting.
	DefaultMode string `json:"defaultMode"`
}

// securityPermissionFindings runs the per-repo permission-inventory lens over the aggregate's
// committed PERMISSIONS artifacts and their fetched bodies. Per PERMISSIONS body it emits: a
// permission-default-mode finding when `permissions.defaultMode` weakens the posture
// (`bypassPermissions` → CRITICAL, `acceptEdits` → MEDIUM; at most one, uncapped); and one HIGH
// finding per DISTINCT over-broad `allow` grant, capped at maxSecurityGrantFindings so a crafted
// settings.json cannot fan out unboundedly. It is pure over the aggregate (no I/O) so the detector
// logic is unit-testable. A PERMISSIONS artifact contributes only when its body was fetched (present
// in aggregate.bodies); a malformed body degrades to no findings (never an error). The deny-rule
// count is intentionally NOT surfaced on the grant finding: deny rules narrow access, so reporting
// the count in an over-broad-access finding read as contradictory.
func securityPermissionFindings(ctx context.Context, orgID, repoID, taskExecutionID string, aggregate harnessImproveAggregate) []*model.PromptLibraryItem {
	var findings []*model.PromptLibraryItem
	for _, artifact := range aggregate.committed {
		if artifact.Type != commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_PERMISSIONS {
			continue
		}
		body, ok := aggregate.bodies[artifact.ID]
		if !ok {
			continue
		}
		var parsed permissionsSettings
		if err := json.Unmarshal([]byte(body), &parsed); err != nil {
			// A committed PERMISSIONS body should be valid settings.json (the fan-out only mints the
			// artifact from a parseable body), so a parse failure here is anomalous — surface it and
			// derive no grants for this artifact rather than fail the scan.
			logger.WarnWithCtx(ctx, "harness-improve: skipping unparseable PERMISSIONS body",
				zap.String(logger.ORG_ID, orgID), zap.String(logger.REPO_ID, repoID),
				zap.String("artifact_id", artifact.ID), zap.Error(err))
			continue
		}
		// defaultMode is a settings-wide posture that overrides the per-grant checks, so flag it FIRST
		// and independently of the allow-grant loop (at most one per body → uncapped).
		if mode := strings.TrimSpace(parsed.Permissions.DefaultMode); mode != "" {
			switch {
			case strings.EqualFold(mode, "bypassPermissions"):
				findings = append(findings, newProjectFinding(orgID, repoID, taskExecutionID, artifact,
					promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_SECURITY, priorityCritical,
					securitySlug(repoID, securityFamilyPermissionDefaultMode, artifact.ID, mode),
					"Permissions default mode bypasses all prompts", mode))
			case strings.EqualFold(mode, "acceptEdits"):
				findings = append(findings, newProjectFinding(orgID, repoID, taskExecutionID, artifact,
					promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_SECURITY, priorityMedium,
					securitySlug(repoID, securityFamilyPermissionDefaultMode, artifact.ID, mode),
					"Permissions default mode auto-approves edits", mode))
			}
		}

		// Dedup the reportable grants so one distinct grant → one finding (a crafted body repeating the
		// same grant collapses to one), keyed to the category label so the emit loop names WHY it fired,
		// then sort for a deterministic cap and emit order.
		reportable := map[string]string{}
		for _, grant := range parsed.Permissions.Allow {
			category := allowGrantCategory(grant)
			if category == "" {
				continue
			}
			reportable[strings.TrimSpace(grant)] = category
		}
		distinct := make([]string, 0, len(reportable))
		for grant := range reportable {
			distinct = append(distinct, grant)
		}
		sort.Strings(distinct)
		if len(distinct) > maxSecurityGrantFindings {
			logger.WarnWithCtx(ctx, "harness-improve: settings permission grants exceeded the per-file cap",
				zap.String(logger.ORG_ID, orgID), zap.String(logger.REPO_ID, repoID),
				zap.String("artifact_id", artifact.ID), zap.Int("cap", maxSecurityGrantFindings),
				zap.Int("total", len(distinct)))
			distinct = distinct[:maxSecurityGrantFindings]
		}
		for _, grant := range distinct {
			referencePoint := fmt.Sprintf("allow %s (%s)", grant, reportable[grant])
			findings = append(findings, newProjectFinding(orgID, repoID, taskExecutionID, artifact,
				promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_SECURITY, priorityHigh,
				securitySlug(repoID, securityFamilyPermissionGrant, artifact.ID, grant),
				"Over-broad permission grant", referencePoint))
		}
	}
	return findings
}

// allowGrantCategory classifies an `allow`-list entry into the reportable category it belongs to,
// returning "" when the grant is not reportable. A securityMutatingAllowTools grant (`Write`,
// `Edit(config.json)`) is always "mutating grant" — the argument constraint does not matter. A
// securityUnboundedAllowTools grant is "unbounded" only when it has no argument constraint (`Bash`)
// or a pure/keyed wildcard (`Bash(*)`, `WebFetch(domain:*)`); a concrete constraint
// (`Bash(git status)`, `WebFetch(domain:example.com)`) is bounded and never flagged. A tool in
// neither set (`Read`) is never reported. The label is used verbatim in the finding's referencePoint.
func allowGrantCategory(entry string) string {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return ""
	}
	tool := entry
	arg := ""
	hasArg := false
	if open := strings.Index(entry, "("); open >= 0 && strings.HasSuffix(entry, ")") {
		tool = entry[:open]
		arg = entry[open+1 : len(entry)-1]
		hasArg = true
	}
	toolKey := strings.ToLower(strings.TrimSpace(tool))
	if _, ok := securityMutatingAllowTools[toolKey]; ok {
		return "mutating grant"
	}
	if _, ok := securityUnboundedAllowTools[toolKey]; ok {
		if !hasArg || isUnboundedArg(toolKey, arg) {
			return "unbounded"
		}
	}
	return ""
}

// isUnboundedArg reports whether a grant's argument constraint leaves the grant unbounded, applying
// the grammar of the grant's TOOL so a bounded exec grant is not misread as unbounded. For an
// exec-style tool (Bash) the bounded-grant syntax is `Bash(<prefix>:*)` — a concrete command prefix
// with wildcarded args — so the grant is unbounded only when the PREFIX before the first `:` is empty
// or wildcard-only (`Bash(*)`, `Bash(:*)`, `Bash()`); a concrete prefix (`Bash(git status:*)`) is a
// deliberately narrowed capability and bounded. For a domain-grant tool (WebFetch/WebSearch) the arg
// is `key:value` and unbounded when the VALUE after the last `:` is wildcard-only (`WebFetch(domain:*)`);
// a concrete value (`WebFetch(domain:example.com)`) is bounded.
func isUnboundedArg(toolKey, arg string) bool {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return true
	}
	if _, exec := securityExecStyleAllowTools[toolKey]; exec {
		prefix := arg
		if idx := strings.Index(arg, ":"); idx >= 0 {
			prefix = arg[:idx]
		}
		prefix = strings.TrimSpace(prefix)
		return prefix == "" || strings.Trim(prefix, "*") == ""
	}
	return isWildcardArg(arg)
}

// isWildcardArg reports whether a DOMAIN-grant arg (WebFetch/WebSearch) leaves the grant unbounded:
// the WHOLE arg is empty or only wildcards/separators (`*`, `*:*`), OR — for a KEYED arg (contains
// `:`) — the value after the last `:` is wildcard-only (`WebFetch(domain:*)` constrains nothing). A
// concrete value after the key (`WebFetch(domain:example.com)`) is bounded. Exec-style tools (Bash)
// use isUnboundedArg's prefix rule instead — the value after `:` is meant to be wildcarded there.
func isWildcardArg(arg string) bool {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return true
	}
	if strings.NewReplacer("*", "", ":", "", " ", "").Replace(arg) == "" {
		return true
	}
	if idx := strings.LastIndex(arg, ":"); idx >= 0 {
		value := strings.TrimSpace(arg[idx+1:])
		return value == "" || strings.Trim(value, "*") == ""
	}
	return false
}

// securitySlug builds the stable per-repo Security finding slug "<repoID>:security:<family>:<hash>"
// where hash is the first 12 hex of sha256(artifactID + ":" + detail), so each distinct subject (an
// over-broad grant, a default-mode value) on one carrier gets its own slug and re-runs converge.
func securitySlug(repoID, family, artifactID, detail string) string {
	sum := sha256.Sum256([]byte(artifactID + ":" + detail))
	shortHash := hex.EncodeToString(sum[:])[:12]
	return fmt.Sprintf("%s:security:%s:%s", repoID, family, shortHash)
}

// OrgSecurityAggregate is the org-wide input to the ORG-scoped Security detectors (OrgMcpFindings,
// OrgCoverageGapFindings). It is populated ONCE per org from org-wide reads — MCP usage, open
// security flags, per-repo hook coverage, and whether the org commits any PERMISSIONS — and the pure
// detectors fold over it once per org.
type OrgSecurityAggregate struct {
	OrgID string
	// McpServerNames is the distinct set of MCP server names used across the org.
	McpServerNames []string
	// SecurityFlagCounts is the org-wide count of open security_flags, keyed by flag_type.
	SecurityFlagCounts map[string]int
	// HookCoverageByRepo maps a repo id to whether that repo has a UserPromptSubmit hook.
	HookCoverageByRepo map[string]bool
	// RepoNamesByID maps a repo id (the keys of HookCoverageByRepo) to its human name (full_name,
	// e.g. owner/repo), so the coverage-gap finding names the uncovered repos instead of leaking raw
	// UUIDs. A repo whose id does not resolve is absent; OrgCoverageGapFindings falls back to the id.
	RepoNamesByID map[string]string
}

// OrgMcpFindings is the ORG-scoped MCP lens (PURE — exported so the org-relational scan slice can run
// it once per org, threading its org task's taskExecutionID). It emits a CRITICAL finding per
// prod-pointed server name and a MEDIUM finding per sprawled base (a base with ≥2 distinct env
// variants). Deterministic order; server names are deduped first.
func OrgMcpFindings(agg OrgSecurityAggregate, taskExecutionID string) []*model.PromptLibraryItem {
	names := dedupSortedStrings(agg.McpServerNames)

	var findings []*model.PromptLibraryItem

	// prod-pointed (CRITICAL): a server name whose text points at production.
	for _, name := range names {
		if !isProdPointedServer(name) {
			continue
		}
		findings = append(findings, orgSecurityFinding(agg.OrgID, taskExecutionID,
			promptlibraryv1.ArtifactType_ARTIFACT_TYPE_MCP_SERVER, nil, priorityCritical,
			orgSecuritySlug(securityFamilyMcpProdPointed, name),
			"MCP server points at production", name))
	}

	// sprawl (MEDIUM): a base shared by ≥2 distinct env variants (near-duplicate per-env servers).
	envsByBase := map[string]map[string]struct{}{}
	for _, name := range names {
		base, env, hasEnv := mcpBaseAndEnv(name)
		if !hasEnv {
			continue
		}
		if envsByBase[base] == nil {
			envsByBase[base] = map[string]struct{}{}
		}
		envsByBase[base][env] = struct{}{}
	}
	bases := make([]string, 0, len(envsByBase))
	for base := range envsByBase {
		bases = append(bases, base)
	}
	sort.Strings(bases)
	sprawledBases := map[string]struct{}{}
	for _, base := range bases {
		envs := sortedPathKeys(envsByBase[base])
		if len(envs) < 2 {
			continue
		}
		sprawledBases[base] = struct{}{}
		referencePoint := fmt.Sprintf("base %s across %d envs: %s", base, len(envs), strings.Join(envs, ", "))
		findings = append(findings, orgSecurityFinding(agg.OrgID, taskExecutionID,
			promptlibraryv1.ArtifactType_ARTIFACT_TYPE_MCP_SERVER, nil, priorityMedium,
			orgSecuritySlug(securityFamilyMcpSprawl, base),
			"Near-duplicate MCP servers across environments", referencePoint))
	}

	// bare-alias near-duplicate (MEDIUM): a bare server name (no `-`, so no recognized env structure)
	// that is a full leading segment of another distinct name — `slack` + `slack-mcp-server`. The
	// {base}-{env} lens misses these because the suffix (`mcp-server`) is not a canonical env. The
	// `base+"-"` prefix test keeps this conservative: it requires `base` to be a WHOLE leading segment,
	// so `gh` does not alias `github-mcp`. One finding per bare base; a base the env-variant lens
	// already flagged is skipped so its shared mcp-sprawl slug is not double-emitted.
	for _, base := range names {
		if strings.Contains(base, "-") {
			continue
		}
		if _, sprawled := sprawledBases[base]; sprawled {
			continue
		}
		var aliases []string
		for _, other := range names {
			if strings.HasPrefix(other, base+"-") {
				aliases = append(aliases, other)
			}
		}
		if len(aliases) == 0 {
			continue
		}
		referencePoint := fmt.Sprintf("bare %s aliased by: %s", base, strings.Join(aliases, ", "))
		findings = append(findings, orgSecurityFinding(agg.OrgID, taskExecutionID,
			promptlibraryv1.ArtifactType_ARTIFACT_TYPE_MCP_SERVER, nil, priorityMedium,
			orgSecuritySlug(securityFamilyMcpSprawl, base),
			"Near-duplicate MCP servers (bare name and suffixed alias)", referencePoint))
	}
	return findings
}

// OrgCoverageGapFindings is the ORG-scoped prompt-coverage-gap lens (PURE — exported so the
// org-relational scan slice runs it once per org). It fires ONE CRITICAL finding when the org has
// OPEN security_flags of some kind AND at least one repo lacks a UserPromptSubmit hook — the org has
// evidence of prompt-time security incidents but not the guard covering every repo (the
// detector spec: "security_flags of a kind exist and no UserPromptSubmit hook").
// MCP usage and committed PERMISSIONS
// are the subject of their OWN detectors (OrgMcpFindings, the per-repo permission inventory) and do
// NOT on their own constitute the surface this lens guards, so they never trigger it. The finding's
// evidence (reference point) carries the sorted uncovered-repo NAMES (owner/repo, resolved via
// RepoNamesByID; the id is the fallback for an unresolved name); it closes (no emission) only when ALL
// repos are covered (the detector spec "all eligible repos covered, not any"). An empty
// HookCoverageByRepo means no
// repos to evaluate → no finding.
func OrgCoverageGapFindings(agg OrgSecurityAggregate, taskExecutionID string) []*model.PromptLibraryItem {
	if len(agg.HookCoverageByRepo) == 0 {
		return nil
	}
	if !hasPositiveCount(agg.SecurityFlagCounts) {
		return nil
	}
	// Name each uncovered repo (owner/repo) rather than its raw UUID — the finding's summary
	// interpolates this list, and a UUID means nothing to an admin. An id that did not resolve to a
	// name falls back to the id so the repo is never silently dropped from the list.
	uncovered := make([]string, 0, len(agg.HookCoverageByRepo))
	for repoID, covered := range agg.HookCoverageByRepo {
		if covered {
			continue
		}
		if name, ok := agg.RepoNamesByID[repoID]; ok && name != "" {
			uncovered = append(uncovered, name)
		} else {
			uncovered = append(uncovered, repoID)
		}
	}
	if len(uncovered) == 0 {
		return nil
	}
	sort.Strings(uncovered)
	referencePoint := strings.Join(uncovered, ", ")
	return []*model.PromptLibraryItem{
		orgSecurityFinding(agg.OrgID, taskExecutionID,
			promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK, nil, priorityCritical,
			orgSecuritySlug(securityFamilyCoverageGap, agg.OrgID),
			"Repos missing a prompt-screening hook", referencePoint),
	}
}

// orgSecurityFinding builds one ORG-scoped Security finding via the shared newDeterministicFinding
// builder, stamping dimension SECURITY, Scope=ORG, and ScopeValue="" (the model convention is EMPTY
// for org scope, not orgID). harnessArtifactID is nil for a relational finding with no single anchor
// artifact.
func orgSecurityFinding(orgID, taskExecutionID string, artifactType promptlibraryv1.ArtifactType,
	harnessArtifactID *string, priority int32, slug, title, referencePoint string,
) *model.PromptLibraryItem {
	return newDeterministicFinding(orgID, taskExecutionID,
		promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_SECURITY,
		promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_ORG, "", artifactType,
		harnessArtifactID, priority, slug, title, referencePoint, "", "")
}

// orgSecuritySlug builds a stable ORG Security finding slug "org:security:<family>:<hash>" where hash
// is the first 12 hex of sha256(family + ":" + detail). ORG findings carry no repo prefix (scope is
// org); keying on the family + its subject (server name, base, or org id) keeps re-runs on the same
// slug so a finding dedups and converges.
func orgSecuritySlug(family, detail string) string {
	sum := sha256.Sum256([]byte(family + ":" + detail))
	shortHash := hex.EncodeToString(sum[:])[:12]
	return fmt.Sprintf("org:security:%s:%s", family, shortHash)
}

// isProdPointedServer reports whether any of a server name's `-`/`_`/`.`-delimited segments is
// exactly (case-insensitive) a securityMcpProdMarkers marker. Segment-exact, not substring, so
// `nonproduction-mcp` / `reproduction-service` do NOT fire while `db-prod` / `db-production` do.
func isProdPointedServer(name string) bool {
	for _, segment := range splitNameSegments(name) {
		if _, ok := securityMcpProdMarkers[strings.ToLower(segment)]; ok {
			return true
		}
	}
	return false
}

// splitNameSegments splits a server name into its `-`/`_`/`.`-delimited segments (empty segments
// dropped) — the unit the prod-marker match compares against.
func splitNameSegments(name string) []string {
	return strings.FieldsFunc(name, func(r rune) bool {
		return r == '-' || r == '_' || r == '.'
	})
}

// mcpBaseAndEnv splits a server name into its base and trailing env variant by stripping a trailing
// env suffix after the last `-`/`_`/`.` delimiter — the SAME delimiter set splitNameSegments and
// isProdPointedServer use, so `db_prod`/`db_dev` group under base `db` just as `gh-prod`/`gh-dev` do.
// hasEnv is false (base is the whole name) when there is no trailing delimiter or the suffix is not
// env-variant-shaped.
func mcpBaseAndEnv(name string) (base, env string, hasEnv bool) {
	idx := strings.LastIndexFunc(name, func(r rune) bool {
		return r == '-' || r == '_' || r == '.'
	})
	if idx <= 0 || idx >= len(name)-1 {
		return name, "", false
	}
	suffix := name[idx+1:]
	if !isEnvVariantSuffix(suffix) {
		return name, "", false
	}
	return name[:idx], suffix, true
}

// isEnvVariantSuffix reports whether suffix is one of the canonical env markers in
// securityMcpEnvSuffixes (case-insensitive) — the closed set the sprawl lens groups across. An
// arbitrary suffix is NOT a variant, so distinct servers like `jira-api`/`jira-webhooks` never read
// as one sprawled base.
func isEnvVariantSuffix(suffix string) bool {
	_, ok := securityMcpEnvSuffixes[strings.ToLower(suffix)]
	return ok
}

// hasPositiveCount reports whether any kind in the org-wide flag-count map has a positive count — the
// "org has security telemetry" signal for the coverage-gap lens.
func hasPositiveCount(counts map[string]int) bool {
	for _, count := range counts {
		if count > 0 {
			return true
		}
	}
	return false
}

// dedupSortedStrings returns the distinct values of in, sorted ascending — a deterministic slice so
// the MCP lenses emit findings in a stable order across runs.
func dedupSortedStrings(in []string) []string {
	set := make(map[string]struct{}, len(in))
	for _, value := range in {
		set[value] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
