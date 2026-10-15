// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/dao"
	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/logger"
)

// The harness recommendation quality gate.
// After the single derive LLM call, Grade runs deterministic Go validators over
// every derived artifact and stamps a surface_state — VISIBLE (passed all blocking
// checks) or HIDDEN_FAILED_EVAL (failed one). It never drops artifacts; the engine
// persists all of them and the dashboard list query filters to VISIBLE. There is no
// LLM-as-judge: all judging is the validators below (single LLM call in the flow is
// derive). On a securityOnly run (below-threshold org with security incidents) the
// gate relaxes the path checks — a security-governance rule legitimately cites no
// repo path — but grades each artifact against the org's actual security_flags
// evidence: only an artifact addressing a real detected incident survives, so the
// general/boilerplate artifacts the underpowered context still produced are hidden
//.

const (
	// bodyMinLength is the minimum artifact body length; shorter bodies are too
	// thin to be useful and are hidden. Mirrors the tetriz-py DerivedArtifact
	// body min_length so the Go gate re-asserts the same floor.
	bodyMinLength = 20
	// maxSlugLen bounds the stored slug to the prompt_library.slug column width
	// (VARCHAR(255)). normalizeSlug truncates to this so a pathologically long LLM
	// slug can never overflow the column and fail the whole batch insert — every
	// graded artifact persists (only security_scope is dropped), so even a
	// gate-hidden row's slug must fit. isWellFormedSlug then hides any slug at this
	// ceiling, so a truncated/over-long slug is never surfaced as a visible
	// artifact. A real slug is a few words, so this never trims a legitimate one.
	maxSlugLen = 255
	// artifactScopeProject is the LLM-facing scope string for a project artifact — the only
	// scope this ship emits (parseArtifactScope maps it to ARTIFACT_SCOPE_PROJECT).
	artifactScopeProject = "project"
)

// Validator names — the FailedValidator field and the quality-gate-failure metric
// label. Kept as constants so the metric cardinality is a fixed, known set.
const (
	validatorBodyMinLength = "body_min_length"
	validatorSlugMissing   = "slug_missing"
	validatorSlugFormat    = "slug_format"
	validatorDuplicateSlug = "duplicate_slug"
	validatorOrgScope      = "org_scope"
	validatorProjectScope  = "project_scope_value"
	validatorPathsExist    = "paths_exist"
	validatorNotDismissed  = "not_dismissed"
	// validatorCommittedArtifact drops a net-new suggestion whose canonical slug matches an
	// artifact the repo already commits (a VCS harness_artifacts row). It is TRANSIENT — the
	// committed set changes between runs, so a hit is dropped (toModels), never persisted as
	// HIDDEN_FAILED_EVAL; persisting it would keep the slug suppressed by the recent-hidden
	// dedup for the whole window after the repo removes that artifact.
	validatorCommittedArtifact = "committed_artifact"
	// validatorPinnedPreserved drops a re-derived artifact whose (type, slug) an admin has
	// pinned: the pinned row stays on the board past supersession, so persisting this one
	// too would put two rows for the same recommendation in front of every viewer. It is
	// TRANSIENT — the gating state is the pin, which an admin can remove — so a hit is
	// dropped, never persisted as HIDDEN_FAILED_EVAL; persisting it would keep the slug
	// suppressed by the recent-hidden dedup for the whole window after an unpin.
	validatorPinnedPreserved = "pinned_preserved"
	validatorSecurityScope   = "security_scope"
	// validatorTemplateSessionSignal hides a TEMPLATE (only) when the repo's session
	// buckets are below the low/high floors. Templates are the sole artifact type
	// derived from session-quality data (per-task-type + high-vs-low gap);
	// Rules and Skills are VCS-derived (review themes + repo tree), so this never
	// applies to them. It is a TRANSIENT gate, so — like validatorSecurityScope — a
	// hit is NOT persisted (toModels drops it); persisting it would poison the
	// recent-hidden dedup and suppress the template forever once its cohort recovers.
	validatorTemplateSessionSignal = "template_session_signal"
	// validatorHookEvent hides a HOOK whose lifecycle event is empty — a hook with no event
	// can never fire, malformed regardless of the resolved tool, so it is a genuine
	// per-artifact quality failure (persisted HIDDEN, feeds dedup) — distinct from the
	// advisory unknown-for-tool event, which KEEPS the hook and only bumps the drift counter.
	validatorHookEvent = "hook_event"
	// validatorHookEventToolScoped hides a HOOK whose event the RESOLVED tool cannot run. Two
	// cases: a Kiro lifecycle event (PreToolUse/SessionStart) Kiro never executes (its
	// vocabulary is closed); and a cross-tool event on an open-vocab tool — one that
	// provably belongs to a DIFFERENT mapped tool (Claude's SubagentStop on a Cursor-resolved
	// hook), inert where it landed. It is TOOL-DEPENDENT, not a malformation: the same event is
	// valid for another tool, and resolveDominantTool can pick a different tool on a later run.
	// So, like the tool-less hides, it is a TRANSIENT drop — NOT persisted (toModels drops it) —
	// else a later run resolving a tool the event IS valid for would find the slug poisoned in
	// the recent-hidden dedup and suppress a good hook.
	validatorHookEventToolScoped = "hook_event_tool_scoped"
	// validatorHookTool hides a HOOK for which no target tool resolved: a
	// hook has no deployable config path without a detected tool, so it cannot be
	// surfaced. In a securityOnly run ide_breakdown is absent and the tool resolves off
	// the RepoTree alone, so a tool-less security hook is still hidden here.
	validatorHookTool = "hook_tool"
	// validatorHookBody hides a HOOK with no command — the payload Go serializes into the
	// resolved tool's config shape (see synthesizeHookBody). Without a command the synthesized
	// body is a no-op, malformed regardless of tool, so it must not grade VISIBLE.
	validatorHookBody = "hook_body"
	// validatorAgentName hides an AGENT with no agent_name — the frontmatter identity the
	// tool loads the subagent by. Agents are otherwise grounded like rules/skills.
	validatorAgentName = "agent_name"
	// validatorAgentDescription hides an AGENT with no agent_description — every tool's agent
	// format requires a description (it tells the tool WHEN to delegate to the subagent); Go
	// would otherwise emit an empty `description`, which loaders reject or leave undelegatable.
	validatorAgentDescription = "agent_description"
	// validatorAgentTool hides an AGENT for which no target tool resolved,
	// symmetric with validatorHookTool: with no detected tool there is no agent-dir
	// destination to create the file in, so the agent cannot be deployed and must not
	// grade VISIBLE.
	validatorAgentTool = "agent_tool"
	// validatorHookSessionSignal / validatorAgentSessionSignal hide a HOOK / AGENT when the
	// repo's session-quality signal is short — the same low/high session-bucket floors that
	// gate TEMPLATE visibility (validatorTemplateSessionSignal), applied per-artifact to the
	// new types. Like the template signal these are TRANSIENT scope drops — NOT persisted
	// (toModels drops them) — so a hook/agent hidden for low session demand re-derives
	// cleanly once the buckets fill, never suppressed forever by the recent-hidden dedup.
	// Per-type reasons (not one shared reason) so the failure metric attributes the hide.
	validatorHookSessionSignal  = "hook_session_signal"
	validatorAgentSessionSignal = "agent_session_signal"
	// validatorNewTypesDisabled drops a HOOK/AGENT when the org's hooks-and-agents feature
	// flag is off at derive time (fail-closed). Also a TRANSIENT scope drop (toModels drops
	// it) so flipping the flag on later re-derives the buckets cleanly.
	validatorNewTypesDisabled = "new_types_disabled"
	// validatorTypeCapExceeded drops a HOOK/AGENT beyond the per-run top-N cap
	// (MaxHooksPerRun / MaxAgentsPerRun) after grading, keeping the highest-priority /
	// highest-confidence few. A non-blocking output limit, never a run
	// failure — so it is TRANSIENT (toModels drops it) and does not bump the quality-gate
	// failure metric.
	validatorTypeCapExceeded = "type_cap_exceeded"
)

var qualityGateFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "recommendations_derivation_quality_gate_failures_total",
	Help: "Count of derived artifacts hidden by a blocking quality-gate validator, by validator and artifact type.",
}, []string{"validator", "type"})

var artifactsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "recommendations_derivation_artifacts_total",
	Help: "Count of graded derivation artifacts, by artifact type and surface_state.",
}, []string{"type", "surface_state"})

// hookEventUnknownTotal counts derived HOOKs whose lifecycle event is not on the
// resolved tool's known-event allowlist — a drift signal, NOT a gate failure (the
// hook is kept VISIBLE). It is deliberately SEPARATE from
// qualityGateFailuresTotal: an unknown-for-tool event is expected as upstream tools
// add events over time, so it must not read as a quality regression.
var hookEventUnknownTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "recommendations_derivation_hook_event_unknown_total",
	Help: "Count of derived hooks whose event is unknown for the resolved target tool (advisory drift signal, hook kept visible).",
}, []string{"target_tool"})

// hookEventCrossToolBlockedTotal counts derived HOOKs suppressed because their event is a
// cross-tool mismatch — unknown to the resolved tool but known to a different mapped tool.
// The suppression is a transient tool-scoped drop (validatorHookEventToolScoped), which is
// deliberately exempt from qualityGateFailuresTotal, so without this dedicated counter a
// suppression would be invisible. It is the observability the cross-tool block owes: because
// the per-tool allowlists are non-exhaustive, a rising rate for one tool is the signal that a
// now-valid event of that tool is missing from its allowlist and should be added (which then
// moves it back to the visible/advisory path).
var hookEventCrossToolBlockedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "recommendations_derivation_hook_event_cross_tool_blocked_total",
	Help: "Count of derived hooks suppressed as a cross-tool event mismatch (event belongs to a different mapped tool).",
}, []string{"target_tool"})

