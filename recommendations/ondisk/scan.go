// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package ondisk

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	taskexecutionsv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/taskexecutions/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations"
	"github.com/tetriz-ai/tetriz-harness/recommendations/dao"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// LocalOrgID is the synthetic single-tenant organization every on-disk scan runs under. The reader is
// single-tenant, so one fixed org id is enough — repos are distinguished by their own (deterministic)
// id, not by org.
const LocalOrgID = "0054da7a-0000-4000-8000-000000000001"

// repoNamespace is the fixed UUID namespace for deriving a stable repo id from a scanned directory's
// absolute path, so re-scanning the same directory lands on the same (org, repo) partition.
var repoNamespace = uuid.MustParse("0054da7a-0000-4000-8000-0000000000de")

// Options configure a scan. Dir is required; the rest default.
type Options struct {
	// Dir is the harness directory to scan (a repository root, or a user-global harness directory).
	Dir string
	// Scope is stamped on every classified artifact. Defaults to REPO_COMMITTED (a repository root — the
	// committed harness the per-repo detectors read).
	Scope commonv1.HarnessArtifactScope
	// OrgID / RepoID default to LocalOrgID and a hash of Dir's absolute path.
	OrgID  string
	RepoID string
	// DetectorConfig tunes the detectors (start from recommendations.DefaultDetectorConfig and override
	// the fields you need). Nil runs the built-in defaults.
	DetectorConfig *recommendations.DetectorConfig
}

// Finding is one persisted DETECTED finding, projected for presentation.
type Finding struct {
	Slug         string
	Title        string
	Dimension    string
	Scope        string
	ScopeValue   string
	SurfaceState string
	Source       string
	Priority     int32
	IsLatest     bool
}

// Result is the outcome of a scan: what was read and the DETECTED findings persisted.
type Result struct {
	OrgID         string
	RepoID        string
	ArtifactCount int
	RepoPathCount int
	Findings      []Finding
	// Coverage reports which detector families the scan ran and which it skipped for lack of input a
	// repository walk cannot supply, so a zero-finding Result is never mistaken for a clean bill across
	// every lens.
	Coverage Coverage
}

// SkippedFamily is one detector family the on-disk scan does not run, with the input it would need.
type SkippedFamily struct {
	Family string
	Needs  string
}

// Coverage is the honest coverage report of an on-disk scan: the detector families that ran against the
// committed harness on disk, and the families that were skipped because they need session, usage, or
// org-wide signals a repository walk does not provide.
type Coverage struct {
	Ran     []string
	Skipped []SkippedFamily
}

// onDiskCoverage is the fixed coverage of the on-disk DETECTED entry point. It is a static property of
// this entry point, not of what any one repo contains: the config-only detectors always run; the
// usage-informed and org-relational lenses always lack their input from a bare repo walk (no session
// data, no per-person usage, no org-wide aggregate), so they never contribute on disk. REPO_COMMITTED
// is the only scope wired in v1 (see Scan's and the package doc).
func onDiskCoverage() Coverage {
	return Coverage{
		Ran: []string{
			"Conflict (dup-id, dead-glob, broken-path, root-restatement)",
			"Instruction-Intent (placement and density over committed instruction files)",
			"Security permission inventory (grants and default-mode in a settings.json)",
		},
		Skipped: []SkippedFamily{
			{"Behavioral deviation", "per-skill usage behavior (session data) and LLM intent classification"},
			{"Personal / never-used", "per-person usage and primary-repo ownership"},
			{"Org MCP sprawl", "org-wide MCP usage (the org-relational scan, not wired on disk)"},
			{"Org coverage-gap", "org-wide signals (the org-relational scan, not wired on disk)"},
		},
	}
}

