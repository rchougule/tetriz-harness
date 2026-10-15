// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package model holds the GORM row models the recommendations engine reads and writes. Enum
// columns bind to proto enums stored as the full value-name string; JSONB columns bind to proto
// messages. The scanner/valuer wiring is provided by the DAO layer; these structs are the shapes.
//
// Several models are a read-subset of a larger upstream table — only the columns this engine reads
// or writes are carried; see each struct's doc.
package model

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	"gorm.io/gorm"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	harnesspb "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/harness/v1"
	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	taskexecutionsv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/taskexecutions/v1"
)

// ArtifactSlugRef is an (artifact_type, slug) pair identifying an artifact by its stable identity
// rather than its row. The mechanical suppressions key on the pair, not slug alone, so an artifact
// of one type cannot suppress a same-slug artifact of another — consistent with the per-type
// is_latest lifecycle (reconcileLatest buckets by artifact_type).
type ArtifactSlugRef struct {
	ArtifactType promptlibraryv1.ArtifactType `gorm:"column:artifact_type;serializer:protoenum"`
	Slug         string                       `gorm:"column:slug"`
}

// PromptLibraryItem is the GORM model for the prompt_library table — one artifact (a DERIVED
// recommendation or a DETECTED finding) for an org, mirroring the table 1:1. Enum columns bind to
// proto enums (stored as the value-name string); the JSONB columns (artifact_content, evidence,
// admin_interaction) bind to proto messages. All three JSONB columns are NOT NULL — the write path
// fills a non-nil empty message for any the caller left nil.
type PromptLibraryItem struct {
	ID             string `gorm:"column:id;type:uuid;default:gen_random_uuid();primaryKey"`
	OrganizationID string `gorm:"column:organization_id;type:uuid;not null"`

	ArtifactType promptlibraryv1.ArtifactType  `gorm:"column:artifact_type;not null;serializer:protoenum"`
	Scope        promptlibraryv1.ArtifactScope `gorm:"column:scope;not null;serializer:protoenum"`
	// ScopeValue is the immutable repository id when Scope=PROJECT, empty for org scope. Stored as
	// text (not a uuid column), so empty is valid.
	ScopeValue string `gorm:"column:scope_value"`
	// TaskType is the free-text template label ("debug", "test", …), not a closed enum — the proto
	// contract is `string task_type`.
	// allow:string-enum — free-text template label per proto contract, not an enum
	TaskType string `gorm:"column:task_type"`

	Title string `gorm:"column:title;not null"`
	// Slug is the artifact's stable one-word identity, distinct from the descriptive Title:
	// kebab-case for rules/templates, "/"-prefixed for skills. It is the dedup/suppression key;
	// normalized on persist and never empty on a VISIBLE row.
	Slug            string                              `gorm:"column:slug;not null"`
	ArtifactContent *promptlibraryv1.ContentRef         `gorm:"column:artifact_content;type:jsonb;not null;serializer:protojson"`
	Evidence        *promptlibraryv1.EvidenceProjection `gorm:"column:evidence;type:jsonb;not null;serializer:protojson"`

	Priority int32 `gorm:"column:priority;not null"`
	// ConfidenceScore mirrors the proto float (0.0-1.0); the column is double precision.
	ConfidenceScore float32 `gorm:"column:confidence_score"`

	// IsLatest is derived, not caller-set: the write path recomputes it after every write so it
	// marks exactly the run owning the most-recent VISIBLE artifact for an (org, scope, scope_value).
	// No gorm `default:true` tag on purpose — GORM omits a zero-value field that has a default tag
	// from the INSERT, so an explicit is_latest=false would silently persist as the column DEFAULT.
	IsLatest         bool                                 `gorm:"column:is_latest;not null"`
	ClientStatus     promptlibraryv1.ArtifactClientStatus `gorm:"column:client_status;not null;serializer:protoenum"`
	SurfaceState     promptlibraryv1.ArtifactSurfaceState `gorm:"column:surface_state;not null;serializer:protoenum"`
	AdminInteraction *promptlibraryv1.AdminInteraction    `gorm:"column:admin_interaction;type:jsonb;not null;serializer:protojson"`

	// TaskExecutionID is the run that produced this artifact (app-level reference, no DB constraint).
	TaskExecutionID string `gorm:"column:task_execution_id;type:uuid"`

	// RecommendationSource / Dimension are the finding provenance + lens. Pointer-typed: the columns
	// are nullable; a nil pointer reads SQL NULL without invoking the enum scanner. nil on a row with
	// no explicit provenance.
	RecommendationSource *promptlibraryv1.ArtifactRecommendationSource `gorm:"column:recommendation_source;serializer:protoenum"`
	Dimension            *promptlibraryv1.HarnessFindingDimension      `gorm:"column:dimension;serializer:protoenum"`
	// HarnessArtifactID is the committed harness_artifacts row a modification finding is about
	// (app-level ref, no FK). nil for a repo-level or non-detector row; a pointer because empty
	// string is not valid for a uuid column.
	HarnessArtifactID *string `gorm:"column:harness_artifact_id;type:uuid"`

	CreatedAt time.Time      `gorm:"column:created_at"`
	UpdatedAt time.Time      `gorm:"column:updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"column:deleted_at"`
}