// registerValidatorMetrics registers the quality-gate metrics on reg. Opt-in (no
// import-time side effect): the package's RegisterMetrics calls it once at startup.
func registerValidatorMetrics(reg prometheus.Registerer) {
	reg.MustRegister(qualityGateFailuresTotal, artifactsTotal, hookEventUnknownTotal, hookEventCrossToolBlockedTotal)
	// Zero-seed both hook-event counters per resolvable target tool so the first real event
	// reads as an increase() on the dashboard: a lazily-created series first appears at 1 with
	// no prior zero sample, so increase() shows nothing until a second event — and hook volumes
	// are capped, so the first event may be the only one and stay invisible.
	// Seeded from toolArtifactTargets (every tool a hook/agent can resolve to), so a tool
	// added to that table is auto-seeded — and Antigravity, which has no hookEventAllowlist
	// entry and so drifts on every event, is not missed.
	for tool := range toolArtifactTargets {
		hookEventUnknownTotal.WithLabelValues(tool.String())
		hookEventCrossToolBlockedTotal.WithLabelValues(tool.String())
	}
}

// kebabSlugPattern is the well-formed slug body: lowercase alphanumerics in
// hyphen-separated groups, no leading/trailing/double hyphen, no spaces or other
// punctuation. Rules/templates must match it whole; a skill must match it after its
// single leading "/". Applied to the already-normalized slug (normalizeSlug has
// lowercased/trimmed and set the skill prefix), so this only rejects shape the
// normalizer doesn't fix (spaces, underscores, dots, empty groups).
var kebabSlugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// urlPattern strips http(s) URLs from an artifact body before path extraction so a
// cited link (e.g. https://github.com/org/repo/blob/main/x.go) is never mistaken
// for a repo path.
var urlPattern = regexp.MustCompile(`https?://\S+`)

// bodyPathCandidate matches a path-shaped token in body prose. A leading "." is
// allowed so dotfile dirs (.github, .cursor) match whole rather than mangled.
var bodyPathCandidate = regexp.MustCompile(`[A-Za-z0-9_.][A-Za-z0-9_./-]*`)

// citableFileExtensions is the set of file extensions a derived artifact is
// expected to cite — source, config, markup, doc, script. Body-path extraction
// requires a candidate's last segment to end in one of these so prose with a
// dotted last segment but a non-file "extension" (a version like "v2.0", a word
// pair like "input/output.contract") is treated as prose, not a path. The set is
// intentionally broad; a real file with an exotic extension simply isn't
// body-grounded (under-extraction, the safe direction) — it is still grounded via
// cited_paths. Extensions are compared lowercased, without the leading dot.
var citableFileExtensions = map[string]struct{}{
	"go": {}, "mod": {}, "sum": {}, "ts": {}, "tsx": {}, "js": {}, "jsx": {},
	"mjs": {}, "cjs": {}, "py": {}, "java": {}, "kt": {}, "kts": {}, "rb": {},
	"rs": {}, "c": {}, "h": {}, "cpp": {}, "cc": {}, "hpp": {}, "cs": {}, "php": {},
	"swift": {}, "scala": {}, "sql": {}, "sh": {}, "bash": {}, "proto": {},
	"dart": {}, "ex": {}, "exs": {}, "clj": {}, "m": {}, "mm": {}, "md": {},
	"mdx": {}, "yaml": {}, "yml": {}, "json": {}, "jsonc": {}, "toml": {},
	"ini": {}, "cfg": {}, "conf": {}, "xml": {}, "html": {}, "css": {}, "scss": {},
	"sass": {}, "less": {}, "txt": {}, "env": {}, "gradle": {}, "properties": {},
	"tf": {}, "tfvars": {}, "gql": {}, "graphql": {}, "vue": {}, "svelte": {},
	"dockerfile": {}, "gitignore": {}, "cursorrules": {}, "lock": {},
}

// citableExtensionlessFiles are well-known repo files that carry no extension, so
// the heuristic branch can ground a hallucinated reference to one (e.g.
// "backend/Dockerfile" not in the tree) without treating every extensionless token
// (a directory reference like "backend/handlers") as a path. Matched on the
// lowercased basename.
var citableExtensionlessFiles = map[string]struct{}{
	"dockerfile": {}, "makefile": {}, "jenkinsfile": {}, "gemfile": {},
	"rakefile": {}, "procfile": {}, "vagrantfile": {}, "brewfile": {},
	"license": {}, "notice": {}, "readme": {}, "changelog": {}, "authors": {},
	"codeowners": {},
}

// DerivedArtifact mirrors tetriz-py's DeriveOrgArtifactsResponse artifact shape
// (the /generate-content derive result, schema-normalized server-side). ScopeValue
// and TaskType are nullable: the LLM leaves scope_value blank (Go owns the repo id)
// and only templates carry task_type. The gate reads Slug, Title, Body, Scope, and
// CitedPaths; the rest (Priority, EvidenceSummary, AddressesIssues, Confidence,
// ScopeValue, TaskType) are the derive contract the engine slice persists, carried
// through unread here. Slug is the stable one-word identity + dedup key; the gate
// hides an artifact whose slug is empty (validatorSlugMissing).
type DerivedArtifact struct {
	Title      string  `json:"title"`
	Slug       string  `json:"slug"`
	Body       string  `json:"body"`
	Scope      string  `json:"scope"`
	ScopeValue *string `json:"scope_value"`
	TaskType   *string `json:"task_type"`
	// TriggerKeywords are the LLM-derived prompt-routing signals a template carries
	// (templates only; the derive schema nulls them off-template). Empty is valid — the
	// install path falls back to a static per-task-type keyword map when it is empty.
	TriggerKeywords []string `json:"trigger_keywords"`
	Priority        string   `json:"priority"`
	Confidence      float64  `json:"confidence"`
	EvidenceSummary string   `json:"evidence_summary"`
	AddressesIssues []string `json:"addresses_issues"`
	CitedPaths      []string `json:"cited_paths"`
	// TargetFile / TargetPlacement / ReferenceInFile place a rule/skill into the repo
	// (multi-IDE): TargetFile is the destination path the body targets;
	// TargetPlacement is "append_to_file" | "new_referenced_file"; ReferenceInFile is
	// the existing rules file that imports a new referenced file. The engine derives
	// target_tool from TargetFile and falls back to the nearest existing rule file when
	// TargetFile is empty. Empty on templates (no rules-file target).
	TargetFile      string `json:"target_file"`
	TargetPlacement string `json:"target_placement"`
	ReferenceInFile string `json:"reference_in_file"`
	// Hook-only extras (nullable in the derive schema, so the gate — not tetriz-py —
	// enforces required-ness). Event is the lifecycle event the hook fires on; Matcher
	// the tool/command matcher; Command the best-effort command. Body carries the JSON
	// snippet the tool merges under its hooks key.
	Event   string `json:"event"`
	Matcher string `json:"matcher"`
	Command string `json:"command"`
	// Agent-only extras. AgentName is the subagent's frontmatter identity; Description is
	// the when-to-delegate blurb; Tools scopes its tool access; Model names the tier. The
	// py JSON keys are agent-prefixed to disambiguate from a rule/skill's fields.
	AgentName   string   `json:"agent_name"`
	Description string   `json:"agent_description"`
	Tools       []string `json:"agent_tools"`
	Model       string   `json:"agent_model"`
}

// DerivationResult is the parsed derive output, bucketed by artifact kind. Adding
// a new kind means adding a bucket here AND a matching loop in flattenArtifacts
// AND a case in artifactTypeLabel — keep the three in sync.
type DerivationResult struct {
	Rules     []DerivedArtifact `json:"rules"`
	Templates []DerivedArtifact `json:"templates"`
	Skills    []DerivedArtifact `json:"skills"`
	Hooks     []DerivedArtifact `json:"hooks"`
	Agents    []DerivedArtifact `json:"agents"`
}

// GradedArtifact is one derived artifact with its grade. ArtifactType records the
// bucket it came from; FailedValidator names the first blocking check it failed
// ("" when VISIBLE) for logging and the failure metric.
type GradedArtifact struct {
	Artifact        DerivedArtifact
	ArtifactType    promptlibraryv1.ArtifactType
	SurfaceState    promptlibraryv1.ArtifactSurfaceState
	FailedValidator string
}

// GradedResult is the full graded set, in rules→templates→skills order.
type GradedResult struct {
	Artifacts []GradedArtifact
}

// NoneVisible reports whether the gate hid every artifact (the engine then marks
// the run COMPLETED_NO_VISIBLE and leaves the prior generation latest).
func (g *GradedResult) NoneVisible() bool {
	for _, a := range g.Artifacts {
		if a.SurfaceState == promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE {
			return false
		}
	}
	return true
}

// QualityGate grades derived artifacts against the deterministic validators. It
// reads the project's dismissed (all-history) and recently-hidden (windowed)
// slugs from prompt_library to suppress repeats.
type QualityGate struct {
	libraryDAO      dao.IPromptLibraryDAO
	hiddenDedupRuns func() int
	minLowBucket    func() int
	minHighBucket   func() int
	maxHooksPerRun  func() int
	maxAgentsPerRun func() int
}

