// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/dao"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// The personal lane of the per-repo scan (harness-improve): an engineer's OWN harness —
// their DESKTOP-source USER_GLOBAL and REPO_LOCAL artifacts — is a second detector input beside the
// repo's committed set, producing PERSON-scoped findings (Scope=PERSON, ScopeValue=person_id). It
// runs in the same per-repo Run, with no LLM leg: both lenses are plain Go over DB-only inputs.
//
// Ownership: a person's whole personal harness (every global AND every repo-local artifact, across
// all their repos) is evaluated in exactly ONE repo run per window — the run for their PRIMARY repo
// (ListPrimaryRepoPersons). The reconcile partition is (PERSON, person_id, artifact_type), which is
// person-wide, not per-repo: if two repo runs each wrote a slice of one person's partition, the later
// run's reconcile would clear the earlier run's is_latest and retract its still-valid findings. One
// owning run per person keeps every partition written by a single run per cycle. The bound this buys
// is the bound it costs: a repo-local artifact is conflict-checked against the committed set of the
// repo being SCANNED only, so a repo-local skill in a non-primary repo is never conflict-checked (its
// never-used check is repo-independent and still runs).
//
// Residual precondition: "exactly one owning run" holds only if the cycle's concurrent repo runs
// resolve the same primary repo per person. StartedAt is stamped per repo run, not once per cycle, so
// it anchors each run deterministically but is cycle-shared only when the fan-out completes within
// one UTC day and no scope is retried; a stagger slot crossing UTC midnight, or an runner-retry that
// re-fires only the failed scopes, can give two runs different windows and re-rank a near-tied person
// — leaving two is_latest PERSON rows until the next cycle's reconcile self-heals (PERSON findings are
// held dark, so no user impact). The proper fix — a single cycle-shared anchor stamped once before the
// fan-out, or one person→owning-repo map computed per org cycle — is deferred to the PERSON serving lane.

const (
	// personalFamilyNeverUsed flags an enabled, invocable personal artifact with zero in-window
	// invocations — a skill/command/agent/hook the engineer installed but never fires.
	personalFamilyNeverUsed = "personal-never-used"
	// personalFamilyShadowsCommitted flags a personal skill/command/agent whose canonical name
	// matches a committed artifact of the scanned repo but whose body differs — the engineer's
	// copy diverges from (and, at the tool's resolution, competes with) the team's.
	personalFamilyShadowsCommitted = "personal-shadows-committed"
)

// personalNeverUsedMinAgeDays is how long an artifact must have been observed before zero
// invocations reads as "never used" rather than "just installed". Measured from first observation
// (created_at) to the window end.
const personalNeverUsedMinAgeDays = 30

// personalAggregate is the personal lane's slice of the per-repo aggregate: the persons this repo
// run owns, their personal artifacts, the in-window invocation totals folded onto those artifacts,
// and the PERSON partitions currently live for them (the auto-close input).
type personalAggregate struct {
	// persons is the sorted set of person ids whose primary repo is the scanned repo — the persons
	// whose personal harness this run evaluates. Empty when no engineer's primary repo is this one.
	persons []string
	// artifacts is every live personal (DESKTOP, USER_GLOBAL/REPO_LOCAL) artifact of persons.
	artifacts []*model.HarnessArtifact
	// invocations maps an artifact id to its in-window invocation total, credited through the
	// resolved artifact_id AND through the unresolved inline identity (so a usage row the resolver
	// has not linked yet still counts as use — a false "used" is a silent non-finding, a false
	// "never used" is a wrong finding, so the fold errs toward used). Every artifact has a key.
	invocations map[string]int64
	// liveDetectedPartitions is the PERSON partitions live for persons before this run; unioned into
	// the evaluated set so a partition whose artifacts vanished auto-closes.
	liveDetectedPartitions []evaluatedPartition
	// windowEnd is the usage window's inclusive end (the run's StartedAt at UTC calendar midnight)
	// the never-used min-age is measured against.
	windowEnd time.Time
	// windowDays is the trailing window the invocation totals cover; surfaces in the finding copy.
	windowDays int
}

