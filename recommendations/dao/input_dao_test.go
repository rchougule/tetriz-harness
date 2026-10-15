// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/testsupport"
)

// Real-Postgres tests for the input-table DAOs. They prove the non-trivial reads: the raw_records
// per-vendor body COALESCE, platform_identity_map BatchGet scoping, the VCS harness inventory
// (artifact_source=VCS filter + the harness_files join + the UNSPECIFIED-sentinel exclusion), the
// personal-lane scope filter, the ide_type enum conversion, and the primary-repo argmax; plus lighter
// checks on the mechanical repo/profile/file reads. Each runs in one rolled-back transaction loading
// the migrations it needs.

func openInputTx(t *testing.T, migrations ...string) *gorm.DB {
	t.Helper()
	tx := testsupport.OpenTestTx(t)
	for _, m := range migrations {
		loadSchemaFile(t, tx, m)
	}
	return tx
}

const (
	inOrgA  = "11111111-1111-1111-1111-111111111111"
	inOrgB  = "22222222-2222-2222-2222-222222222222"
	inRepo1 = "33333333-3333-3333-3333-333333333331"
	inRepo2 = "33333333-3333-3333-3333-333333333332"
	inP1    = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa1"
	inP2    = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa2"
	inConn  = "cccccccc-cccc-cccc-cccc-ccccccccccc1"
)

func TestRawRecordRepo_ReviewBodies_VendorShapes(t *testing.T) {
	tx := openInputTx(t, "0002_vcs_pr_signals.sql")
	repo := NewRawRecordRepo(tx)
	ctx := context.Background()
	now := time.Now().UTC()

	const ins = `INSERT INTO raw_records (organization_id, connector_id, operation_id, data_date, metadata, payload)
		VALUES (?, ?, ?, ?, ?::jsonb, ?::jsonb)`
	rows := []struct {
		org, op, meta, payload string
	}{
		// GitHub/GitLab shape: payload.body
		{inOrgA, "fetch_reviews", `{"repo":"acme/api"}`, `{"body":"please fix the error handling"}`},
		// Bitbucket shape: payload.content.raw
		{inOrgA, "fetch_pr_comments", `{"repo":"acme/api"}`, `{"content":{"raw":"nit: rename this"}}`},
		// Bitbucket nested comment shape: payload.comment.content.raw
		{inOrgA, "fetch_pr_comments", `{"repo":"acme/api"}`, `{"comment":{"content":{"raw":"add a test"}}}`},
		// Empty body → excluded (NULLIF).
		{inOrgA, "fetch_reviews", `{"repo":"acme/api"}`, `{"body":""}`},
		// Different repo → excluded by the repo filter.
		{inOrgA, "fetch_reviews", `{"repo":"other/repo"}`, `{"body":"elsewhere"}`},
		// Other org → excluded.
		{inOrgB, "fetch_reviews", `{"repo":"acme/api"}`, `{"body":"other org"}`},
		// Non-review operation → excluded by the operation filter.
		{inOrgA, "fetch_prs", `{"repo":"acme/api"}`, `{"body":"a PR body, not a review"}`},
	}
	for _, r := range rows {
		if err := tx.Exec(ins, r.org, inConn, r.op, now, r.meta, r.payload).Error; err != nil {
			t.Fatalf("seeding raw_records: %v", err)
		}
	}
	from, to := now.Add(-24*time.Hour), now

	count, err := repo.CountReviewBodies(ctx, inOrgA, "acme/api", inConn, from, to)
	if err != nil {
		t.Fatalf("CountReviewBodies: %v", err)
	}
	if count != 3 {
		t.Errorf("CountReviewBodies = %d, want 3 (3 non-empty acme/api review bodies)", count)
	}

	bodies, err := repo.ListReviewBodies(ctx, inOrgA, []string{"acme/api"}, []string{inConn}, from, to, 0)
	if err != nil {
		t.Fatalf("ListReviewBodies: %v", err)
	}
	if len(bodies) != 3 {
		t.Fatalf("ListReviewBodies = %d rows %v, want 3", len(bodies), bodies)
	}
	seen := map[string]bool{}
	for _, b := range bodies {
		seen[b] = true
	}
	for _, want := range []string{"please fix the error handling", "nit: rename this", "add a test"} {
		if !seen[want] {
			t.Errorf("expected body %q in result %v", want, bodies)
		}
	}
}