// NewQualityGate builds a gate. hiddenDedupRuns supplies how many distinct prior
// runs the recent-hidden dedup window spans (DerivationConfig.HiddenDedupRuns).
// minLowBucket / minHighBucket supply the per-repo session-cohort floors
// (DerivationConfig.MinLowBucket / MinHighBucket) below which a TEMPLATE is hidden
// (validatorTemplateSessionSignal); Rules and Skills are unaffected. maxHooksPerRun /
// maxAgentsPerRun cap the per-run visible HOOK / AGENT counts (a non-blocking top-N
// output limit; a non-positive value means uncapped).
func NewQualityGate(libraryDAO dao.IPromptLibraryDAO, hiddenDedupRuns, minLowBucket, minHighBucket, maxHooksPerRun, maxAgentsPerRun func() int) *QualityGate {
	return &QualityGate{
		libraryDAO:      libraryDAO,
		hiddenDedupRuns: hiddenDedupRuns,
		minLowBucket:    minLowBucket,
		minHighBucket:   minHighBucket,
		maxHooksPerRun:  maxHooksPerRun,
		maxAgentsPerRun: maxAgentsPerRun,
	}
}

// Grade stamps a surface_state on every artifact in result. scopeValue is the
// project's immutable repo id (the artifacts' scope_value the engine will assign);
// repoTree grounds cited paths (nil/empty tree → no path resolves). security is the
// org's windowed incident summary; on a securityOnly run it is the evidence the gate
// grades against — the path checks are relaxed but only an artifact addressing a slug
// the org actually had an incident of survives (the rest are hidden). Returns an
// error only if the dismissed/hidden dedup reads fail — the engine then marks the run
// FAILED. sessionLow/sessionHigh are the repo's low/high session-bucket counts (per the
// configured LowScoreMax/HighScoreMin cutoffs, from the
// AggregatedContext session summary); a TEMPLATE, HOOK, or AGENT is hidden when either is
// below its configured floor (validatorTemplateSessionSignal / validatorHookSessionSignal /
// validatorAgentSessionSignal) — Rules and Skills ignore them.
//
// newTypesEnabled is the org's hooks-and-agents feature flag, re-checked live at derive time
// (fail-closed): a feature-disabled org emits only the original three types (the review floor
// is the admission gate, so every graded run is already review-floor-met).
// resolvedTool is the run's single target tool, resolved once up front from ide_breakdown +
// RepoTree (the gate must not
// guess it from the LLM-authored target_file) — the hook validators need it to check the
// per-tool event allowlist and to confirm a deployable config target exists.
//
// committedSlugs is the set of already-committed VCS artifacts keyed by (type, slug)
// (canonicalized via kebab+normalizeSlug, NOT PromptLibraryForm) for this repo; a net-new
// suggestion whose (type, slug) matches one is dropped as validatorCommittedArtifact (transient).
// Empty/nil disables the check.
func (g *QualityGate) Grade(ctx context.Context, orgID, scopeValue string, result *DerivationResult, repoTree *promptlibraryv1.RepoTree, security *promptlibraryv1.SecuritySummary, sessionLow, sessionHigh int, securityOnly, newTypesEnabled bool, resolvedTool commonv1.IdeType, committedSlugs map[CommittedKey]struct{}) (*GradedResult, error) {
	graded := &GradedResult{}
	flat := flattenArtifacts(result)
	if len(flat) == 0 {
		// validateNonEmpty (response-level, I2): an empty response has nothing to
		// grade and NoneVisible() is trivially true.
		return graded, nil
	}

	// A repo below either session-cohort floor lacks the prompt-quality power that a
	// session-grounded artifact's high-vs-low evidence needs → its TEMPLATES, HOOKS, and
	// AGENTS are hidden (Rules/Skills, being VCS-derived, are unaffected). Computed once
	// for the whole batch, then applied per-type in firstBlockingFailure.
	//
	// NEVER on a securityOnly run: BuildContext omits the SessionSummary there, so both buckets
	// arrive as 0 and would trip this floor — hiding the very secret hook the securityOnly
	// exemption exists to surface. securityOnly bypasses the session gate entirely.
	sessionSignalShort := !securityOnly && (sessionLow < g.minLowBucket() || sessionHigh < g.minHighBucket())

	dismissedSuppress, hiddenSuppress, pinnedSuppress, err := g.suppressionSlugs(ctx, orgID, scopeValue)
	if err != nil {
		return nil, fmt.Errorf("grading derived artifacts: %w", err)
	}
	validPaths := DecodeRepoTreePaths(repoTree)
	validDirs := repoDirsOf(validPaths)
	topLevelDirs := topLevelDirsOf(validPaths)
	securitySlugs := securitySlugsWithEvidence(security)

	// In-run duplicate detection is keyed per (type, slug): a rule and a hook that share a
	// slug are distinct artifacts, so neither should hide the other (matches the per-type
	// is_latest lifecycle and the recent-hidden suppression key).
	seenSlugs := make(map[typedSlugKey]struct{}, len(flat))
	for i := range flat {
		artifact := flat[i].Artifact
		// Slug is the artifact's stable identity; normalize once (trim/lowercase, skill
		// "/"-prefix) and use it for the empty-check, duplicate detection, and
		// suppression so they all key on the same canonical form the engine persists.
		normalizedSlug := normalizeSlug(artifact.Slug, flat[i].ArtifactType)

		// A type-scope drop (feature-disabled org → hooks/agents) is a TRANSIENT drop evaluated
		// FIRST, in the same precedence slot as securityScopedOut: a dropped type must not
		// reserve a slug nor report a quality validator, else its (persisted) HIDDEN row would
		// poison the recent-hidden dedup once the flag state changes.
		typeDrop := typeScopeDrop(flat[i], newTypesEnabled)

		// A securityOnly run scopes out non-security artifacts (security_scope); they
		// are dropped from persistence entirely (toModels). Such an artifact must NOT
		// reserve a slug: otherwise a later corrected security RULE sharing that slug
		// would be hidden as a (persisted) duplicate, poisoning the recent-hidden dedup
		// and leaving the run with no visible security rule.
		scopedOut := securityScopedOut(flat[i].ArtifactType, artifact, securitySlugs, securityOnly)

		// A tool-context drop (tool-less, or a HOOK event that belongs to a different/closed-vocab
		// tool) is TRANSIENT — dropped-not-persisted and re-derived on a later run. Determined
		// HERE, before the duplicate check, so it is excluded from dedup on BOTH sides regardless
		// of processing order: it neither reserves a slug (which would stamp a same-slug valid
		// sibling duplicate_slug) NOR is itself read as a duplicate of an already-seen valid
		// sibling (which would report the persisted duplicate_slug instead of the transient drop,
		// re-persisting it and poisoning the dedup). typeSpecificFailure below re-derives the same
		// validator and meters it — reached because a transient drop keeps isDuplicate false.
		toolContextDrop := hookAgentToolContextDrop(artifact, flat[i].ArtifactType, resolvedTool)

		// Duplicate detection spans the whole response for every artifact that is NOT scoped out,
		// type-dropped, or tool-context-dropped, so a later identical slug is still caught as a
		// duplicate. An empty slug is left out — it fails the slug_missing check first and must
		// not let two empty slugs read as duplicates.
		key := typedSlugKey{artifactType: flat[i].ArtifactType, slug: normalizedSlug}
		canReserveSlug := !scopedOut && typeDrop == "" && toolContextDrop == "" && normalizedSlug != ""
		isDuplicate := false
		if canReserveSlug {
			_, isDuplicate = seenSlugs[key]
			seenSlugs[key] = struct{}{}
		}

		failed := firstBlockingFailure(artifact, normalizedSlug, flat[i].ArtifactType, scopeValue, validPaths, validDirs, topLevelDirs, dismissedSuppress, committedSlugs, hiddenSuppress, pinnedSuppress, securityOnly, scopedOut, isDuplicate, sessionSignalShort, typeDrop, toolContextDrop)
		// A HOOK/AGENT that clears the shared checks still faces the type-specific
		// validators (a hook's event/tool/body copy-safety, an agent's agent_name).
		// Evaluated after the shared checks so a hook missing a slug reports slug_missing;
		// the advisory unknown-for-tool event is metered inside and keeps the hook.
		if failed == "" {
			failed = typeSpecificFailure(artifact, flat[i].ArtifactType, resolvedTool)
		}
		// Meter + log a cross-tool suppression here, where the tool-context drop has taken
		// precedence over the persisting checks. For a non-Kiro tool, hook_event_tool_scoped can
		// only be the cross-tool case (Kiro's is the capability contract, excluded). The counter
		// carries only target_tool, so the log names the suppressed event — because the allowlists
		// are non-exhaustive, that is the actionable detail an operator needs to decide whether the
		// event should be added to the tool's allowlist (a false block) vs a genuine wrong-tool derivation.
		if flat[i].ArtifactType == promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK &&
			failed == validatorHookEventToolScoped && resolvedTool != commonv1.IdeType_IDE_TYPE_KIRO {
			hookEventCrossToolBlockedTotal.WithLabelValues(resolvedTool.String()).Inc()
			logger.Info(ctx, "prompt-library: hook suppressed as a cross-tool event mismatch — if this event is actually valid for the tool, add it to the tool's allowlist",
				zap.String(logger.ORG_ID, orgID), zap.String("repo", scopeValue),
				zap.String("target_tool", resolvedTool.String()), zap.String("event", strings.TrimSpace(artifact.Event)))
		}
		if failed != "" {
			flat[i].SurfaceState = promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_HIDDEN_FAILED_EVAL
			flat[i].FailedValidator = failed
			// The transient scope/context drops are drop-not-persist, not quality failures —
			// they fire on every ordinary below-floor / feature-off / tool-less run, so counting
			// them would swamp the failure dashboard and hide real malformed artifacts.
			if !isMetricExemptDrop(failed) {
				qualityGateFailuresTotal.WithLabelValues(failed, artifactTypeLabel(flat[i].ArtifactType)).Inc()
			}
		} else {
			flat[i].SurfaceState = promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE
		}
		// validateConfidenceRange is advisory — clamp out-of-range confidence in
		// place; it never changes the surface_state.
		flat[i].Artifact.Confidence = clampConfidence(artifact.Confidence)
	}

	// Trim visible hooks/agents beyond the per-run cap (drop-not-persist, never a run
	// failure) AFTER grading, so the cap ranks only artifacts that already passed the
	// gate. Done before the artifactsTotal tally so the counter reflects the final state.
	g.applyTypeCap(flat, promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK, g.maxHooksPerRun)
	g.applyTypeCap(flat, promptlibraryv1.ArtifactType_ARTIFACT_TYPE_AGENT, g.maxAgentsPerRun)

	for i := range flat {
		artifactsTotal.WithLabelValues(artifactTypeLabel(flat[i].ArtifactType), surfaceStateLabel(flat[i].SurfaceState)).Inc()
	}
	graded.Artifacts = flat
	return graded, nil
}

