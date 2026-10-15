// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

import (
	"context"
	"testing"

	"gorm.io/gorm"

	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/testsupport"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// These are real-Postgres tests for the is_latest reconcile — the highest-risk piece of this engine.
// They prove: (1) a re-run promotes the new generation and demotes the prior one within its partition
// while a DIFFERENT partition is untouched; (2) a DETECTED reconcile never clears a DERIVED row's
// is_latest and vice versa; (3) ListVisibleByOrg dark-gates DETECTED findings; (4) a dismissed slug is
// surfaced to the suppression read. Each runs in one rolled-back transaction loading 0006.

func openPromptLibraryTx(t *testing.T) *gorm.DB {
	t.Helper()
	tx := testsupport.OpenTestTx(t)
	loadSchemaFile(t, tx, "0006_prompt_library.sql")
	return tx
}

const (
	plOrgA  = "11111111-1111-1111-1111-111111111111"
	plRepo1 = "33333333-3333-3333-3333-333333333331"
	plRepo2 = "33333333-3333-3333-3333-333333333332"
)

func detectedSrc() *promptlibraryv1.ArtifactRecommendationSource {
	s := promptlibraryv1.ArtifactRecommendationSource_ARTIFACT_RECOMMENDATION_SOURCE_DETECTED
	return &s
}

// plItem builds a VISIBLE prompt_library item for a run. src nil = DERIVED (NULL-source) row;
// non-nil = DETECTED finding. client_status defaults to UNSPECIFIED.
func plItem(runID, scopeValue string, scope promptlibraryv1.ArtifactScope, aType promptlibraryv1.ArtifactType, slug string, src *promptlibraryv1.ArtifactRecommendationSource) *model.PromptLibraryItem {
	return &model.PromptLibraryItem{
		OrganizationID:       plOrgA,
		TaskExecutionID:      runID,
		Scope:                scope,
		ScopeValue:           scopeValue,
		ArtifactType:         aType,
		Slug:                 slug,
		Title:                slug,
		SurfaceState:         promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE,
		ClientStatus:         promptlibraryv1.ArtifactClientStatus_ARTIFACT_CLIENT_STATUS_UNSPECIFIED,
		RecommendationSource: src,
	}
}

// isLatestOf returns is_latest for the single live row a run wrote (these tests write one row per run).
func isLatestOf(t *testing.T, tx *gorm.DB, runID string) bool {
	t.Helper()
	var isLatest bool
	if err := tx.Raw(
		"SELECT is_latest FROM prompt_library WHERE task_execution_id = ? AND deleted_at IS NULL", runID,
	).Scan(&isLatest).Error; err != nil {
		t.Fatalf("reading is_latest for run %s: %v", runID, err)
	}
	return isLatest
}

func TestPromptLibraryRepo_PersistArtifacts_ReconcileLatest(t *testing.T) {
	tx := openPromptLibraryTx(t)
	repo := NewPromptLibraryRepo(tx)
	ctx := context.Background()
	rule := promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE
	project := promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT

	// A DIFFERENT partition (repo2) with its own run — must stay is_latest after the repo1 reconcile.
	if err := repo.PersistArtifacts(ctx, plOrgA, "44440000-0000-0000-0000-000000000002", []*model.PromptLibraryItem{
		plItem("44440000-0000-0000-0000-000000000002", plRepo2, project, rule, "other-rule", nil),
	}); err != nil {
		t.Fatalf("seeding repo2 partition: %v", err)
	}

	// v1 in repo1/RULE.
	if err := repo.PersistArtifacts(ctx, plOrgA, "4444000a-0000-0000-0000-00000000000a", []*model.PromptLibraryItem{
		plItem("4444000a-0000-0000-0000-00000000000a", plRepo1, project, rule, "rule-a", nil),
	}); err != nil {
		t.Fatalf("persisting v1: %v", err)
	}
	if !isLatestOf(t, tx, "4444000a-0000-0000-0000-00000000000a") {
		t.Fatal("v1 should be is_latest after first persist")
	}

	// v2 in the SAME partition — promotes v2, demotes v1.
	if err := repo.PersistArtifacts(ctx, plOrgA, "4444000b-0000-0000-0000-00000000000b", []*model.PromptLibraryItem{
		plItem("4444000b-0000-0000-0000-00000000000b", plRepo1, project, rule, "rule-a", nil),
	}); err != nil {
		t.Fatalf("persisting v2: %v", err)
	}
	if isLatestOf(t, tx, "4444000a-0000-0000-0000-00000000000a") {
		t.Error("v1 should be demoted (is_latest=false) after v2 persist")
	}
	if !isLatestOf(t, tx, "4444000b-0000-0000-0000-00000000000b") {
		t.Error("v2 should be is_latest after second persist")
	}
	if !isLatestOf(t, tx, "44440000-0000-0000-0000-000000000002") {
		t.Error("repo2's run (a different partition) must be UNTOUCHED by the repo1 reconcile")
	}
}

func TestPromptLibraryRepo_DetectedDerivedIsolation(t *testing.T) {
	tx := openPromptLibraryTx(t)
	repo := NewPromptLibraryRepo(tx)
	ctx := context.Background()
	rule := promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE
	project := promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT
	part := EvaluatedPartition{Scope: project, ScopeValue: plRepo1, ArtifactType: rule}

	// A DERIVED row and a DETECTED finding in the SAME (scope, scope_value, artifact_type) — disjoint
	// provenance partitions. Both start is_latest.
	if err := repo.PersistArtifacts(ctx, plOrgA, "5555000a-0000-0000-0000-00000000000a", []*model.PromptLibraryItem{
		plItem("5555000a-0000-0000-0000-00000000000a", plRepo1, project, rule, "rule-a", nil),
	}); err != nil {
		t.Fatalf("seeding derived: %v", err)
	}
	if err := repo.PersistFindings(ctx, plOrgA, "5555000c-0000-0000-0000-00000000000c", []*model.PromptLibraryItem{
		plItem("5555000c-0000-0000-0000-00000000000c", plRepo1, project, rule, "finding-x", detectedSrc()),
	}, []EvaluatedPartition{part}); err != nil {
		t.Fatalf("seeding detected: %v", err)
	}
	if !isLatestOf(t, tx, "5555000a-0000-0000-0000-00000000000a") || !isLatestOf(t, tx, "5555000c-0000-0000-0000-00000000000c") {
		t.Fatal("both the derived row and the detected finding should start is_latest")
	}

	// A NEW DETECTED run must demote only the prior DETECTED finding, never the DERIVED row.
	if err := repo.PersistFindings(ctx, plOrgA, "5555000d-0000-0000-0000-00000000000d", []*model.PromptLibraryItem{
		plItem("5555000d-0000-0000-0000-00000000000d", plRepo1, project, rule, "finding-x", detectedSrc()),
	}, []EvaluatedPartition{part}); err != nil {
		t.Fatalf("persisting detected v2: %v", err)
	}
	if isLatestOf(t, tx, "5555000c-0000-0000-0000-00000000000c") {
		t.Error("prior detected finding should be demoted by the new detected run")
	}
	if !isLatestOf(t, tx, "5555000d-0000-0000-0000-00000000000d") {
		t.Error("new detected finding should be is_latest")
	}
	if !isLatestOf(t, tx, "5555000a-0000-0000-0000-00000000000a") {
		t.Error("DERIVED row must NOT be cleared by a DETECTED reconcile")
	}

	// And the converse: a NEW DERIVED run must not clear the live DETECTED finding.
	if err := repo.PersistArtifacts(ctx, plOrgA, "5555000b-0000-0000-0000-00000000000b", []*model.PromptLibraryItem{
		plItem("5555000b-0000-0000-0000-00000000000b", plRepo1, project, rule, "rule-a", nil),
	}); err != nil {
		t.Fatalf("persisting derived v2: %v", err)
	}
	if isLatestOf(t, tx, "5555000a-0000-0000-0000-00000000000a") {
		t.Error("prior derived row should be demoted by the new derived run")
	}
	if !isLatestOf(t, tx, "5555000b-0000-0000-0000-00000000000b") {
		t.Error("new derived row should be is_latest")
	}
	if !isLatestOf(t, tx, "5555000d-0000-0000-0000-00000000000d") {
		t.Error("live DETECTED finding must NOT be cleared by a DERIVED reconcile")
	}
}

func TestPromptLibraryRepo_PersistFindings_AutoCloseVanishedPartition(t *testing.T) {
	tx := openPromptLibraryTx(t)
	repo := NewPromptLibraryRepo(tx)
	ctx := context.Background()
	rule := promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE
	project := promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT
	part := EvaluatedPartition{Scope: project, ScopeValue: plRepo1, ArtifactType: rule}

	// A DETECTED finding exists and is latest.
	if err := repo.PersistFindings(ctx, plOrgA, "6666000a-0000-0000-0000-00000000000a", []*model.PromptLibraryItem{
		plItem("6666000a-0000-0000-0000-00000000000a", plRepo1, project, rule, "finding-x", detectedSrc()),
	}, []EvaluatedPartition{part}); err != nil {
		t.Fatalf("seeding finding: %v", err)
	}
	if !isLatestOf(t, tx, "6666000a-0000-0000-0000-00000000000a") {
		t.Fatal("finding should start is_latest")
	}

	// A re-scan fully evaluates the partition but produces NO finding (the condition no longer detects).
	// Reconcile-only: items empty, the partition still named — must auto-close (clear without re-promote).
	if err := repo.PersistFindings(ctx, plOrgA, "6666000b-0000-0000-0000-00000000000b", nil, []EvaluatedPartition{part}); err != nil {
		t.Fatalf("auto-close run: %v", err)
	}
	if isLatestOf(t, tx, "6666000a-0000-0000-0000-00000000000a") {
		t.Error("vanished finding should be auto-closed (is_latest cleared, not re-promoted)")
	}
	live, err := repo.ListLiveDetectedPartitions(ctx, plOrgA, project, plRepo1)
	if err != nil {
		t.Fatalf("ListLiveDetectedPartitions: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("expected no live detected partition after auto-close, got %+v", live)
	}
}

// TestPromptLibraryRepo_PersistFindings_UnreconciledPartitionStaysNotLatest pins the fillArtifactDefaults
// guard (item.IsLatest = false on the persist path). A finding in a partition the run did NOT evaluate
// (a degraded lens: present in items, absent from evaluatedPartitions) is inserted but never reconciled,
// so it must land is_latest=false even when the caller handed it IsLatest=true — otherwise the column's
// DEFAULT true, or a caller's stale value, would leave an un-reconciled finding wrongly live.
func TestPromptLibraryRepo_PersistFindings_UnreconciledPartitionStaysNotLatest(t *testing.T) {
	tx := openPromptLibraryTx(t)
	repo := NewPromptLibraryRepo(tx)
	ctx := context.Background()
	rule := promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE
	project := promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT

	runID := "88880000-0000-0000-0000-00000000000a"
	item := plItem(runID, plRepo1, project, rule, "unreconciled-finding", detectedSrc())
	item.IsLatest = true // the caller sets it true; the persist path must force it false

	// evaluatedPartitions names a DIFFERENT partition (repo2), so the item's repo1 partition is never
	// reconciled — the partially-evaluated-lens case.
	evaluated := []EvaluatedPartition{{Scope: project, ScopeValue: plRepo2, ArtifactType: rule}}
	if err := repo.PersistFindings(ctx, plOrgA, runID, []*model.PromptLibraryItem{item}, evaluated); err != nil {
		t.Fatalf("PersistFindings: %v", err)
	}
	if isLatestOf(t, tx, runID) {
		t.Error("a finding in an un-reconciled partition must land is_latest=false, got true")
	}
}

func TestPromptLibraryRepo_ListVisibleByOrg_DarkGate(t *testing.T) {
	tx := openPromptLibraryTx(t)
	repo := NewPromptLibraryRepo(tx)
	ctx := context.Background()
	rule := promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE
	project := promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT
	part := EvaluatedPartition{Scope: project, ScopeValue: plRepo1, ArtifactType: rule}

	if err := repo.PersistArtifacts(ctx, plOrgA, "7777000a-0000-0000-0000-00000000000a", []*model.PromptLibraryItem{
		plItem("7777000a-0000-0000-0000-00000000000a", plRepo1, project, rule, "rule-a", nil),
	}); err != nil {
		t.Fatalf("seeding derived: %v", err)
	}
	if err := repo.PersistFindings(ctx, plOrgA, "7777000b-0000-0000-0000-00000000000b", []*model.PromptLibraryItem{
		plItem("7777000b-0000-0000-0000-00000000000b", plRepo1, project, rule, "finding-x", detectedSrc()),
	}, []EvaluatedPartition{part}); err != nil {
		t.Fatalf("seeding detected: %v", err)
	}

	dark, err := repo.ListVisibleByOrg(ctx, plOrgA, "", false)
	if err != nil {
		t.Fatalf("ListVisibleByOrg(dark): %v", err)
	}
	if len(dark) != 1 || dark[0].Slug != "rule-a" {
		t.Fatalf("dark list = %d rows %v, want only the DERIVED rule-a (DETECTED excluded)", len(dark), slugsOf(dark))
	}

	lit, err := repo.ListVisibleByOrg(ctx, plOrgA, "", true)
	if err != nil {
		t.Fatalf("ListVisibleByOrg(includeDetected): %v", err)
	}
	if len(lit) != 2 {
		t.Fatalf("includeDetected list = %d rows %v, want both DERIVED + DETECTED", len(lit), slugsOf(lit))
	}
}

func TestPromptLibraryRepo_ListDismissedSlugs_Suppression(t *testing.T) {
	tx := openPromptLibraryTx(t)
	repo := NewPromptLibraryRepo(tx)
	ctx := context.Background()
	rule := promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE
	project := promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT

	dismissed := plItem("8888000a-0000-0000-0000-00000000000a", plRepo1, project, rule, "dismissed-rule", nil)
	dismissed.ClientStatus = promptlibraryv1.ArtifactClientStatus_ARTIFACT_CLIENT_STATUS_DISMISSED
	kept := plItem("8888000a-0000-0000-0000-00000000000a", plRepo1, project, rule, "kept-rule", nil)
	if err := repo.PersistArtifacts(ctx, plOrgA, "8888000a-0000-0000-0000-00000000000a", []*model.PromptLibraryItem{dismissed, kept}); err != nil {
		t.Fatalf("seeding dismiss fixture: %v", err)
	}

	slugs, err := repo.ListDismissedSlugs(ctx, plOrgA, project, plRepo1)
	if err != nil {
		t.Fatalf("ListDismissedSlugs: %v", err)
	}
	if len(slugs) != 1 || slugs[0] != "dismissed-rule" {
		t.Errorf("dismissed slugs = %v, want [dismissed-rule] (the non-dismissed slug must be filtered)", slugs)
	}
}

func slugsOf(items []*model.PromptLibraryItem) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.Slug)
	}
	return out
}
