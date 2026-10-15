// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Command recommendations runs the harness recommendation engines over a set of
// (organization, repository) targets: the DERIVED per-repo pipeline (Engine.Run), the DETECTED
// per-repo committed-harness scan (HarnessImproveEngine.Run), and the org-relational DETECTED scan
// once per organization (HarnessImproveEngine.RunOrgScan). It runs the engines sequentially over the
// given targets — a plain command-line driver, with no scheduler or background fan-out of its own.
//
// It is standalone: a local Postgres (--dsn) is the only hard dependency. The LLM is optional — with no
// --tetriz-py-url the DERIVED/behavioral/fix legs degrade (a no-op client), and the config-only DETECTED
// detectors still run and persist findings, which is the zero-setup on-disk demo.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	taskexecutionsv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/taskexecutions/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations"
	"github.com/tetriz-ai/tetriz-harness/recommendations/dao"
	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/config"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
	"github.com/tetriz-ai/tetriz-harness/recommendations/ondisk"
)

// target is one organization/repository the engines run for.
type target struct {
	orgID  string
	repoID string
}

// engines bundles the constructed engines plus the run-lifecycle DAO the loop uses to mint each run's
// task_executions row before the engine resumes it.
type engines struct {
	derive  *recommendations.Engine
	improve *recommendations.HarnessImproveEngine
	execDAO dao.ITaskExecutionDAO
}

// llmLegs is the union of the three LLM seams the engines consume. Both the real LLM client and the no-op
// satisfy it, so one variable selects the mode and feeds all three engine params.
type llmLegs interface {
	DeriveArtifacts(ctx context.Context, aggCtx *promptlibraryv1.AggregatedContext, orgID, sourceID string) (*recommendations.DerivationResult, []byte, error)
	ClassifyHarnessIntent(ctx context.Context, orgID, sourceID string, input recommendations.HarnessIntentInput) (recommendations.HarnessIntentResult, error)
	GenerateHarnessFix(ctx context.Context, orgID, sourceID string, input recommendations.HarnessFixInput) (recommendations.HarnessFixResult, error)
}

// usage prints the top-level help. It names the `scan` subcommand — the quickstart command — which the
// default flag usage alone would never mention, then lists the flags for the default org/repos run.
func usage() {
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  recommendations [flags]       run the DERIVED + DETECTED engines over --org/--repos")
	fmt.Fprintln(os.Stderr, "  recommendations scan [flags]  scan a committed harness on disk (see 'recommendations scan --help')")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Flags for the default org/repos run:")
	flag.PrintDefaults()
}

func main() {
	orgID := flag.String("org", "", "organization id to scan")
	repos := flag.String("repos", "", "comma-separated repository ids to scan")
	dsn := flag.String("dsn", "", "PostgreSQL DSN (e.g. host=localhost port=5432 user=... dbname=... sslmode=disable)")
	pyURL := flag.String("tetriz-py-url", "", "tetriz-py base URL; empty runs with a no-op LLM (DETECTED config-only)")
	flag.Usage = usage

	// A non-flag first arg is a subcommand. `scan` is the zero-setup DETECTED on-disk entry point: point
	// it at a harness directory and get findings, with no org/repo ids and no session data upstream. Any
	// other non-flag first arg is a mistake (e.g. a typo of `scan`) — reject it with usage rather than
	// silently falling through to the default org/repos run and emitting a misleading "--org ... required".
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		if os.Args[1] == "scan" {
			if err := runScan(os.Args[2:]); err != nil {
				log.Fatalf("scan: %v", err)
			}
			return
		}
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}

	flag.Parse()

	if *orgID == "" || *repos == "" || *dsn == "" {
		log.Fatal("--org, --repos and --dsn are required")
	}

	var targets []target
	for _, repoID := range strings.Split(*repos, ",") {
		repoID = strings.TrimSpace(repoID)
		if repoID == "" {
			continue
		}
		targets = append(targets, target{orgID: *orgID, repoID: repoID})
	}

	ctx := context.Background()
	built, err := buildEngines(*dsn, *pyURL)
	if err != nil {
		log.Fatalf("building engines: %v", err)
	}
	if err := run(ctx, built, targets); err != nil {
		log.Fatalf("running recommendations: %v", err)
	}
}