// typeScopeDrop returns the transient drop validator for an artifact type the run's
// eligibility context excludes, or "" when the type is in scope. An org without the feature
// (newTypesEnabled=false) drops HOOK/AGENT and emits only the original three. The review floor
// is the mandatory admission gate, so a graded run is always review-floor-met (a below-floor
// repo never derives). Every drop here is drop-not-persist (see the validator const docs), so
// none of them poisons the dedup.
func typeScopeDrop(gradedArtifact GradedArtifact, newTypesEnabled bool) string {
	switch gradedArtifact.ArtifactType {
	case promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK,
		promptlibraryv1.ArtifactType_ARTIFACT_TYPE_AGENT:
		if !newTypesEnabled {
			return validatorNewTypesDisabled
		}
	case promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE,
		promptlibraryv1.ArtifactType_ARTIFACT_TYPE_TEMPLATE,
		promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL,
		promptlibraryv1.ArtifactType_ARTIFACT_TYPE_UNSPECIFIED,
		promptlibraryv1.ArtifactType_ARTIFACT_TYPE_CLAUDE_MD,
		promptlibraryv1.ArtifactType_ARTIFACT_TYPE_MCP_SERVER,
		promptlibraryv1.ArtifactType_ARTIFACT_TYPE_COMMAND,
		promptlibraryv1.ArtifactType_ARTIFACT_TYPE_PERMISSIONS:
		// Rules/Templates/Skills are always in type-scope once admitted (review floor met);
		// an unresolved type is never type-scoped-out. The shared validators judge them.
		// The detector-finding types (CLAUDE_MD/MCP_SERVER/COMMAND/PERMISSIONS) are persisted
		// by the harness-improve pipeline, not derived through this grading path, so they never
		// reach this switch; listed for exhaustiveness and treated as never type-scoped-out.
	}
	return ""
}

// isMetricExemptDrop reports whether a validator is a transient scope/context drop that
// must stay out of the quality-gate failure metric: a type excluded by the org feature
// (new_types_disabled), a hook/agent the run resolved no tool for (hook_tool / agent_tool),
// or a hook whose event the resolved tool can't run (hook_event_tool_scoped). All fire on
// ordinary feature-off / tool-less / wrong-tool runs, so counting them would swamp the failure
// dashboard and hide real malformed artifacts. security_scope,
// template_session_signal, and the hook/agent session-signal drops are NOT exempt: they are
// drop-not-persist too (see toModels) but counted by design, so an underpowered securityOnly /
// low-session run's suppressed output stays visible in the metric.
func isMetricExemptDrop(validator string) bool {
	return validator == validatorNewTypesDisabled ||
		validator == validatorHookTool || validator == validatorAgentTool ||
		validator == validatorHookEventToolScoped
}

// isTransientDrop reports whether a hide is a transient scope/context drop rather than a
// quality-eval failure: its gating state (the session buckets, a feature flag, the resolved
// tool, the catalog digest, the per-run cap) changes between runs, so the artifact is a good
// one this run's context could not place. toModels drops these instead of persisting them —
// persisting one as HIDDEN_FAILED_EVAL would let the recent-hidden dedup suppress that slug
// long after the state changed.
//
// A HIDDEN_FAILED_EVAL from a genuine quality validator (body/slug/dup/paths, an empty
// hook_event/hook_body/agent_name) is NOT transient: it is malformed regardless of context,
// so it persists and feeds the dedup.
func isTransientDrop(validator string) bool {
	switch validator {
	case validatorSecurityScope, validatorTemplateSessionSignal,
		validatorHookSessionSignal, validatorAgentSessionSignal,
		validatorNewTypesDisabled, validatorTypeCapExceeded,
		validatorPinnedPreserved, validatorCommittedArtifact,
		validatorHookTool, validatorAgentTool, validatorHookEventToolScoped:
		return true
	default:
		return false
	}
}

// typeSpecificFailure runs the HOOK / AGENT type validators on an artifact that has
// already cleared the shared checks, returning the blocking validator or "". For a HOOK
// it also meters the advisory unknown-for-tool event (kept, not failed). Non-hook,
// non-agent types have no type-specific check and return "".
func typeSpecificFailure(artifact DerivedArtifact, artifactType promptlibraryv1.ArtifactType, resolvedTool commonv1.IdeType) string {
	switch artifactType {
	case promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK:
		return hookBlockingFailure(artifact, resolvedTool)
	case promptlibraryv1.ArtifactType_ARTIFACT_TYPE_AGENT:
		// A tool-less run can place no agent — drop transiently (agent_tool) FIRST, before judging
		// name/prose malformations, symmetric with hookBlockingFailure (a later tool-resolving run
		// re-judges; persisting a quality hide here would poison the recent-hidden dedup).
		if resolvedTool == commonv1.IdeType_IDE_TYPE_UNSPECIFIED {
			return validatorAgentTool
		}
		if strings.TrimSpace(artifact.AgentName) == "" {
			return validatorAgentName
		}
		if strings.TrimSpace(artifact.Description) == "" {
			return validatorAgentDescription
		}
		// The agent's instruction prose (body) — the system prompt Go wraps in the tool's envelope
		// (see synthesizeAgentBody) — is covered by the shared body-min-length check above.
		return ""
	default:
		return ""
	}
}

// hookAgentToolContextDrop returns the TRANSIENT tool-context validator for a HOOK/AGENT the
// run's resolved tool cannot place — tool-less (hook_tool/agent_tool), or a HOOK event that
// belongs to a different tool (a Kiro lifecycle event, or a cross-tool event: hook_event_tool_scoped)
// — or "" when the artifact is placeable or fails only an intrinsic malformation (empty
// event/command/name). It is a PURE predicate (no metering) that mirrors the tool-scoped returns
// of hookBlockingFailure IN THE SAME PRECEDENCE — keep the two in sync; hookBlockingFailure stays
// the metering authority. The grade loop consults this BEFORE the duplicate check so a transient
// drop is excluded from dedup on both sides (it neither reserves a slug nor reads as a duplicate).
func hookAgentToolContextDrop(artifact DerivedArtifact, artifactType promptlibraryv1.ArtifactType, resolvedTool commonv1.IdeType) string {
	if resolvedTool == commonv1.IdeType_IDE_TYPE_UNSPECIFIED {
		if artifactType == promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK {
			return validatorHookTool
		}
		if artifactType == promptlibraryv1.ArtifactType_ARTIFACT_TYPE_AGENT {
			return validatorAgentTool
		}
		return ""
	}
	if artifactType != promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK {
		return ""
	}
	event := strings.TrimSpace(artifact.Event)
	if event == "" {
		return "" // an empty event is the hook_event malformation, not a tool-context drop
	}
	if resolvedTool == commonv1.IdeType_IDE_TYPE_KIRO && !isKnownHookEvent(resolvedTool, event) {
		return validatorHookEventToolScoped
	}
	if strings.TrimSpace(artifact.Command) == "" {
		return "" // an empty command is the hook_body malformation, ordered before the cross-tool check
	}
	if _, mapped := hookEventAllowlist[resolvedTool]; mapped && !isKnownHookEvent(resolvedTool, event) && isKnownForAnySupportedTool(event) {
		return validatorHookEventToolScoped
	}
	return ""
}

