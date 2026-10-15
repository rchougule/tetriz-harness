// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"path"
	"sort"
	"strings"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// Instruction-Intent detector thresholds — engineer-chosen PROVISIONAL literals, not a settled
// decision: no in-repo reference implementation exists and the spec specifies no values. They are
// calibration placeholders that must be re-derived against real data (and, like the sibling
// behavioral window, likely homed in the config package) before the detector is enabled; each one changes
// which orgs' files get flagged.
const (
	// instructionIntentTokenFloor is the always-loaded token ceiling a root CLAUDE.md should stay
	// under; over it, the file taxes every request's context budget.
	instructionIntentTokenFloor = 12000
	// instructionIntentMinTokensForShare is the size below which the directive-share signal is not
	// meaningful (a tiny file is legitimately mostly prose).
	instructionIntentMinTokensForShare = 1000
	// instructionIntentMaxDirectiveShare is the directive-line fraction below which a file reads as
	// mostly prose rather than instructions.
	instructionIntentMaxDirectiveShare = 0.15
	// instructionIntentTokLineQuartileMinCorpus is the minimum count of instruction artifacts with a
	// body needed to compute a meaningful tok/line quartile; below it the tok/line lens is skipped
	// (insufficient corpus).
	instructionIntentTokLineQuartileMinCorpus = 4
)

// Instruction-Intent finding families — the metric that fired, threaded into the slug and used to
// pick the human title. Stable identifiers, not user copy.
const (
	instructionIntentFamilyTokenFloor     = "token-floor"
	instructionIntentFamilyTokPerLine     = "tok-per-line"
	instructionIntentFamilyDirectiveShare = "directive-share"
)

// estimateTokens approximates the token count of content as len/4. PROVISIONAL: there is no
// in-repo tokenizer and the detector spec leaves the estimator unspecified, so this
// uses the common chars/4 heuristic. Byte length, not rune count — the estimate is coarse by design.
func estimateTokens(content string) int {
	return len(content) / 4
}

// countContentLines returns the number of non-blank lines in content, floored at 1 so callers can
// divide by it safely. A "blank" line is empty after trimming surrounding whitespace.
func countContentLines(content string) int {
	count := 0
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	if count < 1 {
		return 1
	}
	return count
}

// nonBlankContent returns content's non-blank lines joined by "\n" — the same lines
// countContentLines counts. tokPerLine estimates tokens over this so the numerator and denominator
// measure the same text; a mostly-blank body no longer inflates its density.
func nonBlankContent(content string) string {
	nonBlank := make([]string, 0)
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) != "" {
			nonBlank = append(nonBlank, line)
		}
	}
	return strings.Join(nonBlank, "\n")
}

// tokPerLine is the estimated tokens per content line — the density signal the tok/line quartile
// ranks. Both numerator and denominator use the non-blank content only (nonBlankContent /
// countContentLines), so blank lines never distort the ratio; the denominator is floored at 1.
func tokPerLine(content string) float64 {
	return float64(estimateTokens(nonBlankContent(content))) / float64(countContentLines(content))
}

// directiveShare returns the fraction of a document's content lines that are directives, plus
// whether that share is reliable. A CONTENT line is non-blank and not inside a ``` fenced code
// block; a DIRECTIVE line is additionally not a `#` heading and its trimmed text starts with a list
// marker (`- `, `* `, `+ `, or a number then `.`/`)` then a space). share is 0 when there are no
// content lines. reliable is false when a ``` fence was left open at end-of-scan (an unbalanced
// fence — a typo or a body truncated mid-fence at the content-size cap — silently swallows the rest
// of the file, so its share cannot be trusted). A low reliable share means the file is mostly prose
// rather than actionable instructions.
func directiveShare(content string) (share float64, reliable bool) {
	contentLines := 0
	directiveLines := 0
	inFence := false
	for _, rawLine := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(rawLine)
		if strings.HasPrefix(trimmed, "```") {
			// The fence delimiter is not itself content; toggle the code-block state around it.
			inFence = !inFence
			continue
		}
		if inFence || trimmed == "" {
			continue
		}
		contentLines++
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if startsWithListMarker(trimmed) {
			directiveLines++
		}
	}
	if contentLines == 0 {
		return 0, !inFence
	}
	return float64(directiveLines) / float64(contentLines), !inFence
}