// TableName returns the database table name for GORM.
func (PromptLibraryItem) TableName() string { return "prompt_library" }

// MemberVisibleScopes is the allowlist of scopes the org-facing surfaces may serve. A scope absent
// here (PERSON: one engineer's own findings) is hidden from members. The list and the by-id
// predicate share this one definition so the set reachable by id never exceeds the list.
var MemberVisibleScopes = []promptlibraryv1.ArtifactScope{
	promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_ORG,
	promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT,
}

// IsMemberVisibleScope reports whether scope is in MemberVisibleScopes.
func IsMemberVisibleScope(scope promptlibraryv1.ArtifactScope) bool {
	for _, allowed := range MemberVisibleScopes {
		if scope == allowed {
			return true
		}
	}
	return false
}

// IsDetectedFinding reports whether this row is a DETECTED finding (vs a DERIVED recommendation).
// Null-safe — a nil RecommendationSource is not DETECTED.
func (i *PromptLibraryItem) IsDetectedFinding() bool {
	return i.RecommendationSource != nil &&
		*i.RecommendationSource == promptlibraryv1.ArtifactRecommendationSource_ARTIFACT_RECOMMENDATION_SOURCE_DETECTED
}

// Surfaceable reports whether the artifact is part of the live dashboard surface: VISIBLE AND
// (PINNED OR (is_latest AND not dismissed)), excluding DETECTED findings (kept dark on the member
// surface). The single predicate for "reachable by id".
func (i *PromptLibraryItem) Surfaceable() bool {
	if i.IsDetectedFinding() {
		return false
	}
	return i.SurfaceableIgnoringSource()
}

// SurfaceableIgnoringSource applies the surface_state / is_latest / pinned / dismissed contract
// WITHOUT the DETECTED-source exclusion, so an un-darked finding is reachable on the same visibility
// contract as a derived row.
func (i *PromptLibraryItem) SurfaceableIgnoringSource() bool {
	if i.SurfaceState != promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE {
		return false
	}
	if !IsMemberVisibleScope(i.Scope) {
		return false
	}
	if i.ClientStatus == promptlibraryv1.ArtifactClientStatus_ARTIFACT_CLIENT_STATUS_PINNED {
		return true
	}
	// DISMISSED is terminal — a dismissed row drops off the by-id surface.
	return i.IsLatest &&
		i.ClientStatus != promptlibraryv1.ArtifactClientStatus_ARTIFACT_CLIENT_STATUS_DISMISSED
}

// ToProto maps the row to its wire entity. The JSONB proto fields pass through directly;
// deleted_at is emitted only when the soft-delete is set.
func (p *PromptLibraryItem) ToProto() *promptlibraryv1.PromptLibraryItem {
	if p == nil {
		return nil
	}
	out := &promptlibraryv1.PromptLibraryItem{
		Id:               p.ID,
		OrganizationId:   p.OrganizationID,
		ArtifactType:     p.ArtifactType,
		Scope:            p.Scope,
		ScopeValue:       p.ScopeValue,
		TaskType:         p.TaskType,
		Title:            p.Title,
		Slug:             p.Slug,
		ArtifactContent:  p.ArtifactContent,
		Evidence:         p.Evidence,
		Priority:         p.Priority,
		ConfidenceScore:  p.ConfidenceScore,
		IsLatest:         p.IsLatest,
		ClientStatus:     p.ClientStatus,
		SurfaceState:     p.SurfaceState,
		AdminInteraction: p.AdminInteraction,
		TaskExecutionId:  p.TaskExecutionID,
		CreatedAt:        timestamppb.New(p.CreatedAt),
		UpdatedAt:        timestamppb.New(p.UpdatedAt),
	}
	if p.RecommendationSource != nil {
		out.RecommendationSource = *p.RecommendationSource
	}
	if p.Dimension != nil {
		out.Dimension = *p.Dimension
	}
	if p.HarnessArtifactID != nil {
		out.HarnessArtifactId = *p.HarnessArtifactID
	}
	if p.DeletedAt.Valid {
		out.DeletedAt = timestamppb.New(p.DeletedAt.Time)
	}
	return out
}