// hookBlockingFailure returns the blocking validator for a HOOK, or "" when it passes.
// An empty event, no resolved tool, a Kiro lifecycle event the Kiro IDE cannot run
// , or an empty command each hides the hook. An event outside the resolved
// tool's known vocabulary splits two ways: if it provably belongs
// to a DIFFERENT mapped tool it is a cross-tool mismatch — inert here — and is suppressed
// (tool-scoped, transient); if it is unknown across every mapped tool (or the resolved tool
// is itself unmapped) it is genuine drift — advisory: bump the counter and keep the hook.
func hookBlockingFailure(artifact DerivedArtifact, resolvedTool commonv1.IdeType) string {
	// A tool-less run can place no hook at all, so drop every hook transiently (hook_tool)
	// FIRST — before judging its event/body, which are only meaningful against a resolved
	// tool. A later tool-resolving run re-judges and surfaces a good hook; persisting a
	// per-artifact quality hide here would poison the recent-hidden dedup for that slug.
	if resolvedTool == commonv1.IdeType_IDE_TYPE_UNSPECIFIED {
		return validatorHookTool
	}
	event := strings.TrimSpace(artifact.Event)
	if event == "" {
		return validatorHookEvent
	}
	// Kiro's hook vocabulary is CLOSED to its file events (Kiro executes no
	// lifecycle hooks), unlike the other tools' open/drifting vocabularies. So for Kiro an
	// unknown event is blocking, not advisory: any event outside its file-event allowlist
	// names an automation Kiro will never run, so suppress rather than keep-and-meter. The
	// hide is TOOL-DEPENDENT (transient — see validatorHookEventToolScoped), not a malformation.
	if resolvedTool == commonv1.IdeType_IDE_TYPE_KIRO && !isKnownHookEvent(resolvedTool, event) {
		return validatorHookEventToolScoped
	}
	// The command is the hook's payload; without it the synthesized body is a no-op, malformed
	// regardless of tool. Event + command are the load-bearing inputs Go serializes into the resolved
	// tool's shape (matcher is optional — a non-tool event omits it); the copy-paste body is
	// Go-synthesized (see synthesizeHookBody), so the LLM-authored body is not graded here.
	if strings.TrimSpace(artifact.Command) == "" {
		return validatorHookBody
	}
	if !isKnownHookEvent(resolvedTool, event) {
		// An event known to a DIFFERENT mapped tool is a cross-tool mismatch, inert here, so
		// suppress it (see validatorHookEventToolScoped for why this is a transient drop, not a
		// quality hide). The `mapped` guard is load-bearing: an unmapped tool (no allowlist entry,
		// e.g. Antigravity) cannot be judged for a mismatch, so it stays on the advisory path. The
		// suppression is metered + logged by the grade loop (where the drop takes precedence over
		// the persisting shared checks); here it only classifies.
		if _, mapped := hookEventAllowlist[resolvedTool]; mapped && isKnownForAnySupportedTool(event) {
			return validatorHookEventToolScoped
		}
		// Unknown across every mapped tool → genuine vocabulary drift: keep + meter (this is the
		// only path where the hook stays visible, so metering here fires once, for kept hooks only).
		hookEventUnknownTotal.WithLabelValues(resolvedTool.String()).Inc()
	}
	return ""
}

// ValidateHookBody is the copy-safety check applied when an admin edits a hook body. The
// edited body must stay a JSON object and must still reference the structured event and matcher
// — the lifecycle trigger the hook fires on, which govern where/when it runs and are held in
// sync with the panel. It deliberately does NOT check the command: the proto documents
// `command` as admin-edited (the body JSON is the only surface it is edited through), so
// requiring the derive-time command to still appear would reject a legitimate command edit.
// event/matcher stay authoritative here; the command is authoritative in the body. Returns a
// descriptive error naming the failure, or nil when the body is safe. (At derivation time the
// body is Go-synthesized from the structured event/matcher/command — see synthesizeHookBody — a
// different contract from this admin edit of the already-synthesized JSON.)
func ValidateHookBody(body, event, matcher string) error {
	if !isJSONObject(body) {
		return fmt.Errorf("hook body must be a JSON object")
	}
	if !jsonEscapedContains(body, event) || !jsonEscapedContains(body, strings.TrimSpace(matcher)) {
		return fmt.Errorf("hook body must reference the declared event %q and matcher %q", event, matcher)
	}
	return nil
}

// isJSONObject reports whether body, trimmed, is a top-level JSON object. A hook config is
// always a JSON object; json.Valid alone would also accept a bare JSON string / array / scalar
// that happens to carry the event/matcher/command tokens — it passes the token-agreement
// checks but is not a loadable hook when pasted into the tool config. In JSON grammar only an
// object starts with '{', so a well-formed value with a leading brace is exactly an object —
// without forcing a fixed nested schema (the per-tool body shapes differ; see synthesizeHookBody).
func isJSONObject(body string) bool {
	trimmed := strings.TrimSpace(body)
	return len(trimmed) > 0 && trimmed[0] == '{' && json.Valid([]byte(trimmed))
}

// jsonEscapedContains reports whether token, in the JSON-escaped form it takes inside a
// JSON string value, appears in body. An empty token is vacuously present. HTML escaping
// is disabled: json.Marshal would turn shell operators &, <, > into &/</>,
// which a decoded body carries as the literal characters, so a command like `a && b` would
// false-miss and wrongly hide the hook.
func jsonEscapedContains(body, token string) bool {
	if token == "" {
		return true
	}
	return strings.Contains(body, jsonStringEscaper.Replace(token))
}

// jsonStringEscaper produces the JSON-escaped form a token takes inside a JSON string value:
// backslash, quote, and the control chars are escaped, but &, <, > are left LITERAL (a
// decoded body carries them so, unlike json.Marshal's HTML-escaped &/</>).
// NewReplacer does a single non-overlapping pass, so `\`→`\\` does not re-escape its output.
var jsonStringEscaper = strings.NewReplacer(
	`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`,
)

// applyTypeCap hides visible artifacts of artifactType beyond the top maxN, stamping
// validatorTypeCapExceeded. A non-positive cap means uncapped.
func (g *QualityGate) applyTypeCap(flat []GradedArtifact, artifactType promptlibraryv1.ArtifactType, capFn func() int) {
	maxN := capFn()
	if maxN <= 0 {
		return
	}
	applyVisibleCap(flat, maxN, func(gradedArtifact GradedArtifact) bool {
		return gradedArtifact.ArtifactType == artifactType
	})
}

// applyVisibleCap hides the VISIBLE artifacts matching selects beyond the top maxN by
// priority then confidence, stamping validatorTypeCapExceeded (a transient drop, never a
// run failure — so it does not bump the failure metric). maxN is an absolute bound the
// caller has already resolved: zero hides every match. Only already-VISIBLE artifacts are
// ranked; a hidden one stays hidden with its original validator.
func applyVisibleCap(flat []GradedArtifact, maxN int, selects func(GradedArtifact) bool) {
	var visible []int
	for i := range flat {
		if selects(flat[i]) && flat[i].SurfaceState == promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE {
			visible = append(visible, i)
		}
	}
	if len(visible) <= maxN {
		return
	}
	sort.SliceStable(visible, func(a, b int) bool {
		leftPriority, rightPriority := priorityToInt(flat[visible[a]].Artifact.Priority), priorityToInt(flat[visible[b]].Artifact.Priority)
		if leftPriority != rightPriority {
			return leftPriority > rightPriority
		}
		return flat[visible[a]].Artifact.Confidence > flat[visible[b]].Artifact.Confidence
	})
	for _, idx := range visible[maxN:] {
		flat[idx].SurfaceState = promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_HIDDEN_FAILED_EVAL
		flat[idx].FailedValidator = validatorTypeCapExceeded
	}
}