// personalUsageIdentity is the inline identity a still-PENDING usage row carries; matched by hand
// against a personal artifact's own identity columns.
type personalUsageIdentity struct {
	personID   string
	ideType    commonv1.IdeType
	sourcePath string
	userGlobal bool
}

// foldPersonalInvocations credits each personal artifact with its in-window invocations from both
// resolved (artifact_id) and unresolved (inline identity) usage rows. Every artifact gets a key, so a
// consumer reads a missing count as 0 without a presence check.
func foldPersonalInvocations(artifacts []*model.HarnessArtifact, summaries []dao.PersonalInvocationSummary) map[string]int64 {
	byID := make(map[string]int64, len(summaries))
	byIdentity := map[personalUsageIdentity]int64{}
	for _, s := range summaries {
		if s.ArtifactID != "" {
			byID[s.ArtifactID] += s.Invocations
			continue
		}
		byIdentity[personalUsageIdentity{personID: s.PersonID, ideType: s.IdeType, sourcePath: s.SourcePath, userGlobal: s.UserGlobal}] += s.Invocations
	}
	out := make(map[string]int64, len(artifacts))
	for _, a := range artifacts {
		n := byID[a.ID]
		if a.PersonID != nil {
			n += byIdentity[personalUsageIdentity{
				personID:   *a.PersonID,
				ideType:    a.IdeType,
				sourcePath: a.SourcePath,
				userGlobal: a.Scope == commonv1.HarnessArtifactScope_HARNESS_ARTIFACT_SCOPE_USER_GLOBAL,
			}]
		}
		out[a.ID] = n
	}
	return out
}

// isInvocableArtifact reports whether an artifact type is fired by the tool (and so leaves usage
// records) — the types the never-used lens can judge. An instruction file (CLAUDE.md / rule) is
// loaded, never invoked, so zero usage rows say nothing about it.
func isInvocableArtifact(a *model.HarnessArtifact) bool {
	switch a.Type {
	case commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL,
		commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_COMMAND,
		commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_AGENT,
		commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_HOOK:
		return true
	default:
		return false
	}
}

// isNameResolvedArtifact reports whether the tool resolves an artifact type BY NAME, so a personal
// and a committed artifact sharing a name compete for the same invocation — the types the
// shadows-committed lens can judge. Hooks key on event+matcher, not name, so they are excluded.
func isNameResolvedArtifact(a *model.HarnessArtifact) bool {
	return isBehavioralCandidate(a)
}

// personalFindingArtifactType maps a personal artifact's type onto the prompt_library artifact-type
// column: the committed map for every type it covers, plus HOOK (a personal never-used subject the
// committed detectors never have — no per-repo committed detector is about a hook).
func personalFindingArtifactType(t commonv1.HarnessArtifactType) promptlibraryv1.ArtifactType {
	if t == commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_HOOK {
		return promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK
	}
	return instructionArtifactType(t)
}

// personalLensSubject reports whether ANY personal lens can be about this artifact — the predicate
// that decides both which artifacts can produce a finding and which (person, type) partitions the
// run reports as evaluated. Kept as one function so a partition can never be reported evaluated for
// a type no lens reads (which would wrongly auto-close a standing finding).
func personalLensSubject(a *model.HarnessArtifact) bool {
	return isInvocableArtifact(a) || isNameResolvedArtifact(a)
}