// startsWithListMarker reports whether trimmed (already whitespace-trimmed) opens with a Markdown
// list marker: `- `, `* `, `+ `, or an ordered marker (a digit run then `.` or `)` then a space).
func startsWithListMarker(trimmed string) bool {
	if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") || strings.HasPrefix(trimmed, "+ ") {
		return true
	}
	digits := 0
	for digits < len(trimmed) && trimmed[digits] >= '0' && trimmed[digits] <= '9' {
		digits++
	}
	if digits == 0 {
		return false
	}
	// Need "<digits><.|)> " — a delimiter then a space directly after the digit run.
	if digits+1 >= len(trimmed) {
		return false
	}
	delim := trimmed[digits]
	return (delim == '.' || delim == ')') && trimmed[digits+1] == ' '
}

// percentile75NearestRank returns the 75th percentile of values by the nearest-rank method: sort
// ascending, rank = ceil(0.75 * n), return values[rank-1]. Callers gate on len(values) >= the
// corpus floor, so an empty slice is never passed (it would panic — that is the caller's contract).
// The input slice is copied before sorting so the caller's order is preserved.
func percentile75NearestRank(values []float64) float64 {
	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)
	rank := int(math.Ceil(0.75 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	return sorted[rank-1]
}

// isInstructionArtifact reports whether an artifact is an instruction/config document the
// Instruction-Intent lens measures: a CLAUDE.md, an AGENTS.md, or a rule file.
func isInstructionArtifact(a *model.HarnessArtifact) bool {
	switch a.Type {
	case commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_CLAUDE_MD,
		commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_AGENTS_MD,
		commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE:
		return true
	default:
		return false
	}
}

// isRootAlwaysLoadedInstruction reports whether an artifact is a repo-root always-loaded instruction
// file — a root CLAUDE.md or AGENTS.md (Codex-convention repos use AGENTS.md) — the file the
// token-floor lens targets. It must be one of those types whose source path is root-level, i.e.
// `path.Dir(SourcePath) == "."` (a bare `CLAUDE.md`/`AGENTS.md` or any path with no directory component).
func isRootAlwaysLoadedInstruction(a *model.HarnessArtifact) bool {
	switch a.Type {
	case commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_CLAUDE_MD,
		commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_AGENTS_MD:
	default:
		return false
	}
	return path.Dir(a.SourcePath) == "."
}

// instructionArtifactType maps a committed harness artifact's type onto the prompt_library
// artifact-type column. CLAUDE.md and AGENTS.md both map to ARTIFACT_TYPE_CLAUDE_MD (the shared
// instruction-file type); a rule maps to ARTIFACT_TYPE_RULE; a SKILL/COMMAND/AGENT maps to its own
// type so a Conflict or behavioral-deviation finding on that body carries the correct ArtifactType; a PERMISSIONS
// carrier maps to ARTIFACT_TYPE_PERMISSIONS so a per-repo Security finding on a settings.json body
// carries the correct ArtifactType.
func instructionArtifactType(t commonv1.HarnessArtifactType) promptlibraryv1.ArtifactType {
	switch t {
	case commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_CLAUDE_MD,
		commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_AGENTS_MD:
		return promptlibraryv1.ArtifactType_ARTIFACT_TYPE_CLAUDE_MD
	case commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE:
		return promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE
	case commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL:
		return promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL
	case commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_COMMAND:
		return promptlibraryv1.ArtifactType_ARTIFACT_TYPE_COMMAND
	case commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_AGENT:
		// A behavioral-deviation finding on a subagent body carries the correct AGENT type.
		return promptlibraryv1.ArtifactType_ARTIFACT_TYPE_AGENT
	case commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_PERMISSIONS:
		return promptlibraryv1.ArtifactType_ARTIFACT_TYPE_PERMISSIONS
	default:
		// Any committable HarnessArtifactType a finding can be a SUBJECT of MUST have a case above.
		// UNSPECIFIED here is a silent drop — vanishedDetectedPartitions and deriveEvaluationStatus
		// both skip UNSPECIFIED, so a new type maps to nothing, produces no finding, and no evaluated
		// partition, with no compile error. Adding a finding-subject type is a coupled edit: this map,
		// the bodyRequiredFor* predicates, and (for a behavioral candidate) behavioralEntityType.
		return promptlibraryv1.ArtifactType_ARTIFACT_TYPE_UNSPECIFIED
	}
}

// bodiedInstructionArtifact pairs an instruction artifact with its fetched committed body.
type bodiedInstructionArtifact struct {
	artifact *model.HarnessArtifact
	body     string
}

// bodyRequiredForInstruction reports whether an artifact's body is input to the Instruction-Intent
// lens — the instruction/config documents it measures (CLAUDE.md / AGENTS.md / rule).
func bodyRequiredForInstruction(a *model.HarnessArtifact) bool {
	return isInstructionArtifact(a)
}

// bodiedArtifacts collects the aggregate's committed artifacts that satisfy want AND have a fetched
// body, in committed order. A body is absent when it was unresolvable, missing, or a transient read
// error — such an artifact simply contributes no finding.
func bodiedArtifacts(aggregate harnessImproveAggregate, want func(*model.HarnessArtifact) bool) []bodiedInstructionArtifact {
	bodied := make([]bodiedInstructionArtifact, 0, len(aggregate.committed))
	for _, artifact := range aggregate.committed {
		if !want(artifact) {
			continue
		}
		body, ok := aggregate.bodies[artifact.ID]
		if !ok {
			continue
		}
		bodied = append(bodied, bodiedInstructionArtifact{artifact: artifact, body: body})
	}
	return bodied
}

// bodiedInstructionArtifacts collects the aggregate's instruction artifacts that have a fetched body,
// in committed order — the input to the Instruction-Intent lens. Filtered to instruction types only,
// so the Instruction-Intent iteration never picks up a SKILL/COMMAND the Conflict lens fetches.
func bodiedInstructionArtifacts(aggregate harnessImproveAggregate) []bodiedInstructionArtifact {
	return bodiedArtifacts(aggregate, bodyRequiredForInstruction)
}

// instructionIntentFindings runs the Instruction-Intent deterministic placement lens over the
// aggregate's committed artifacts and their fetched bodies, emitting one Restructure finding per
// tripped metric. It is pure over the aggregate (no I/O) so the detector logic is unit-testable.
// An artifact contributes only when its body was fetched (present in aggregate.bodies).
func instructionIntentFindings(orgID, repoID, taskExecutionID string, aggregate harnessImproveAggregate) []*model.PromptLibraryItem {
	instruction := bodiedInstructionArtifacts(aggregate)

	var findings []*model.PromptLibraryItem

	// Token floor: a repo-root always-loaded instruction file (CLAUDE.md or AGENTS.md) whose
	// estimated size exceeds the always-loaded ceiling. Every such root file with a fetched body is
	// already in the instruction slice (both are instruction types), so filter that instead of
	// re-scanning committed + re-probing bodies.
	for _, bi := range instruction {
		if !isRootAlwaysLoadedInstruction(bi.artifact) {
			continue
		}
		tokens := estimateTokens(bi.body)
		if tokens > instructionIntentTokenFloor {
			findings = append(findings, newInstructionIntentFinding(orgID, repoID, taskExecutionID, bi.artifact,
				instructionIntentFamilyTokenFloor, "Always-loaded file is large enough to crowd context",
				fmt.Sprintf("est. %d tokens > floor %d", tokens, instructionIntentTokenFloor)))
		}
	}

	// tok/line quartile: over the instruction corpus, flag every artifact whose density is strictly
	// above the repo's own 75th percentile. Needs a corpus floor to be a meaningful quartile.
	if len(instruction) >= instructionIntentTokLineQuartileMinCorpus {
		densities := make([]float64, len(instruction))
		for i, bi := range instruction {
			densities[i] = tokPerLine(bi.body)
		}
		p75 := percentile75NearestRank(densities)
		for _, bi := range instruction {
			density := tokPerLine(bi.body)
			if density > p75 {
				findings = append(findings, newInstructionIntentFinding(orgID, repoID, taskExecutionID, bi.artifact,
					instructionIntentFamilyTokPerLine, "Lines are denser than most files in this repo",
					fmt.Sprintf("tok/line %.1f > repo 75th-pctl %.1f", density, p75)))
			}
		}
	}

	// Directive share: a non-trivial instruction file that is mostly prose (few directive lines).
	// An unreliable share (unbalanced fence) must not drive a finding — skip it.
	for _, bi := range instruction {
		if estimateTokens(bi.body) <= instructionIntentMinTokensForShare {
			continue
		}
		share, reliable := directiveShare(bi.body)
		if !reliable {
			continue
		}
		if share < instructionIntentMaxDirectiveShare {
			findings = append(findings, newInstructionIntentFinding(orgID, repoID, taskExecutionID, bi.artifact,
				instructionIntentFamilyDirectiveShare, "Mostly prose — few clear instructions to act on",
				fmt.Sprintf("directive share %.0f%% < floor %.0f%%", share*100, instructionIntentMaxDirectiveShare*100)))
		}
	}

	return findings
}

// newInstructionIntentFinding builds one advisory Restructure finding for the Instruction-Intent
// lens via the shared newDeterministicFinding builder, stamping dimension INSTRUCTION_INTENT and the
// LOW priority every such finding carries. referencePoint is the concrete metric detail (e.g. the
// measured value vs threshold); title is the human sentence for the family.
func newInstructionIntentFinding(orgID, repoID, taskExecutionID string, artifact *model.HarnessArtifact,
	family, title, referencePoint string,
) *model.PromptLibraryItem {
	return newProjectFinding(orgID, repoID, taskExecutionID, artifact,
		promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_INSTRUCTION_INTENT,
		priorityLow, instructionIntentSlug(repoID, family, artifact.ID), title, referencePoint)
}

// newProjectFinding builds one PROJECT-scoped deterministic finding via newDeterministicFinding,
// stamping Scope=PROJECT, ScopeValue=repoID, ArtifactType from the artifact's own type
// (instructionArtifactType), and HarnessArtifactID=&artifact.ID — the boilerplate every per-repo
// detector (Instruction-Intent, Conflict, per-repo Security) shares. The caller supplies its
// dimension, priority, the finished slug, title, and the evidence reference_point.
func newProjectFinding(orgID, repoID, taskExecutionID string, artifact *model.HarnessArtifact,
	dimension promptlibraryv1.HarnessFindingDimension, priority int32, slug, title, referencePoint string,
) *model.PromptLibraryItem {
	return newDeterministicFinding(orgID, taskExecutionID, dimension,
		promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, repoID, instructionArtifactType(artifact.Type),
		&artifact.ID, priority, slug, title, referencePoint, artifact.SourcePath, artifact.Name)
}

// newDeterministicFinding builds the PromptLibraryItem shared by the deterministic detectors
// (Instruction-Intent, Conflict, Security): an advisory finding — nil artifact_content (advisory
// findings carry no body), confidence 1.0, VISIBLE surface, DETECTED source, the metric/detail as the
// evidence reference_point, and the DAO's fillArtifactDefaults backfilling the NOT NULL JSONB columns
// left nil. It is the low-level primitive; callers reach it through one of the two scope wrappers —
// newProjectFinding (Scope=PROJECT, ScopeValue=repoID, ArtifactType from the artifact) and
// orgSecurityFinding (Scope=ORG, ScopeValue empty per the model convention, harnessArtifactID nil for
// a relational finding with no single anchor artifact). Each detector stamps its own dimension and
// priority and computes its own slug, passing the finished slug in.
func newDeterministicFinding(orgID, taskExecutionID string, dimension promptlibraryv1.HarnessFindingDimension,
	scope promptlibraryv1.ArtifactScope, scopeValue string, artifactType promptlibraryv1.ArtifactType,
	harnessArtifactID *string, priority int32, slug, title, referencePoint, sourcePath, sourceName string,
) *model.PromptLibraryItem {
	source := promptlibraryv1.ArtifactRecommendationSource_ARTIFACT_RECOMMENDATION_SOURCE_DETECTED
	fc := findingCopyFor(slugFamily(slug), referencePoint, "")
	return &model.PromptLibraryItem{
		OrganizationID:       orgID,
		TaskExecutionID:      taskExecutionID,
		Dimension:            &dimension,
		RecommendationSource: &source,
		HarnessArtifactID:    harnessArtifactID,
		Priority:             priority,
		ConfidenceScore:      1.0,
		SurfaceState:         promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE,
		ClientStatus:         promptlibraryv1.ArtifactClientStatus_ARTIFACT_CLIENT_STATUS_UNSPECIFIED,
		ArtifactType:         artifactType,
		Scope:                scope,
		ScopeValue:           scopeValue,
		Slug:                 slug,
		Title:                title,
		// Advisory by default; attachGroundedFixes flips it false when a grounded fix is attached.
		Evidence: &promptlibraryv1.EvidenceProjection{
			ReferencePoint:    referencePoint,
			Summary:           fc.Summary,
			RecommendedAction: fc.RecommendedAction,
			Advisory:          true,
			SourcePath:        sourcePath,
			SourceName:        sourceName,
		},
	}
}

// instructionIntentSlug builds the stable finding slug "<repoID>:instruction-intent:<family>:<hash>"
// where hash is the first 12 hex of sha256(artifactID + ":" + family) — stable per (artifact,
// family) so re-runs converge on the same slug.
func instructionIntentSlug(repoID, family, artifactID string) string {
	sum := sha256.Sum256([]byte(artifactID + ":" + family))
	shortHash := hex.EncodeToString(sum[:])[:12]
	return fmt.Sprintf("%s:instruction-intent:%s:%s", repoID, family, shortHash)
}