// firstBlockingFailure runs the blocking validators in order and returns the name
// of the first that fails (which becomes FailedValidator + the failure-metric
// label), or "" if the artifact passes them all. Order is cheapest-structural
// first; when an artifact fails more than one check the earliest in this list is
// the one reported.
func firstBlockingFailure(artifact DerivedArtifact, normalizedSlug string, artifactType promptlibraryv1.ArtifactType, scopeValue string, validPaths, validDirs, topLevelDirs map[string]struct{}, dismissedSuppress map[string]struct{}, committedSuppress map[CommittedKey]struct{}, hiddenSuppress, pinnedSuppress map[typedSlugKey]struct{}, securityOnly, scopedOut, isDuplicate, sessionSignalShort bool, typeDrop, toolContextDrop string) string {
	switch {
	case typeDrop != "":
		// A type-scoped-out artifact (a feature-disabled hook/agent) is dropped FIRST — like
		// scopedOut — so it never reserves a slug or reports a quality validator. The drop
		// is transient (toModels drops it), so labeling it a quality validator would
		// re-persist the row and poison the recent-hidden dedup once the flag changes.
		return typeDrop
	case scopedOut:
		// Below-threshold + security-incident run: surface ONLY security-governance
		// RULES (the spec "below threshold → only security rules surface", spec I4) — see
		// securityScopedOut. Checked FIRST so a general artifact that ALSO fails a
		// quality check (missing/duplicate slug, short body) is classified security_scope,
		// not that quality validator — only security_scope hides are dropped from
		// persistence (toModels), so a quality-validator label here would re-persist
		// the row and poison the recent-hidden dedup for a later full run. A valid
		// security RULE is not scopedOut and falls through to the quality checks below.
		return validatorSecurityScope
	case normalizedSlug == "":
		// Slug is the first-class identity; an empty/blank slug (after normalization)
		// has no stable identity to dedup or render on, so it is hidden. The strict
		// derive prompt + Pydantic min_length make this rare; this is the backstop.
		return validatorSlugMissing
	case !isWellFormedSlug(normalizedSlug, artifactType):
		// Slug is non-empty but violates the kebab (rules/templates) / "/"-prefixed
		// kebab (skills) contract — e.g. spaces or punctuation the normalizer doesn't
		// fix. Hidden (non-blocking — the run still completes) so a non-canonical
		// identity can't reach the dashboard or skew exact-match dedup. The strict
		// derive prompt makes this rare; this is the format backstop.
		return validatorSlugFormat
	case artifactType != promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK && len(strings.TrimSpace(artifact.Body)) < bodyMinLength:
		// A HOOK's body is Go-synthesized from the structured event/matcher/command (its LLM body
		// is vestigial), so it is exempt from the min-length check; its payload — the command — is
		// checked in hookBlockingFailure. An AGENT's body IS its instruction prose, so it keeps this
		// check (an empty/short prose is a malformed agent).
		return validatorBodyMinLength
	case artifactType == promptlibraryv1.ArtifactType_ARTIFACT_TYPE_TEMPLATE && sessionSignalShort:
		// Templates, Hooks, and Agents are session-grounded: when the repo's session cohort is
		// below the low/high bucket floors that evidence is underpowered, so the artifact is
		// hidden. Because this hide is TRANSIENT (toModels drops it, never persisting), it is
		// evaluated before the duplicate and scope checks — a session-gated artifact the model
		// emits twice would otherwise have its second copy reported duplicate_slug and persisted
		// HIDDEN, poisoning the slug-keyed recent-hidden dedup so it stays suppressed even after
		// the cohort recovers. Ordered after only the structural identity checks (slug/body) so a
		// malformed artifact still reports that; Rules and Skills (VCS-derived) never reach this.
		return validatorTemplateSessionSignal
	case toolContextDrop != "":
		// A transient tool-context drop (tool-less, or a Kiro/cross-tool HOOK event —
		// hookAgentToolContextDrop) is dropped-not-persisted and re-derives on a later run, so it
		// must take precedence over every PERSISTING check below (duplicate / scope / paths /
		// not_dismissed): otherwise a cross-tool hook whose slug is already recent-hidden would be
		// reported not_dismissed and re-persisted, renewing the entry and keeping the slug
		// suppressed even after a later run resolves the owning tool. Ordered AFTER the intrinsic
		// identity malformations (slug/body) so a malformed hook still reports those, mirroring the
		// session-signal template hide above. The grade loop meters + logs the cross-tool case.
		return toolContextDrop
	case artifactType == promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK && sessionSignalShort &&
		strings.TrimSpace(artifact.Event) != "" && strings.TrimSpace(artifact.Command) != "":
		// Hooks are session-grounded like Templates: low session demand hides them (per-artifact
		// visibility, not admission — the review floor already admitted the repo). Ordered after
		// toolContextDrop so a cross-tool hook still reports its tool-context reason (which the
		// grade loop meters/logs), and — like the template hide — before the persisting checks so
		// a re-emitted slug is not re-persisted as duplicate_slug. Transient (toModels drops it),
		// so a hook hidden for low sessions re-surfaces once the buckets fill. The event/command
		// guard is load-bearing: a MALFORMED hook (empty event/command) must fall through to its
		// persisting malformation (hook_event/hook_body via typeSpecificFailure), not be masked as
		// this transient session hide.
		return validatorHookSessionSignal
	case artifactType == promptlibraryv1.ArtifactType_ARTIFACT_TYPE_AGENT && sessionSignalShort &&
		strings.TrimSpace(artifact.AgentName) != "" && strings.TrimSpace(artifact.Description) != "":
		// Agents mirror the Hook session-visibility gate above (agent_tool tool-less drops are
		// handled by toolContextDrop, checked first). The name/description guard is load-bearing
		// for the same reason: a malformed agent falls through to agent_name/agent_description.
		return validatorAgentSessionSignal
	case isCommitted(artifactType, normalizedSlug, committedSuppress):
		// The repo already commits an artifact of THIS (type, slug), so re-suggesting it is
		// redundant. Keyed by (type, slug) — not slug alone — so a committed command/rule
		// "audit" never suppresses a derived hook/template "audit". Checked BEFORE the
		// persisting checks (duplicate/paths/dismissed) so a committed match is dropped, never
		// persisted as a quality hide — the committed set changes between runs, so a persisted
		// HIDDEN_FAILED_EVAL would poison the recent-hidden dedup once the repo drops that
		// artifact (, transient like the pin/session-signal drops above).
		return validatorCommittedArtifact
	case isDuplicate:
		return validatorDuplicateSlug
	case parseArtifactScope(artifact.Scope) != promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT:
		// This ship emits ARTIFACT_SCOPE_PROJECT only; an org-scope (or unknown)
		// artifact is a backstop violation and hidden.
		return validatorOrgScope
	case scopeValue == "":
		// A project artifact with no repo id to bind cannot be grounded/persisted —
		// guards a misconfigured run.
		return validatorProjectScope
	case !pathsGrounded(artifact, artifactType, validPaths, validDirs, topLevelDirs, securityOnly):
		return validatorPathsExist
	case isPinnedPreserved(normalizedSlug, artifactType, pinnedSuppress):
		// Last, after the persisting quality checks: a re-derived artifact that is ALSO
		// malformed should record its real defect and feed the dedup, not be written off as
		// a pin. Only an otherwise-valid duplicate of a pinned artifact drops here.
		return validatorPinnedPreserved
	case isSuppressed(normalizedSlug, artifactType, dismissedSuppress, hiddenSuppress):
		return validatorNotDismissed
	}
	return ""
}

// isPinnedPreserved reports whether an admin has pinned this exact (type, slug), so the
// pinned row is already holding that recommendation's place on the board.
func isPinnedPreserved(normalizedSlug string, artifactType promptlibraryv1.ArtifactType, pinned map[typedSlugKey]struct{}) bool {
	if normalizedSlug == "" {
		return false // an empty slug is handled by validatorSlugMissing, not here
	}
	_, ok := pinned[typedSlugKey{artifactType: artifactType, slug: normalizedSlug}]
	return ok
}

// securitySlugsWithEvidence returns the set of security taxonomy slugs the org
// actually had an incident of in this window, read off the SecuritySummary's fixed
// per-slug counters. A securityOnly run grades an artifact as a real governance
// artifact only if it addresses one of these — an artifact citing a security slug
// the org has zero incidents of is ungrounded boilerplate, not a governance rule.
// Nil-safe (nil summary → empty set, so a securityOnly run with no evidence hides
// everything → COMPLETED_NO_VISIBLE, the safe direction).
func securitySlugsWithEvidence(security *promptlibraryv1.SecuritySummary) map[string]struct{} {
	slugs := make(map[string]struct{}, 3)
	if security.GetApiKeyLeak() > 0 {
		slugs[slugAPIKeyLeak] = struct{}{}
	}
	if security.GetPiiInjection() > 0 {
		slugs[slugPIIInjection] = struct{}{}
	}
	if security.GetDbCredentialLeak() > 0 {
		slugs[slugDBCredentialLeak] = struct{}{}
	}
	return slugs
}

// securityScopedOut reports whether a securityOnly run filters this artifact out
// as non-security: it is not a RULE, or it is a rule that addresses no real
// detected incident. Such an artifact is the general/boilerplate the underpowered
// below-threshold context produced; it is hidden (security_scope) AND dropped from
// persistence (toModels), so it must not reserve a duplicate title nor poison the
// recent-hidden dedup. A valid security RULE is never scoped out. Always false off
// the securityOnly path.
func securityScopedOut(artifactType promptlibraryv1.ArtifactType, artifact DerivedArtifact, securitySlugs map[string]struct{}, securityOnly bool) bool {
	if !securityOnly {
		return false
	}
	// A securityOnly run surfaces two shapes of security-governance output: a security
	// RULE and the secret / secret-file-protect HOOK. Both are
	// evidence-sensitive — exempt only when addresses_issues names a security slug with
	// present incident evidence, so a general (non-security) rule or hook stays scoped
	// out. AGENT (and every other type) is always scoped out: security never routes to
	// an agent.
	switch artifactType {
	case promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE,
		promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK:
		return !addressesSecurityEvidence(artifact, securitySlugs)
	default:
		return true
	}
}

// addressesSecurityEvidence reports whether the artifact is a security-governance
// artifact for this run: its addresses_issues names at least one security slug the
// org actually had an incident of. Empty addresses_issues → false (an untagged
// artifact cannot be confirmed as security and is hidden on a securityOnly run —
// the tetriz-py derive prompt requires security rules to populate the slug).
func addressesSecurityEvidence(artifact DerivedArtifact, securitySlugs map[string]struct{}) bool {
	for _, slug := range artifact.AddressesIssues {
		if _, ok := securitySlugs[strings.ToLower(strings.TrimSpace(slug))]; ok {
			return true
		}
	}
	return false
}

// pathsGrounded checks that the artifact cites only real repo paths (I12). Every
// cited_paths entry must resolve in the project tree — as a real file, or as a real
// directory (a location reference like "backend/pkg/" that names an area rather than
// one file; the tree index holds only files, so directories are grounded against the
// tree's directory prefixes in validDirs). For a rule/skill, every repo-file-shaped
// token in the body prose must additionally both resolve in the tree AND appear in
// cited_paths — closing the gap where a file mentioned only in prose escapes the
// structured check. A directory named in prose is grounded file-unlike: it must
// resolve to a real repo directory (validDirs), but is deliberately NOT required to
// also appear in cited_paths. That asymmetry is the point — extending the
// cited-membership rule to directories would re-hide a legitimate rule that mentions a
// real dir like "backend/pkg/" in prose without re-declaring it. extractBodyDirs is
// conservative (fires only on an explicit trailing "/" under a real top-level dir), so
// ordinary slashed prose (an "a/b" ratio, "HTTP/1.1", an import path) stays visible. A
// TEMPLATE body is exempt from all prose grounding (files and directories): it is a
// reusable prompt scaffold whose example paths are fill-in placeholders, not repo
// claims (matching the derive prompt's "templates use fill-in placeholders"); its
// structured cited_paths are still validated. Skipped entirely for securityOnly runs
// (security rules legitimately cite no repo path). Empty cited_paths with no graded
// body paths passes.
func pathsGrounded(artifact DerivedArtifact, artifactType promptlibraryv1.ArtifactType, validPaths, validDirs, topLevelDirs map[string]struct{}, securityOnly bool) bool {
	// A HOOK references a tool-config path plus a best-effort command, not repo source
	// files, so it is exempt from repo-source path grounding (the same relaxation a
	// securityOnly run uses) — its deployability is enforced by the hook validators
	// (event + resolved tool). An AGENT cites repo paths and falls through, grounded
	// exactly like a rule/skill.
	if securityOnly || artifactType == promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK {
		return true
	}
	cited := make(map[string]struct{}, len(artifact.CitedPaths))
	for _, citedPath := range artifact.CitedPaths {
		if strings.TrimSpace(citedPath) == "" {
			continue
		}
		// Normalize the citation to the tree's clean repo-relative form before the
		// exact-match lookup, so a real file cited as "./x", "/x", "x/", or with a
		// backslash separator still grounds instead of being wrongly hidden. A
		// hallucinated path stays unresolved (path.Clean of "a/../b" canonicalizes
		// but never invents a tree entry).
		normalized := normalizeRepoPath(citedPath)
		if _, ok := validPaths[normalized]; ok {
			cited[normalized] = struct{}{}
			continue
		}
		// A citation naming a real directory is a legitimate location reference (e.g.
		// "reuse shared utilities in backend/pkg/"). It contributes no file to the
		// prose-grounding set below. A directory that is not a prefix of any real tree
		// file is not in validDirs, so a hallucinated directory is still rejected.
		if _, ok := validDirs[normalized]; ok {
			continue
		}
		return false // neither a real file nor a real directory — hallucinated citation
	}
	// Templates are illustrative prompt scaffolds; their body example paths are not
	// repo claims, so grade them only on the structured cited_paths above.
	if artifactType == promptlibraryv1.ArtifactType_ARTIFACT_TYPE_TEMPLATE {
		return true
	}
	for _, bodyPath := range extractBodyPaths(artifact.Body, topLevelDirs, validPaths) {
		if _, ok := validPaths[bodyPath]; !ok {
			return false // path cited in prose does not exist in the tree
		}
		if _, ok := cited[bodyPath]; !ok {
			return false // path cited in prose but missing from the structured list
		}
	}
	// Directories are grounded file-unlike: resolve in validDirs, no cited_paths
	// membership required (see this function's doc comment;).
	for _, bodyDir := range extractBodyDirs(artifact.Body, topLevelDirs) {
		if _, ok := validDirs[bodyDir]; !ok {
			return false // directory named in prose does not exist in the tree
		}
	}
	return true
}