// personalFindings runs the two deterministic personal lenses over the aggregate's personal
// artifacts and returns PERSON-scoped findings. It is PURE over the aggregate (no I/O).
//
// never-used: an ENABLED invocable artifact, first observed at least personalNeverUsedMinAgeDays
// before the window end, with zero in-window invocations.
//
// shadows-committed: a name-resolved artifact (skill/command/agent) that is USER_GLOBAL, or
// REPO_LOCAL in the scanned repo, whose canonical name matches a committed artifact of the same type
// in the scanned repo but whose body differs. Body equality is decided by current_file_id: bodies
// are content-addressed in harness_files, unique per (org, content_hash), so equal bodies share one
// file row and a differing id is a differing body — no the content store read needed. Identical content is a plain
// duplicate copy and is not flagged.
func personalFindings(orgID, repoID, taskExecutionID string, aggregate harnessImproveAggregate, aliases map[string]string) []*model.PromptLibraryItem {
	personal := aggregate.personal
	if len(personal.artifacts) == 0 {
		return nil
	}
	// The scanned repo's committed name-resolved artifacts by (type, canonical name) — the twin the
	// shadow lens looks up. Two committed artifacts of one (type, name) in one repo cannot coexist at
	// the tool's resolution either, so first-wins is a stable enough pick for the reference point.
	type committedKey struct {
		artifactType commonv1.HarnessArtifactType
		name         string
	}
	committedByName := map[committedKey]*model.HarnessArtifact{}
	for _, c := range aggregate.committed {
		if !isNameResolvedArtifact(c) {
			continue
		}
		key := committedKey{artifactType: c.Type, name: resolveCanonicalName(c.Name, aliases)}
		if _, seen := committedByName[key]; !seen {
			committedByName[key] = c
		}
	}
	minAgeCutoff := personal.windowEnd.AddDate(0, 0, -personalNeverUsedMinAgeDays)

	var findings []*model.PromptLibraryItem
	for _, a := range personal.artifacts {
		if a.PersonID == nil {
			// A person-less row cannot be personal (the DAO read filters person_id IN ?); skip rather
			// than mint a finding with an empty scope value.
			continue
		}
		personID := *a.PersonID
		if isInvocableArtifact(a) &&
			a.OperationalState == commonv1.HarnessOperationalState_HARNESS_OPERATIONAL_STATE_ENABLED &&
			!a.CreatedAt.IsZero() && !a.CreatedAt.After(minAgeCutoff) &&
			personal.invocations[a.ID] == 0 {
			findings = append(findings, newPersonalFinding(orgID, personID, taskExecutionID, a,
				promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_INSTRUCTION_INTENT, priorityLow,
				personalFamilyNeverUsed, "Personal artifact has not been used",
				fmt.Sprintf("0 invocations in %d days", personal.windowDays)))
		}
		if isNameResolvedArtifact(a) && personalArtifactInScannedRepo(a, repoID) {
			key := committedKey{artifactType: a.Type, name: resolveCanonicalName(a.Name, aliases)}
			if twin, ok := committedByName[key]; ok && twin.CurrentFileID != a.CurrentFileID {
				findings = append(findings, newPersonalFinding(orgID, personID, taskExecutionID, a,
					promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_CONFLICT, priorityMedium,
					personalFamilyShadowsCommitted, "Personal copy diverges from the team's committed version",
					twin.SourcePath))
			}
		}
	}
	return findings
}

// personalArtifactInScannedRepo reports whether a personal artifact is resolvable by the tool inside
// the scanned repo: a USER_GLOBAL artifact applies everywhere, a REPO_LOCAL one only in its own repo.
func personalArtifactInScannedRepo(a *model.HarnessArtifact, repoID string) bool {
	switch a.Scope {
	case commonv1.HarnessArtifactScope_HARNESS_ARTIFACT_SCOPE_USER_GLOBAL:
		return true
	case commonv1.HarnessArtifactScope_HARNESS_ARTIFACT_SCOPE_REPO_LOCAL:
		return a.RepositoryID != nil && *a.RepositoryID == repoID
	default:
		return false
	}
}

// newPersonalFinding builds one PERSON-scoped deterministic finding via newDeterministicFinding:
// Scope=PERSON, ScopeValue=personID, ArtifactType from the artifact, HarnessArtifactID=&artifact.ID
// (the attribution to the desktop row), and a person-partitioned slug.
func newPersonalFinding(orgID, personID, taskExecutionID string, artifact *model.HarnessArtifact,
	dimension promptlibraryv1.HarnessFindingDimension, priority int32, family, title, referencePoint string,
) *model.PromptLibraryItem {
	return newDeterministicFinding(orgID, taskExecutionID, dimension,
		promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PERSON, personID, personalFindingArtifactType(artifact.Type),
		&artifact.ID, priority, personalSlug(personID, family, artifact.ID), title, referencePoint,
		artifact.SourcePath, artifact.Name)
}