// Scan is the zero-setup DETECTED entry point: it reads the harness on disk (opts.Dir), seeds it into
// the harness-inventory tables, runs the config-only DETECTED per-repo scan (no LLM, no session/usage
// data), and returns the persisted findings. db must already carry the schema (the caller runs
// recommendations.RunMigrations, or a test loads the migrations into a transaction). The LLM legs are
// the no-op client, so the behavioral/fix legs degrade and only the config-only detectors contribute —
// exactly the stranger-points-it-at-a-repo story.
//
// The returned Result.Coverage names the detector families that ran and the ones skipped for lack of
// on-disk input, so a zero-finding scan is never read as a clean bill of health. Scan is idempotent:
// re-scanning the same directory re-seeds the (org, repo) partition in place and the findings reconcile
// (the latest run's findings stay is_latest).
//
// Scope limitation (v1): only REPO_COMMITTED harness is wired on disk. opts.Scope defaults to
// REPO_COMMITTED; a USER_GLOBAL / REPO_LOCAL personal harness directory can be seeded but the lenses
// that consume it (personal / never-used, personal-shadows-committed) need per-person usage data that a
// repository walk does not provide, so they yield nothing on disk today (a stated non-goal for v1, not a
// silent gap — see Result.Coverage).
func Scan(ctx context.Context, db *gorm.DB, opts Options) (*Result, error) {
	if opts.Dir == "" {
		return nil, fmt.Errorf("scan: Dir is required")
	}
	scope := opts.Scope
	if scope == commonv1.HarnessArtifactScope_HARNESS_ARTIFACT_SCOPE_UNSPECIFIED {
		scope = commonv1.HarnessArtifactScope_HARNESS_ARTIFACT_SCOPE_REPO_COMMITTED
	}
	abs, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("resolving harness dir %q: %w", opts.Dir, err)
	}
	orgID := opts.OrgID
	if orgID == "" {
		orgID = LocalOrgID
	}
	repoID := opts.RepoID
	if repoID == "" {
		repoID = uuid.NewSHA1(repoNamespace, []byte(abs)).String()
	}

	inv, err := Read(abs, scope)
	if err != nil {
		return nil, fmt.Errorf("reading harness dir: %w", err)
	}

	execDAO := dao.NewTaskExecutionRepo(db)
	detectorCfg := opts.DetectorConfig
	if detectorCfg == nil {
		defaults := recommendations.DefaultDetectorConfig()
		detectorCfg = &defaults
	}
	noop := recommendations.NewNoopLLMClient()
	engine := recommendations.NewHarnessImproveEngine(
		dao.NewGormTransactor(db),
		execDAO,
		dao.NewHarnessArtifactRepo(db),
		dao.NewRepositoryRepo(db),
		dao.NewSecurityFlagDAO(db),
		dao.NewPromptLibraryRepo(db),
		dao.NewHarnessFileRepo(db),
		dao.NewContentStore(db),
		dao.NewSessionInsightDAO(db),
		dao.NewHarnessUsageRecordsRepo(db),
		noop, // harnessIntentClassifier (degrades with no tetriz-py)
		noop, // harnessFixGenerator (degrades with no tetriz-py)
		detectorCfg,
		func(string) bool { return true },
	)

	exec := &model.TaskExecution{
		OrganizationID: orgID,
		TaskType:       taskexecutionsv1.TaskType_TASK_TYPE_HARNESS_IMPROVE,
		TriggerType:    taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_MANUAL,
		DedupKey:       repoID,
	}
	// Claim the partition before changing its input. A rejected run changes nothing, and a
	// failed seed rolls back both the replacement inventory and the new RUNNING row.
	if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		created, err := dao.NewTaskExecutionRepo(tx).CreateIfNoneRunning(ctx, exec)
		if err != nil {
			return fmt.Errorf("creating run row for repo %s: %w", repoID, err)
		}
		if !created {
			return fmt.Errorf("a scan for repo %s is already in flight", repoID)
		}
		if err := inv.Seed(ctx, tx, orgID, repoID); err != nil {
			return fmt.Errorf("seeding inventory: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	// Run acknowledges handled worker failures with nil. Scan is synchronous, so it must
	// inspect the terminal row rather than present old findings as a successful new scan.
	// If an early error leaves the row RUNNING, release its slot even when ctx was canceled.
	fail := func(cause error) error {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, cleanupErr := execDAO.MarkTerminalIfRunning(cleanupCtx, orgID, exec.ID,
			taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_FAILED,
			taskexecutionsv1.TaskExecutionSubStatus_TASK_EXECUTION_SUB_STATUS_UNSPECIFIED,
			time.Now().UTC(), cause.Error())
		if cleanupErr != nil {
			return errors.Join(cause, fmt.Errorf("releasing failed scan: %w", cleanupErr))
		}
		return cause
	}
	if err := engine.Run(ctx, orgID, taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_MANUAL, exec.ID); err != nil {
		return nil, fail(fmt.Errorf("running DETECTED scan for repo %s: %w", repoID, err))
	}
	terminal, err := execDAO.GetByID(ctx, orgID, exec.ID)
	if err != nil {
		return nil, fail(fmt.Errorf("reading scan outcome for repo %s: %w", repoID, err))
	}
	switch terminal.Status {
	case taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED_NO_VISIBLE:
		// Both are successful scans; the latter simply has no visible findings.
	default:
		detail := ""
		if terminal.ErrorMessage != nil {
			detail = ": " + *terminal.ErrorMessage
		}
		return nil, fail(fmt.Errorf("DETECTED scan for repo %s ended with %s%s", repoID, terminal.Status, detail))
	}

	findings, err := readDetectedFindings(ctx, db, orgID, repoID)
	if err != nil {
		return nil, fmt.Errorf("reading findings for repo %s: %w", repoID, err)
	}
	return &Result{
		OrgID:         orgID,
		RepoID:        repoID,
		ArtifactCount: len(inv.Artifacts),
		RepoPathCount: len(inv.RepoPaths),
		Findings:      findings,
		Coverage:      onDiskCoverage(),
	}, nil
}

// readDetectedFindings projects the LIVE persisted DETECTED prompt_library rows for (org, repo) into
// presentation findings, newest-ranked first (highest priority, then slug). "Live" matches the project's
// own definition in dao.ListLiveDetectedPartitions: is_latest AND surface_state VISIBLE — so a latest row
// that reconciled to HIDDEN is never surfaced. The reconcile keeps superseded rows in the table as
// history; the scan result and the CLI count/listing surface only the live partition, so re-scanning the
// same directory reports a stable finding set rather than one that grows by the live count each run.
func readDetectedFindings(ctx context.Context, db *gorm.DB, orgID, repoID string) ([]Finding, error) {
	var rows []struct {
		Slug                 string `gorm:"column:slug"`
		Title                string `gorm:"column:title"`
		RecommendationSource string `gorm:"column:recommendation_source"`
		Dimension            string `gorm:"column:dimension"`
		Scope                string `gorm:"column:scope"`
		ScopeValue           string `gorm:"column:scope_value"`
		SurfaceState         string `gorm:"column:surface_state"`
		Priority             int32  `gorm:"column:priority"`
		IsLatest             bool   `gorm:"column:is_latest"`
	}
	if err := db.WithContext(ctx).Raw(
		`SELECT slug, title, recommendation_source, dimension, scope, scope_value, surface_state, priority, is_latest
		   FROM prompt_library
		  WHERE organization_id = ?
		    AND scope_value = ?
		    AND recommendation_source = ?
		    AND is_latest = true
		    AND surface_state = ?
		    AND deleted_at IS NULL
		  ORDER BY priority DESC, slug ASC`,
		orgID, repoID,
		promptlibraryv1.ArtifactRecommendationSource_ARTIFACT_RECOMMENDATION_SOURCE_DETECTED.String(),
		promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE.String(),
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	findings := make([]Finding, 0, len(rows))
	for _, r := range rows {
		findings = append(findings, Finding{
			Slug:         r.Slug,
			Title:        r.Title,
			Dimension:    r.Dimension,
			Scope:        r.Scope,
			ScopeValue:   r.ScopeValue,
			SurfaceState: r.SurfaceState,
			Source:       r.RecommendationSource,
			Priority:     r.Priority,
			IsLatest:     r.IsLatest,
		})
	}
	return findings, nil
}