// repoDirsOf collects every ancestor directory of the tree's file paths, at all
// levels ("backend/pkg/x.go" → "backend", "backend/pkg"), so pathsGrounded can
// ground a cited_paths entry that names a real directory. DecodeRepoTreePaths
// indexes only files, so without this a legitimate directory citation reads as a
// hallucinated path. A directory with no files under it does not exist in the tree
// and is correctly absent, so a hallucinated directory still fails grounding.
func repoDirsOf(validPaths map[string]struct{}) map[string]struct{} {
	dirs := make(map[string]struct{})
	for filePath := range validPaths {
		for i := 0; i < len(filePath); i++ {
			if filePath[i] == '/' {
				dirs[filePath[:i]] = struct{}{}
			}
		}
	}
	return dirs
}

// normalizeRepoPath canonicalizes a cited path to the tree's clean repo-relative
// form: backslashes → "/", path.Clean (collapses "./", "//", "a/../b"), and a
// leading "/" stripped. The caller skips empty citations before this, so the
// path.Clean("")=="." case never reaches here.
func normalizeRepoPath(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	p = path.Clean(p)
	return strings.TrimPrefix(p, "/")
}

// topLevelDirsOf collects the distinct top-level directory names of the tree's
// file paths (the segment before the first "/"; root files contribute none). It
// grounds body-path extraction in the real repo structure.
func topLevelDirsOf(validPaths map[string]struct{}) map[string]struct{} {
	dirs := make(map[string]struct{})
	for path := range validPaths {
		if slash := strings.IndexByte(path, '/'); slash > 0 {
			dirs[path[:slash]] = struct{}{}
		}
	}
	return dirs
}

// extractBodyPaths pulls repo-file-path references out of body prose, grounded in
// the project tree. It is deliberately biased toward never extracting a non-path
// token so a good artifact can't be spuriously hidden. URLs are stripped first,
// then each candidate is kept only if either:
//   - it resolves (normalized) to a real tree file — catches root files
//     ("README.md", "go.mod"), extensionless files ("backend/Dockerfile"), and any
//     real path cited in prose. A non-path token isn't in the tree, so it's never
//     extracted; the membership check is the safety floor.
//   - OR it is a plausible-but-absent path under a real top-level dir with a
//     citable extension ("backend/ghost.go") — a prose-only hallucination to catch
//     (it then fails grounding in pathsGrounded). This branch is what lets the gate
//     hide a hallucinated path the tree-membership branch can't see.
//
// The heuristic branch treats ordinary prose with a slash as non-path: "HTTP/1.1",
// "@org/pkg/index.js" fail the dir gate; "backend/v2.0", "internal/external.split"
// fail the citable-basename gate. The narrow accepted residual is a real-dir +
// real-ext token used architecturally ("client/server.go" when client/ exists) —
// it genuinely looks like a file path. The candidate is normalized first, so a
// non-canonical hallucination ("./backend/ghost.go", trailing slash) is caught by
// the heuristic branch rather than slipping past the first-segment check.
func extractBodyPaths(body string, topLevelDirs, validPaths map[string]struct{}) []string {
	cleaned := urlPattern.ReplaceAllString(body, " ")
	seen := make(map[string]struct{})
	var paths []string
	addPath := func(p string) {
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}
	for _, token := range bodyPathCandidate.FindAllString(cleaned, -1) {
		token = strings.TrimRight(token, ".,;:!?)")
		if token == "" {
			continue
		}
		// Canonicalize once and use the result for BOTH branches, so a "./"- or
		// trailing-slash spelling can't bypass the heuristic's first-segment check.
		normalized := normalizeRepoPath(token)
		// A token that resolves to a real tree file is a genuine reference,
		// regardless of slash/extension shape (covers root + extensionless files).
		if _, ok := validPaths[normalized]; ok {
			addPath(normalized)
			continue
		}
		// Otherwise only a plausible path under a real top-level dir with a citable
		// basename is a candidate (a prose-only hallucination caught downstream).
		slash := strings.IndexByte(normalized, '/')
		if slash <= 0 {
			continue
		}
		if _, ok := topLevelDirs[normalized[:slash]]; !ok {
			continue
		}
		if isCitableBasename(normalized[strings.LastIndexByte(normalized, '/')+1:]) {
			addPath(normalized)
		}
	}
	return paths
}

// extractBodyDirs pulls directory-shaped references out of body prose. It is a
// separate, deliberately narrow pass from extractBodyPaths (which handles files) so
// the file heuristics stay untouched. A token is a directory candidate only on a
// strong signal — an explicit trailing "/" AND a first segment that is a real
// top-level dir — which is what keeps ordinary slashed prose ("HTTP/1.1", an "a/b"
// ratio, "input/output", a Go import path) from being read as a directory. A token
// with no trailing slash is left ambiguous and never extracted: better to miss a
// hallucination than to hide a good artifact. The returned candidates are normalized
// and grounded against validDirs by the caller. URLs are stripped first so a link
// path is never mistaken for a repo directory.
//
// The trailing slash is treated as a directory claim, not just a prose hint: a
// slash-joined compound written with a trailing slash under a real top-level dir
// ("backend/frontend/") is evaluated as a path and hidden if it does not resolve —
// the same signal that catches "backend/ghost/". The ordinary prose form of such a
// compound carries no trailing slash ("backend/frontend split") and is not extracted;
// a real directory always resolves in validDirs and stays visible.
func extractBodyDirs(body string, topLevelDirs map[string]struct{}) []string {
	cleaned := urlPattern.ReplaceAllString(body, " ")
	seen := make(map[string]struct{})
	var dirs []string
	for _, token := range bodyPathCandidate.FindAllString(cleaned, -1) {
		// Trim trailing sentence punctuation but keep "/", so "backend/pkg/." yields
		// "backend/pkg/" and the trailing-slash signal survives.
		token = strings.TrimRight(token, ".,;:!?)")
		if !strings.HasSuffix(token, "/") {
			continue // no explicit trailing slash — ambiguous, not treated as a dir
		}
		normalized := normalizeRepoPath(token) // path.Clean strips the trailing slash
		slash := strings.IndexByte(normalized, '/')
		firstSegment := normalized
		if slash > 0 {
			firstSegment = normalized[:slash]
		}
		if _, ok := topLevelDirs[firstSegment]; !ok {
			continue // first segment is not a real top-level dir — leave ambiguous
		}
		// A token whose last segment looks like a file (e.g. a stray "backend/main.go/")
		// is a file reference handled by extractBodyPaths, not a directory — skip it so a
		// real file is never mis-flagged as a hallucinated dir.
		if isCitableBasename(normalized[strings.LastIndexByte(normalized, '/')+1:]) {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		dirs = append(dirs, normalized)
	}
	return dirs
}

// isCitableBasename reports whether a path's last segment names a file the gate
// should ground: a dotted name with a citable extension (foo.go, ci.yml) OR a
// known extensionless file (Dockerfile, Makefile). A dotted name with a non-file
// extension ("v2.0", "external.split") and a generic extensionless name
// ("handlers", a directory reference) both return false — the conservative
// direction that keeps prose from being treated as a path.
func isCitableBasename(base string) bool {
	if dot := strings.LastIndexByte(base, '.'); dot >= 0 {
		_, ok := citableFileExtensions[strings.ToLower(base[dot+1:])]
		return ok
	}
	_, ok := citableExtensionlessFiles[strings.ToLower(base)]
	return ok
}

// isCommitted reports whether this (artifactType, normalizedSlug) matches an artifact the repo
// already commits (a VCS harness_artifacts row canonicalized into committed by kebab+normalizeSlug,
// keyed by type). (type, slug) exact-set membership, mirroring the recent-hidden suppression's
// typedSlugKey lookup — so a committed rule/command can't suppress a derived hook/template of the
// same name. An empty committed set makes this a no-op.
func isCommitted(artifactType promptlibraryv1.ArtifactType, normalizedSlug string, committed map[CommittedKey]struct{}) bool {
	if normalizedSlug == "" {
		return false // an empty slug is handled by validatorSlugMissing, not here
	}
	_, ok := committed[CommittedKey{Type: artifactType, Slug: normalizedSlug}]
	return ok
}

// isSuppressed reports whether the artifact's normalized slug exactly matches a
// dismissed or recently-hidden slug. Slug is the stable identity,
// so suppression is exact-set membership (not the former fuzzy title match) — both
// sides are normalized through normalizeSlug, so a case/prefix variant can't escape.
// isSuppressed reports whether a (type, slug) is a persisted hide — an admin dismissal (slug-only) or a
// recent-hidden (type, slug). It delegates to harnessGateHiddenReason so the suppression decision and
// the gate_hidden_total reason attribution share one source of truth for the precedence (dismissal
// before recent-hidden) and the empty-slug carve-out; a non-empty reason means suppressed.
func isSuppressed(normalizedSlug string, artifactType promptlibraryv1.ArtifactType, dismissed map[string]struct{}, hidden map[typedSlugKey]struct{}) bool {
	return harnessGateHiddenReason(normalizedSlug, artifactType, dismissed, hidden) != ""
}

// normalizeSlug canonicalizes a derived slug for storage and dedup matching:
// trimmed + lowercased, with the skill "/"-prefix convention enforced (skills get
// exactly one leading "/"; rules/templates carry none), and capped at maxSlugLen so
// the stored value always fits the prompt_library.slug column. It does NOT fabricate
// a slug from the title — a blank or slash-only input returns "" so the quality gate
// hides the artifact (validatorSlugMissing). The engine persists this exact form and
// the gate suppresses against it, so the two never disagree.
func normalizeSlug(slug string, artifactType promptlibraryv1.ArtifactType) string {
	cleaned := strings.ToLower(strings.TrimSpace(slug))
	cleaned = strings.TrimLeft(cleaned, "/")
	if cleaned == "" {
		return ""
	}
	if artifactType == promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL {
		cleaned = "/" + cleaned
	}
	return truncateToBytes(cleaned, maxSlugLen)
}

// truncateToBytes returns s capped to at most maxBytes bytes, trimming any trailing
// partial rune so the result stays valid UTF-8. A correctly-formed kebab slug is
// pure ASCII (one byte per rune), so this is a plain cut in the only case that
// matters; the rune trim only guards a pathological multibyte input (which the
// kebab gate hides anyway). The cap protects the DB column — see maxSlugLen.
func truncateToBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	truncated := s[:maxBytes]
	for len(truncated) > 0 && !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}
	return truncated
}