// personalSlug builds the stable finding slug "<personID>:personal:<family>:<hash>" where hash is the
// first 12 hex of sha256(artifactID + ":" + family) — stable per (artifact, family) so re-runs converge
// on the same slug, and person-prefixed so the suppression sets (keyed per scope value) stay disjoint.
func personalSlug(personID, family, artifactID string) string {
	sum := sha256.Sum256([]byte(artifactID + ":" + family))
	return fmt.Sprintf("%s:personal:%s:%s", personID, family, hex.EncodeToString(sum[:])[:12])
}

// personalEvaluationStatus returns, per owned person, the fully-evaluated PERSON partitions: one per
// (person, artifact_type) among the person's lens-subject artifacts, unioned with the person's live
// PERSON partitions (so a type whose artifacts vanished auto-closes). Both lenses are deterministic
// over DB-only inputs — no body, tree, or LLM leg can degrade — so every partition is always
// body-complete and never path- or behavioral-fed.
func personalEvaluationStatus(personal personalAggregate) map[string]evaluationStatus {
	partitions := map[string]map[promptlibraryv1.ArtifactType]struct{}{}
	for _, p := range personal.persons {
		partitions[p] = map[promptlibraryv1.ArtifactType]struct{}{}
	}
	for _, a := range personal.artifacts {
		if a.PersonID == nil || !personalLensSubject(a) {
			continue
		}
		types, owned := partitions[*a.PersonID]
		if !owned {
			continue
		}
		if t := personalFindingArtifactType(a.Type); t != promptlibraryv1.ArtifactType_ARTIFACT_TYPE_UNSPECIFIED {
			types[t] = struct{}{}
		}
	}
	for _, live := range personal.liveDetectedPartitions {
		if types, owned := partitions[live.ScopeValue]; owned && live.Scope == promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PERSON {
			types[live.ArtifactType] = struct{}{}
		}
	}
	out := make(map[string]evaluationStatus, len(partitions))
	for personID, types := range partitions {
		status := evaluationStatus{pathLensComplete: true, behavioralComplete: true}
		ordered := make([]promptlibraryv1.ArtifactType, 0, len(types))
		for t := range types {
			ordered = append(ordered, t)
		}
		sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
		for _, t := range ordered {
			status.scannedPartitions = append(status.scannedPartitions, scannedPartition{
				partition:    evaluatedPartition{Scope: promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PERSON, ScopeValue: personID, ArtifactType: t},
				bodyComplete: true,
			})
		}
		out[personID] = status
	}
	return out
}

// gatePersonalFindings runs the findings gate once per owned person — the gate's suppression reads are
// keyed by one (scope, scope_value), and each person is their own PERSON partition — and returns the
// concatenated persist set, evaluated partitions, and hidden reasons. No fix lane feeds a personal
// finding (both lenses are advisory), so fixes and repoPaths are empty and groundedFixes is discarded.
// Persons are visited in sorted order so the output is deterministic.
func (e *HarnessImproveEngine) gatePersonalFindings(ctx context.Context, orgID string, aggregate harnessImproveAggregate, findings []*model.PromptLibraryItem) (gated []*model.PromptLibraryItem, evaluated []evaluatedPartition, hiddenReasons []string, err error) {
	statusByPerson := personalEvaluationStatus(aggregate.personal)
	byPerson := map[string][]*model.PromptLibraryItem{}
	for _, finding := range findings {
		byPerson[finding.ScopeValue] = append(byPerson[finding.ScopeValue], finding)
	}
	for _, personID := range aggregate.personal.persons {
		status := statusByPerson[personID]
		personFindings := byPerson[personID]
		if len(personFindings) == 0 && len(status.scannedPartitions) == 0 {
			continue
		}
		personGated, personEvaluated, _, personHidden, gateErr := e.gateFindings(ctx, orgID, personID,
			promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PERSON, personFindings, nil, map[string]struct{}{}, status)
		if gateErr != nil {
			return nil, nil, nil, fmt.Errorf("gating personal findings for person %s: %w", personID, gateErr)
		}
		gated = append(gated, personGated...)
		evaluated = append(evaluated, personEvaluated...)
		hiddenReasons = append(hiddenReasons, personHidden...)
	}
	return gated, evaluated, hiddenReasons, nil
}