// runScan is the `recommendations scan` subcommand: it reads a harness directory on disk, seeds it into
// a local Postgres, runs the config-only DETECTED detectors (no LLM, no session data), and prints the
// findings. It is the stranger-points-it-at-a-repo story — no org/repo ids needed.
func runScan(args []string) error {
	flags := flag.NewFlagSet("scan", flag.ExitOnError)
	harnessDir := flags.String("harness-dir", "", "harness directory to scan (a repository root, or a user-global harness dir)")
	dsn := flags.String("dsn", "", "PostgreSQL DSN for the scratch inventory (e.g. host=localhost port=5432 user=... dbname=... sslmode=disable)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *harnessDir == "" || *dsn == "" {
		return fmt.Errorf("--harness-dir and --dsn are required")
	}

	db, err := openDB(*dsn)
	if err != nil {
		return err
	}
	if err := recommendations.RunMigrations(db); err != nil {
		return fmt.Errorf("running migrations: %w", err)
	}

	result, err := ondisk.Scan(context.Background(), db, ondisk.Options{Dir: *harnessDir})
	if err != nil {
		return err
	}

	fmt.Printf("scanned %s: %d harness artifacts over %d files; %d DETECTED finding(s)\n",
		*harnessDir, result.ArtifactCount, result.RepoPathCount, len(result.Findings))
	for _, f := range result.Findings {
		// ondisk.Scan surfaces only the live partition, so every finding here is live — the [live] marker
		// is unconditional (kept to match the README listing).
		fmt.Printf("  - [%s] %s (priority %d) [live]\n    %s\n", f.Dimension, f.Title, f.Priority, f.Slug)
	}

	// Always report coverage, so a zero-finding scan reads as "the config-only detectors found nothing",
	// never as a clean bill of health across every lens.
	fmt.Println("\ndetector coverage:")
	fmt.Println("  ran (config-only, over the committed harness on disk):")
	for _, family := range result.Coverage.Ran {
		fmt.Printf("    - %s\n", family)
	}
	fmt.Println("  skipped (need session/usage/org data a repository walk does not provide):")
	for _, skipped := range result.Coverage.Skipped {
		fmt.Printf("    - %s: needs %s\n", skipped.Family, skipped.Needs)
	}
	return nil
}

// run drives both engines over every target: for each repository the DERIVED pipeline and the DETECTED
// per-repo scan, then the org-relational DETECTED scan once per organization. Each engine run is preceded
// by CreateIfNoneRunning, which mints the RUNNING task_executions row (keyed by the partition's dedup_key)
// the engine resumes by id. A run whose single-in-flight slot is already held (created=false) is skipped.
func run(ctx context.Context, e engines, targets []target) error {
	scannedOrg := map[string]bool{}
	for _, t := range targets {
		if err := startAndRun(ctx, e.execDAO, t.orgID, taskexecutionsv1.TaskType_TASK_TYPE_DERIVE_ORG_ARTIFACTS, t.repoID,
			func(execID string) error {
				return e.derive.Run(ctx, t.orgID, taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_MANUAL, execID, false)
			}); err != nil {
			return fmt.Errorf("derive run for org %s repo %s: %w", t.orgID, t.repoID, err)
		}
		if err := startAndRun(ctx, e.execDAO, t.orgID, taskexecutionsv1.TaskType_TASK_TYPE_HARNESS_IMPROVE, t.repoID,
			func(execID string) error {
				return e.improve.Run(ctx, t.orgID, taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_MANUAL, execID)
			}); err != nil {
			return fmt.Errorf("harness-improve run for org %s repo %s: %w", t.orgID, t.repoID, err)
		}
		if scannedOrg[t.orgID] {
			continue
		}
		if err := startAndRun(ctx, e.execDAO, t.orgID, taskexecutionsv1.TaskType_TASK_TYPE_HARNESS_IMPROVE, t.orgID,
			func(execID string) error {
				return e.improve.RunOrgScan(ctx, t.orgID, "cli", execID)
			}); err != nil {
			return fmt.Errorf("harness-improve org scan for org %s: %w", t.orgID, err)
		}
		scannedOrg[t.orgID] = true
	}
	return nil
}

// startAndRun mints the RUNNING task_executions row for (org, taskType, dedupKey) and, when it won the
// single-in-flight slot, invokes the engine with the minted id. A lost slot (created=false) is skipped.
func startAndRun(ctx context.Context, execDAO dao.ITaskExecutionDAO, orgID string, taskType taskexecutionsv1.TaskType, dedupKey string, invoke func(execID string) error) error {
	exec := &model.TaskExecution{
		OrganizationID: orgID,
		TaskType:       taskType,
		TriggerType:    taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_MANUAL,
		DedupKey:       dedupKey,
	}
	created, err := execDAO.CreateIfNoneRunning(ctx, exec)
	if err != nil {
		return fmt.Errorf("creating run row (task_type %s dedup_key %s): %w", taskType, dedupKey, err)
	}
	if !created {
		log.Printf("skipping %s for dedup_key %s: a run is already in flight", taskType, dedupKey)
		return nil
	}
	return invoke(exec.ID)
}