// TaskExecution is the GORM model for the task_executions table — a generic audit row for a
// scheduled / manually-triggered run, discriminated by task_type. Enum columns bind to proto enums;
// input_payload / output_payload bind to the ContentRef proto.
type TaskExecution struct {
	ID             string                               `gorm:"column:id;type:uuid;default:gen_random_uuid();primaryKey"`
	OrganizationID string                               `gorm:"column:organization_id;type:uuid;not null"`
	TaskType       taskexecutionsv1.TaskType            `gorm:"column:task_type;not null;serializer:protoenum"`
	Status         taskexecutionsv1.TaskExecutionStatus `gorm:"column:status;not null;serializer:protoenum"`
	// SubStatus is the fine reason within Status; UNSPECIFIED when a terminal needs no sub-reason.
	SubStatus   taskexecutionsv1.TaskExecutionSubStatus `gorm:"column:substatus;not null;serializer:protoenum"`
	TriggerType taskexecutionsv1.TaskTriggerType        `gorm:"column:trigger_type;not null;serializer:protoenum"`
	TriggeredBy *string                                 `gorm:"column:triggered_by;type:uuid"`
	// DedupKey is the single-in-flight identity (the repository id for a per-repo run); empty for
	// runs that do not serialize on one.
	DedupKey      string                       `gorm:"column:dedup_key"`
	InputPayload  *taskexecutionsv1.ContentRef `gorm:"column:input_payload;type:jsonb;not null;serializer:protojson"`
	OutputPayload *taskexecutionsv1.ContentRef `gorm:"column:output_payload;type:jsonb;serializer:protojson"`
	// ErrorMessage is *string so a non-FAILED run stores SQL NULL, not "" — the contract is that a
	// non-NULL error_message means exactly failures. Set (non-nil) only on a FAILED run.
	ErrorMessage *string `gorm:"column:error_message"`

	StartedAt   time.Time      `gorm:"column:started_at;not null"`
	CompletedAt *time.Time     `gorm:"column:completed_at"`
	CreatedAt   time.Time      `gorm:"column:created_at"`
	UpdatedAt   time.Time      `gorm:"column:updated_at"`
	DeletedAt   gorm.DeletedAt `gorm:"column:deleted_at"`
}

// TableName returns the database table name for GORM.
func (TaskExecution) TableName() string { return "task_executions" }

// HarnessFile is a content-addressed file body, shared across every engineer who carries that
// content. Status binds to the HarnessFileStatus proto enum stored as its name string. One row per
// (org, content_hash). Content holds the body inline.
type HarnessFile struct {
	ID             string                     `gorm:"column:id;type:uuid;default:gen_random_uuid();primaryKey"`
	OrganizationID string                     `gorm:"column:organization_id;type:uuid;not null"`
	ContentHash    string                     `gorm:"column:content_hash;not null"`
	ByteSize       int64                      `gorm:"column:byte_size"`
	Status         commonv1.HarnessFileStatus `gorm:"column:status;not null;serializer:protoenum"`
	Content        []byte                     `gorm:"column:content"`
	CreatedAt      time.Time                  `gorm:"column:created_at"`
}

// TableName returns the database table name for GORM.
func (HarnessFile) TableName() string { return "harness_files" }