func TestPlatformIdentityMapRepo_BatchGet(t *testing.T) {
	tx := openInputTx(t, "0005_identity.sql")
	repo := NewPlatformIdentityMapRepo(tx)
	ctx := context.Background()

	const ins = `INSERT INTO platform_identity_map (organization_id, person_id, platform, platform_identity_id) VALUES (?, ?, ?, ?)`
	id1 := "dddddddd-dddd-dddd-dddd-ddddddddddd1"
	id2 := "dddddddd-dddd-dddd-dddd-ddddddddddd2"
	seed := []struct{ org, person, platform, identity string }{
		{inOrgA, inP1, "github", id1},
		{inOrgA, inP2, "github", id2},
		{inOrgA, inP1, "gitlab", "dddddddd-dddd-dddd-dddd-ddddddddddd3"}, // other platform → excluded
		{inOrgB, inP1, "github", "dddddddd-dddd-dddd-dddd-ddddddddddd4"}, // other org → excluded
	}
	for _, s := range seed {
		if err := tx.Exec(ins, s.org, s.person, s.platform, s.identity).Error; err != nil {
			t.Fatalf("seeding platform_identity_map: %v", err)
		}
	}

	got, err := repo.BatchGetIdentityIDsForPersons(ctx, inOrgA, []string{inP1, inP2, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa9"}, "github")
	if err != nil {
		t.Fatalf("BatchGet: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d mappings %v, want 2 (p1,p2 on github; unmapped person omitted)", len(got), got)
	}
	if got[inP1] != id1 || got[inP2] != id2 {
		t.Errorf("mappings = %v, want p1=%s p2=%s", got, id1, id2)
	}
}

// insertHarnessArtifact seeds one harness_artifacts row honoring the person/source + scope/repo CHECKs.
func insertHarnessArtifact(t *testing.T, tx *gorm.DB, id, source, personID, scope, aType, name, repoID, srcPath, fileID string) {
	t.Helper()
	var person, repo any
	if personID == "" {
		person = nil
	} else {
		person = personID
	}
	if repoID == "" {
		repo = nil
	} else {
		repo = repoID
	}
	const ins = `INSERT INTO harness_artifacts
		(id, organization_id, artifact_source, person_id, ide_type, scope, type, name, repository_id, source_path, current_file_id, operational_state)
		VALUES (?, ?, ?, ?, 'IDE_TYPE_CLAUDE_CODE', ?, ?, ?, ?, ?, ?, 'HARNESS_OPERATIONAL_STATE_ENABLED')`
	if err := tx.Exec(ins, id, inOrgA, source, person, scope, aType, name, repo, srcPath, fileID).Error; err != nil {
		t.Fatalf("seeding harness_artifacts %s: %v", name, err)
	}
}

func insertHarnessFile(t *testing.T, tx *gorm.DB, id, hash, content string) {
	t.Helper()
	const ins = `INSERT INTO harness_files (id, organization_id, content_hash, status, content)
		VALUES (?, ?, ?, 'HARNESS_FILE_STATUS_CONFIRMED', ?)`
	if err := tx.Exec(ins, id, inOrgA, hash, []byte(content)).Error; err != nil {
		t.Fatalf("seeding harness_files %s: %v", id, err)
	}
}

func TestHarnessArtifactRepo_VCSInventory(t *testing.T) {
	tx := openInputTx(t, "0004_harness_inventory.sql")
	repo := NewHarnessArtifactRepo(tx)
	ctx := context.Background()

	f1 := "ffffffff-ffff-ffff-ffff-fffffffffff1"
	f2 := "ffffffff-ffff-ffff-ffff-fffffffffff2"
	f3 := "ffffffff-ffff-ffff-ffff-fffffffffff3"
	f4 := "ffffffff-ffff-ffff-ffff-fffffffffff4"
	f5 := "ffffffff-ffff-ffff-ffff-fffffffffff5"
	insertHarnessFile(t, tx, f1, "h1", "rule body one")
	insertHarnessFile(t, tx, f2, "h2", "hook body")
	insertHarnessFile(t, tx, f3, "h3", "")
	insertHarnessFile(t, tx, f4, "h4", "repo2 rule body")
	insertHarnessFile(t, tx, f5, "h5", "personal body")

	a := "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeee"
	// repo1 VCS: a RULE, a HOOK, and the UNSPECIFIED settings sentinel.
	insertHarnessArtifact(t, tx, a+"1", vcsArtifactSource, "", "HARNESS_ARTIFACT_SCOPE_REPO_COMMITTED", "HARNESS_ARTIFACT_TYPE_RULE", "rules/x", inRepo1, ".cursor/rules/x.md", f1)
	insertHarnessArtifact(t, tx, a+"2", vcsArtifactSource, "", "HARNESS_ARTIFACT_SCOPE_REPO_COMMITTED", "HARNESS_ARTIFACT_TYPE_HOOK", "hooks/y", inRepo1, ".claude/hooks/y", f2)
	insertHarnessArtifact(t, tx, a+"3", vcsArtifactSource, "", "HARNESS_ARTIFACT_SCOPE_REPO_COMMITTED", "HARNESS_ARTIFACT_TYPE_UNSPECIFIED", ".claude/settings.json#empty", inRepo1, ".claude/settings.json", f3)
	// repo2 VCS: a RULE.
	insertHarnessArtifact(t, tx, a+"4", vcsArtifactSource, "", "HARNESS_ARTIFACT_SCOPE_REPO_COMMITTED", "HARNESS_ARTIFACT_TYPE_RULE", "rules/z", inRepo2, ".cursor/rules/z.md", f4)
	// DESKTOP personal p1: a USER_GLOBAL rule (personal) and a REPO_COMMITTED working copy (NOT personal).
	insertHarnessArtifact(t, tx, a+"5", desktopArtifactSource, inP1, "HARNESS_ARTIFACT_SCOPE_USER_GLOBAL", "HARNESS_ARTIFACT_TYPE_RULE", "personal/rule", "", "~/.claude/rule", f5)
	insertHarnessArtifact(t, tx, a+"6", desktopArtifactSource, inP1, "HARNESS_ARTIFACT_SCOPE_REPO_COMMITTED", "HARNESS_ARTIFACT_TYPE_RULE", "working/copy", inRepo1, ".cursor/rules/x.md", f1)

	vcs, err := repo.ListVCSByRepo(ctx, inOrgA, inRepo1)
	if err != nil {
		t.Fatalf("ListVCSByRepo: %v", err)
	}
	if len(vcs) != 3 {
		t.Errorf("ListVCSByRepo(repo1) = %d, want 3 (RULE, HOOK, sentinel — all VCS; the desktop working copy excluded)", len(vcs))
	}
	for _, art := range vcs {
		if art.PersonID != nil {
			t.Errorf("VCS artifact %s has a person_id, want person-less", art.Name)
		}
	}

	repoIDs, err := repo.ListRepoIDsWithLiveVCSArtifacts(ctx, inOrgA)
	if err != nil {
		t.Fatalf("ListRepoIDsWithLiveVCSArtifacts: %v", err)
	}
	// repo1 (has RULE+HOOK, the sentinel alone wouldn't count) + repo2.
	set := map[string]bool{}
	for _, id := range repoIDs {
		set[id] = true
	}
	if !set[inRepo1] || !set[inRepo2] || len(repoIDs) != 2 {
		t.Errorf("repo ids = %v, want {repo1, repo2}", repoIDs)
	}

	content, err := repo.ListVCSArtifactContentByType(ctx, inOrgA, []commonv1.HarnessArtifactType{commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE})
	if err != nil {
		t.Fatalf("ListVCSArtifactContentByType: %v", err)
	}
	if len(content) != 2 {
		t.Fatalf("content-by-type(RULE) = %d, want 2 (repo1 rules/x + repo2 rules/z; HOOK + sentinel + desktop excluded)", len(content))
	}
	for _, row := range content {
		if row.ContentHash == "" || row.RepositoryID == "" {
			t.Errorf("content row %+v missing hash or repo (join to harness_files failed)", row)
		}
	}

	personal, err := repo.ListPersonalByPersons(ctx, inOrgA, []string{inP1})
	if err != nil {
		t.Fatalf("ListPersonalByPersons: %v", err)
	}
	if len(personal) != 1 || personal[0].Name != "personal/rule" {
		t.Errorf("ListPersonalByPersons = %d rows, want 1 (USER_GLOBAL only; REPO_COMMITTED working copy excluded)", len(personal))
	}
}

func TestHarnessFileRepo_ListByIDs_Content(t *testing.T) {
	tx := openInputTx(t, "0004_harness_inventory.sql")
	repo := NewHarnessFileRepo(tx)
	ctx := context.Background()

	f1 := "ffffffff-ffff-ffff-ffff-fffffffffff1"
	f2 := "ffffffff-ffff-ffff-ffff-fffffffffff2"
	insertHarnessFile(t, tx, f1, "h1", "body one")
	insertHarnessFile(t, tx, f2, "h2", "body two")

	files, err := repo.ListByIDs(ctx, inOrgA, []string{f1, f2})
	if err != nil {
		t.Fatalf("ListByIDs: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("ListByIDs = %d, want 2", len(files))
	}
	byID := map[string]string{}
	for _, f := range files {
		byID[f.ID] = string(f.Content)
		if f.Status != commonv1.HarnessFileStatus_HARNESS_FILE_STATUS_CONFIRMED {
			t.Errorf("file %s status = %s, want CONFIRMED (protoenum round-trip)", f.ID, f.Status)
		}
	}
	if byID[f1] != "body one" || byID[f2] != "body two" {
		t.Errorf("contents = %v, want bodies read from the content column", byID)
	}
}

func TestHarnessUsageRecordsRepo_InvocationsAndPrimaryRepo(t *testing.T) {
	tx := openInputTx(t, "0004_harness_inventory.sql")
	repo := NewHarnessUsageRecordsRepo(tx)
	ctx := context.Background()
	today := time.Now().UTC()

	const insUsage = `INSERT INTO harness_usage_records
		(organization_id, person_id, ide_type, user_global, source_path, artifact_type, name, conversation_id, usage_date, invocation_count, repository_id)
		VALUES (?, ?, 'IDE_TYPE_CURSOR', ?, ?, 'HARNESS_ARTIFACT_TYPE_RULE', ?, ?, ?, ?, ?)`
	// p1 invokes twice in repo1 across two conversations, once in repo2.
	mustExec := func(sql string, args ...any) {
		if err := tx.Exec(sql, args...).Error; err != nil {
			t.Fatalf("seeding usage: %v", err)
		}
	}
	mustExec(insUsage, inOrgA, inP1, false, "/rules/a", "rule-a", "conv1", today, 3, inRepo1)
	mustExec(insUsage, inOrgA, inP1, false, "/rules/a", "rule-a", "conv2", today, 2, inRepo1)
	mustExec(insUsage, inOrgA, inP1, true, "~/.cursor/g", "g", "conv3", today, 1, inRepo2)

	from, to := today.Add(-24*time.Hour), today.Add(24*time.Hour)
	invs, err := repo.ListInvocationsForPersons(ctx, inOrgA, []string{inP1}, from, to)
	if err != nil {
		t.Fatalf("ListInvocationsForPersons: %v", err)
	}
	var repo1Total, userGlobalTotal int64
	for _, s := range invs {
		if s.IdeType != commonv1.IdeType_IDE_TYPE_CURSOR {
			t.Errorf("ide_type = %s, want CURSOR (enum name converted)", s.IdeType)
		}
		if s.SourcePath == "/rules/a" {
			repo1Total += s.Invocations
		}
		if s.UserGlobal {
			userGlobalTotal += s.Invocations
		}
	}
	if repo1Total != 5 {
		t.Errorf("/rules/a invocations = %d, want 5 (3+2 summed across conversations)", repo1Total)
	}
	if userGlobalTotal != 1 {
		t.Errorf("user-global invocations = %d, want 1", userGlobalTotal)
	}

	// harness_sync_scopes makes repo1 p1's synced repo; the usage weight (5 in repo1 vs 1 in repo2) makes
	// repo1 the argmax primary repo.
	const insScope = `INSERT INTO harness_sync_scopes (organization_id, person_id, repository_id, inventory_fingerprint) VALUES (?, ?, ?, 'fp')`
	mustExec(insScope, inOrgA, inP1, inRepo1)

	persons, err := repo.ListPrimaryRepoPersons(ctx, inOrgA, inRepo1, from, to)
	if err != nil {
		t.Fatalf("ListPrimaryRepoPersons: %v", err)
	}
	if len(persons) != 1 || persons[0] != inP1 {
		t.Errorf("primary-repo persons for repo1 = %v, want [p1] (repo1 is p1's argmax)", persons)
	}
	// repo2 is not p1's primary (only 1 invocation), so p1 must NOT be returned for repo2.
	persons2, err := repo.ListPrimaryRepoPersons(ctx, inOrgA, inRepo2, from, to)
	if err != nil {
		t.Fatalf("ListPrimaryRepoPersons repo2: %v", err)
	}
	if len(persons2) != 0 {
		t.Errorf("primary-repo persons for repo2 = %v, want [] (repo1 is p1's primary)", persons2)
	}
}

func TestRepositoryRepo_Reads(t *testing.T) {
	tx := openInputTx(t, "0002_vcs_pr_signals.sql")
	repo := NewRepositoryRepo(tx)
	ctx := context.Background()

	const ins = `INSERT INTO vcs_repositories (id, organization_id, connector_id, full_name, name, archived, tree_content)
		VALUES (?, ?, ?, ?, ?, ?, ?)`
	mustExec := func(args ...any) {
		if err := tx.Exec(ins, args...).Error; err != nil {
			t.Fatalf("seeding vcs_repositories: %v", err)
		}
	}
	mustExec(inRepo1, inOrgA, inConn, "acme/api", "api", false, "src/\nsrc/main.go")
	mustExec(inRepo2, inOrgA, inConn, "acme/web", "web", true, nil) // archived → excluded from ListAllActiveForOrg

	got, err := repo.GetByID(ctx, inOrgA, inRepo1)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.FullName != "acme/api" || got.TreeContent == nil || *got.TreeContent == "" {
		t.Errorf("GetByID = %+v, want acme/api with tree_content", got)
	}

	list, err := repo.ListByIDs(ctx, inOrgA, []string{inRepo1, inRepo2})
	if err != nil {
		t.Fatalf("ListByIDs: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("ListByIDs = %d, want 2", len(list))
	}

	active, err := repo.ListAllActiveForOrg(ctx, inOrgA)
	if err != nil {
		t.Fatalf("ListAllActiveForOrg: %v", err)
	}
	if len(active) != 1 || active[0].ID != inRepo1 {
		t.Errorf("ListAllActiveForOrg = %d rows, want 1 (archived repo2 excluded)", len(active))
	}

	if _, err := repo.GetByID(ctx, inOrgB, inRepo1); err == nil {
		t.Error("GetByID for a foreign org should not return the row")
	}
}

func TestVCSRepoProfileRepo_GetByRepoID(t *testing.T) {
	tx := openInputTx(t, "0002_vcs_pr_signals.sql")
	repo := NewVCSRepoProfileRepo(tx)
	ctx := context.Background()

	const ins = `INSERT INTO vcs_repo_profile (organization_id, repo_id, merge_enforcement) VALUES (?, ?, ?)`
	if err := tx.Exec(ins, inOrgA, inRepo1, "MERGE_ENFORCEMENT_REQUIRE_REVIEW").Error; err != nil {
		t.Fatalf("seeding vcs_repo_profile: %v", err)
	}

	got, err := repo.GetByRepoID(ctx, inOrgA, inRepo1)
	if err != nil {
		t.Fatalf("GetByRepoID: %v", err)
	}
	if got.MergeEnforcement == nil || *got.MergeEnforcement != "MERGE_ENFORCEMENT_REQUIRE_REVIEW" {
		t.Errorf("merge_enforcement = %v, want the stored label", got.MergeEnforcement)
	}

	if _, err := repo.GetByRepoID(ctx, inOrgA, inRepo2); err != ErrNotFound {
		t.Errorf("missing profile should be ErrNotFound, got %v", err)
	}
}