// buildEngines opens the database, applies the bundled migrations, constructs every DAO + the content
// store + the transactor + the LLM client (no-op unless a tetriz-py URL is given), and wires the DERIVED
// and DETECTED engines. The aggregator always takes a concrete LLM client (it is only exercised on the
// DERIVED/review-theme paths); the deriver + intent/fix seams select the no-op when no URL is set.
func buildEngines(dsn, pyURL string) (engines, error) {
	db, err := openDB(dsn)
	if err != nil {
		return engines{}, err
	}
	if err := recommendations.RunMigrations(db); err != nil {
		return engines{}, fmt.Errorf("running migrations: %w", err)
	}

	libraryDAO := dao.NewPromptLibraryRepo(db)
	execDAO := dao.NewTaskExecutionRepo(db)
	harnessArtifactDAO := dao.NewHarnessArtifactRepo(db)
	fileDAO := dao.NewHarnessFileRepo(db)
	repoDAO := dao.NewRepositoryRepo(db)
	repoProfileDAO := dao.NewVCSRepoProfileRepo(db)
	securityDAO := dao.NewSecurityFlagDAO(db)
	sessionDAO := dao.NewSessionInsightDAO(db)
	usageDAO := dao.NewHarnessUsageRecordsRepo(db)
	pullRequestDAO := dao.NewPullRequestRepo(db)
	rawRecordDAO := dao.NewRawRecordRepo(db)
	platformIdentityDAO := dao.NewPlatformIdentityMapRepo(db)
	contentStore := dao.NewContentStore(db)
	transactor := dao.NewGormTransactor(db)

	deriveCfg := defaultDeriveConfig()
	detectorCfg := config.DefaultDetectorConfig()

	// The aggregator needs a concrete *LLMClient regardless of mode; with an empty URL it only errors if
	// a DERIVED/review-theme path actually calls it (the DETECTED demo never does).
	llm := recommendations.NewLLMClient(pyURL, func() int { return 20 }, func() int { return 60 })
	var legs llmLegs = recommendations.NewNoopLLMClient()
	if pyURL != "" {
		legs = llm
	}

	aggregator := recommendations.NewAggregator(
		sessionDAO, pullRequestDAO, rawRecordDAO, securityDAO, repoDAO, repoProfileDAO, libraryDAO,
		llm, platformIdentityDAO, func() recommendations.DerivationParams { return deriveParams(deriveCfg) },
	)
	qualityGate := recommendations.NewQualityGate(
		libraryDAO,
		func() int { return deriveCfg.HiddenDedupRuns },
		func() int { return deriveCfg.MinLowBucket },
		func() int { return deriveCfg.MinHighBucket },
		func() int { return deriveCfg.MaxHooksPerRun },
		func() int { return deriveCfg.MaxAgentsPerRun },
	)

	flagEnabled := func(string) bool { return true }
	derive := recommendations.NewEngine(
		transactor, aggregator, legs, qualityGate, libraryDAO, execDAO, harnessArtifactDAO, contentStore,
		recommendations.NewNoopAnalyticsEmitter(),
		func() int { return deriveCfg.DataWindowDays },
		func() int { return deriveCfg.MinReviewThemesPerRepo },
		flagEnabled,
		func(string) bool { return true },
	)
	improve := recommendations.NewHarnessImproveEngine(
		transactor, execDAO, harnessArtifactDAO, repoDAO, securityDAO, libraryDAO, fileDAO, contentStore,
		sessionDAO, usageDAO, legs, legs, &detectorCfg, flagEnabled,
	)
	return engines{derive: derive, improve: improve, execDAO: execDAO}, nil
}

// openDB opens the CLI's Postgres connection with GORM's colored query logger turned off, so a
// connection failure surfaces once as a plain wrapped error rather than GORM's ANSI-colored,
// absolute-source-path log line printed alongside the wrapped one. The DSN is never put in the error
// because it can carry a password.
func openDB(dsn string) (*gorm.DB, error) {
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		return nil, fmt.Errorf("connecting to Postgres: %w", err)
	}
	return db, nil
}

// defaultDeriveConfig is a neutral DERIVED-flow config for the standalone CLI.
func defaultDeriveConfig() config.DeriveConfig {
	return config.DeriveConfig{
		LowScoreMax:               4,
		HighScoreMin:              7,
		MaxReviewThemesPerComment: 3,
		DataWindowDays:            30,
		HiddenDedupRuns:           5,
		MaxHooksPerRun:            5,
		MaxAgentsPerRun:           5,
	}
}

// deriveParams projects the DERIVED config into the aggregator's per-run params.
func deriveParams(cfg config.DeriveConfig) recommendations.DerivationParams {
	return recommendations.DerivationParams{
		LowScoreMax:                cfg.LowScoreMax,
		HighScoreMin:               cfg.HighScoreMin,
		RepoTreeMaxTokens:          cfg.RepoTreeMaxTokens,
		MaxReviewBodies:            cfg.MaxReviewBodies,
		ReviewClassifyLookbackDays: cfg.ReviewClassifyLookbackDays,
		ReviewClassifyMaxComments:  cfg.ReviewClassifyMaxComments,
		MinReviewCommentsPerRepo:   cfg.MinReviewCommentsPerRepo,
		MaxReviewThemesPerComment:  cfg.MaxReviewThemesPerComment,
	}
}