// HarnessArtifact is the observed-artifact inventory row, discriminated by ArtifactSource (VCS =
// org-canonical/person-less; DESKTOP = per-person). The detectors read the VCS rows. This is a
// read-subset of the upstream table — only the columns this engine reads are carried. Enum columns
// bind to their proto enum name string; Files is a proto-message JSONB manifest.
type HarnessArtifact struct {
	ID             string `gorm:"column:id;type:uuid;default:gen_random_uuid();primaryKey"`
	OrganizationID string `gorm:"column:organization_id;type:uuid;not null"`
	// PersonID is the observing engineer for a DESKTOP row; NULL for a VCS row (org-grain,
	// person-less). A pointer so GORM writes SQL NULL for the person-less case.
	PersonID *string `gorm:"column:person_id;type:uuid"`

	IdeType commonv1.IdeType              `gorm:"column:ide_type;not null;serializer:protoenum"`
	Scope   commonv1.HarnessArtifactScope `gorm:"column:scope;not null;serializer:protoenum"`
	Type    commonv1.HarnessArtifactType  `gorm:"column:type;not null;serializer:protoenum"`
	Name    string                        `gorm:"column:name;not null"`

	// RepositoryID is the resolved repository id; NULL only for USER_GLOBAL scope.
	RepositoryID *string `gorm:"column:repository_id;type:uuid"`
	SourcePath   string  `gorm:"column:source_path;not null"`

	CurrentFileID string `gorm:"column:current_file_id;type:uuid;not null"`

	OperationalState commonv1.HarnessOperationalState `gorm:"column:operational_state;not null;serializer:protoenum"`

	// ArtifactSource discriminates a DESKTOP row (per-engineer) from a VCS row (org-grain). PersonID
	// is NULL iff this is VCS.
	ArtifactSource commonv1.HarnessArtifactSource `gorm:"column:artifact_source;not null;serializer:protoenum"`

	// Files is the per-file manifest of a directory artifact — the primary plus every helper. Proto
	// message JSONB. The `default` tag makes GORM omit a nil manifest on insert so the column default
	// fires for a single-file artifact.
	Files *harnesspb.HarnessArtifactFileSet `gorm:"column:files;type:jsonb;default:'{}';serializer:protojson"`
	// ContentGeneration is the artifact-level resurrection guard. 0 for a DESKTOP row.
	ContentGeneration int64 `gorm:"column:content_generation;not null"`

	CreatedAt time.Time      `gorm:"column:created_at"`
	DeletedAt gorm.DeletedAt `gorm:"column:deleted_at"`
}

// TableName returns the database table name for GORM.
func (HarnessArtifact) TableName() string { return "harness_artifacts" }

// FileRefs returns this artifact's stored manifest entries, nil-safe (a nil artifact or a nil Files
// set yields no entries).
func (a *HarnessArtifact) FileRefs() []*harnesspb.HarnessArtifactFileRef {
	if a == nil || a.Files == nil {
		return nil
	}
	return a.Files.GetFiles()
}

// Repository is the per-repo registry (read-subset). TreeContent holds the paths-only repo tree the
// reader fills; TreeKey is the legacy tree reference, read by the DERIVED in-scope gate
// (ListInScopeProjects) to tell whether a repo has a fetched tree to ground against.
type Repository struct {
	ID              string  `gorm:"column:id;type:uuid;default:gen_random_uuid();primaryKey"`
	OrganizationID  string  `gorm:"column:organization_id;type:uuid;not null"`
	ConnectorID     string  `gorm:"column:connector_id"`
	PlatformRepoID  string  `gorm:"column:platform_repo_id"`
	Name            string  `gorm:"column:name"`
	FullName        string  `gorm:"column:full_name"`
	PrimaryLanguage *string `gorm:"column:primary_language"`
	Archived        bool    `gorm:"column:archived;not null"`
	TreeKey         *string `gorm:"column:tree_key"`
	TreeContent     *string `gorm:"column:tree_content"`

	CreatedAt time.Time      `gorm:"column:created_at"`
	UpdatedAt time.Time      `gorm:"column:updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"column:deleted_at"`
}

// TableName returns the database table name for GORM.
func (Repository) TableName() string { return "vcs_repositories" }

// VCSRepoProfile carries per-repo policy context (read-subset). MergeEnforcement is a text label
// (the upstream enum's value-name string); a missing row is non-fatal to the engine. The table is a
// thin policy map keyed by the composite (organization_id, repo_id) — no surrogate id, no timestamps,
// no soft-delete (see the migration), so the model carries none either.
type VCSRepoProfile struct {
	OrganizationID   string  `gorm:"column:organization_id;type:uuid;not null;primaryKey"`
	RepoID           string  `gorm:"column:repo_id;type:uuid;not null;primaryKey"`
	MergeEnforcement *string `gorm:"column:merge_enforcement"`
}

// TableName returns the database table name for GORM.
func (VCSRepoProfile) TableName() string { return "vcs_repo_profile" }

// SessionTaskTypeSummary is one per-task-type aggregate row (mean dimension scores over the
// window). Plain scan target for a GROUP BY aggregate, not a table model.
type SessionTaskTypeSummary struct {
	TaskType         string  `gorm:"column:task_type"`
	Sessions         int     `gorm:"column:sessions"`
	AvgOverall       float64 `gorm:"column:avg_overall"`
	Clarity          float64 `gorm:"column:clarity"`
	Specificity      float64 `gorm:"column:specificity"`
	ContextProvision float64 `gorm:"column:context_provision"`
	Actionability    float64 `gorm:"column:actionability"`
	Completeness     float64 `gorm:"column:completeness"`
	Efficiency       float64 `gorm:"column:efficiency"`
}