// isWellFormedSlug reports whether an already-normalized slug honors the per-type
// shape: a skill is a single leading "/" + a kebab body; a rule/template is a bare
// kebab body. Caller passes the normalizeSlug output, so this is a format backstop
// for what the normalizer can't fix (spaces, underscores, dots, empty hyphen
// groups) — not a re-normalization. An empty slug is the caller's slug_missing case
// and never reaches here.
func isWellFormedSlug(normalizedSlug string, artifactType promptlibraryv1.ArtifactType) bool {
	// A slug at the column ceiling is either pathological or normalizeSlug-truncated;
	// either way it is not a usable identity, so hide it (slug_format) rather than
	// surface a clipped slug as visible. A legitimate slug is far shorter, so this
	// never false-hides a real one. Pairs with the maxSlugLen cap in normalizeSlug.
	if len(normalizedSlug) >= maxSlugLen {
		return false
	}
	if artifactType == promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL {
		body, ok := strings.CutPrefix(normalizedSlug, "/")
		return ok && kebabSlugPattern.MatchString(body)
	}
	return kebabSlugPattern.MatchString(normalizedSlug)
}

// typedSlugKey keys the mechanical dedup — the in-run duplicate check and the recent-hidden
// suppression — by (artifact_type, slug). A rule and a hook that share a slug are distinct
// artifacts (different target files, different mechanisms), so keying on slug alone would let a
// hidden/duplicate one hide the other, and would contradict the per-type is_latest lifecycle
// (reconcileLatest buckets by artifact_type). Both the recent-hidden and the pinned-preserved
// suppressions key on it. Admin dismissal stays slug-only — a separate, intentional concern
// (an admin dismisses a name, independent of type).
type typedSlugKey struct {
	artifactType promptlibraryv1.ArtifactType
	slug         string
}

// CommittedKey identifies a committed VCS artifact by (type, slug) for the additive-dedup set
// . Exported — unlike the internal typedSlugKey — because it appears in the qualityGrader
// interface's Grade signature, which a separate-package mock (and the external engine test) must
// name and construct. Same (type, slug) shape as typedSlugKey; kept distinct so the interface
// surface stays intentional rather than leaking an internal helper.
type CommittedKey struct {
	Type promptlibraryv1.ArtifactType
	Slug string
}

// suppressionSlugs returns the two exact-match suppression sets the gate applies: the project's
// all-history admin-DISMISSED slugs (keyed by slug) and its recently-HIDDEN slugs (windowed to
// the last hiddenDedupRuns distinct runs, keyed by (artifact_type, slug) so a hidden rule does
// not suppress a same-slug hook). Stored slugs are already canonical (normalizeSlug ran at
// persist, so skills keep their leading "/"); this only lowercases + trims as a defensive guard
// against case/whitespace drift — it deliberately does NOT strip the leading "/", so a stored
// skill slug "/foo" stays "/foo" and matches the gate-side normalizeSlug("foo", SKILL) == "/foo".
func (g *QualityGate) suppressionSlugs(ctx context.Context, orgID, scopeValue string) (map[string]struct{}, map[typedSlugKey]struct{}, map[typedSlugKey]struct{}, error) {
	dismissed, err := g.libraryDAO.ListDismissedSlugs(ctx, orgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, scopeValue)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("listing dismissed slugs for org %s repo %s: %w", orgID, scopeValue, err)
	}
	// nil recommendation source = the NULL-source DERIVED lane (DETECTED detector findings live in
	// their own dedup lane and must not suppress a same-slug derived artifact).
	hidden, err := g.libraryDAO.ListRecentHiddenFailedSlugs(ctx, orgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, scopeValue, nil, g.hiddenDedupRuns())
	if err != nil {
		return nil, nil, nil, fmt.Errorf("listing recent hidden slugs for org %s repo %s: %w", orgID, scopeValue, err)
	}
	pinned, err := g.libraryDAO.ListPinnedSlugs(ctx, orgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, scopeValue)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("listing pinned slugs for org %s repo %s: %w", orgID, scopeValue, err)
	}
	dismissedSet := make(map[string]struct{}, len(dismissed))
	for _, slug := range dismissed {
		if normalized := strings.ToLower(strings.TrimSpace(slug)); normalized != "" {
			dismissedSet[normalized] = struct{}{}
		}
	}
	hiddenSet := make(map[typedSlugKey]struct{}, len(hidden))
	for _, row := range hidden {
		if normalized := strings.ToLower(strings.TrimSpace(row.Slug)); normalized != "" {
			hiddenSet[typedSlugKey{artifactType: row.ArtifactType, slug: normalized}] = struct{}{}
		}
	}
	pinnedSet := make(map[typedSlugKey]struct{}, len(pinned))
	for _, row := range pinned {
		if normalized := strings.ToLower(strings.TrimSpace(row.Slug)); normalized != "" {
			pinnedSet[typedSlugKey{artifactType: row.ArtifactType, slug: normalized}] = struct{}{}
		}
	}
	return dismissedSet, hiddenSet, pinnedSet, nil
}

func flattenArtifacts(result *DerivationResult) []GradedArtifact {
	if result == nil {
		return nil
	}
	flat := make([]GradedArtifact, 0, len(result.Rules)+len(result.Templates)+len(result.Skills)+len(result.Hooks)+len(result.Agents))
	for _, artifact := range result.Rules {
		flat = append(flat, GradedArtifact{Artifact: artifact, ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE})
	}
	for _, artifact := range result.Templates {
		flat = append(flat, GradedArtifact{Artifact: artifact, ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_TEMPLATE})
	}
	for _, artifact := range result.Skills {
		flat = append(flat, GradedArtifact{Artifact: artifact, ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL})
	}
	for _, artifact := range result.Hooks {
		flat = append(flat, GradedArtifact{Artifact: artifact, ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK})
	}
	for _, artifact := range result.Agents {
		flat = append(flat, GradedArtifact{Artifact: artifact, ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_AGENT})
	}
	return flat
}

// parseArtifactScope maps the LLM's lowercase scope string to the proto enum;
// anything other than "project"/"org" is UNSPECIFIED (and so hidden by the
// project-only backstop).
func parseArtifactScope(scope string) promptlibraryv1.ArtifactScope {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case artifactScopeProject:
		return promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT
	case "org":
		return promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_ORG
	default:
		return promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_UNSPECIFIED
	}
}

func clampConfidence(confidence float64) float64 {
	if confidence < 0 {
		return 0
	}
	if confidence > 1 {
		return 1
	}
	return confidence
}

func artifactTypeLabel(t promptlibraryv1.ArtifactType) string {
	switch t {
	case promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE:
		return "rule"
	case promptlibraryv1.ArtifactType_ARTIFACT_TYPE_TEMPLATE:
		return "template"
	case promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL:
		return "skill"
	case promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK:
		return "hook"
	case promptlibraryv1.ArtifactType_ARTIFACT_TYPE_AGENT:
		return "agent"
	default:
		return unspecifiedLabel
	}
}

func surfaceStateLabel(s promptlibraryv1.ArtifactSurfaceState) string {
	switch s {
	case promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE:
		return "visible"
	case promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_HIDDEN_FAILED_EVAL:
		return "hidden_failed_eval"
	default:
		return unspecifiedLabel
	}
}
