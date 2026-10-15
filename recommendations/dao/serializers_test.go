// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

import (
	"testing"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	harnesspb "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/harness/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// These prove the GORM serializers end-to-end through a real gorm.Create → First round-trip: the
// proto-enum columns persist as their value-NAME string and parse back to the enum, and the
// proto-message JSONB column (HarnessArtifact.Files) persists as protojson and parses back.

func TestProtoEnumSerializer_HarnessArtifactRoundTrip(t *testing.T) {
	tx := openTestTx(t)
	loadSchemaFile(t, tx, "0004_harness_inventory.sql")

	repoID := "cccccccc-cccc-cccc-cccc-cccccccccccc"
	fileID := "dddddddd-dddd-dddd-dddd-dddddddddddd"
	want := &model.HarnessArtifact{
		OrganizationID:   testOrgA,
		ArtifactSource:   commonv1.HarnessArtifactSource_HARNESS_ARTIFACT_SOURCE_VCS, // VCS ⇒ person_id NULL
		IdeType:          commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		Scope:            commonv1.HarnessArtifactScope_HARNESS_ARTIFACT_SCOPE_REPO_COMMITTED, // ⇒ repository_id set
		Type:             commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL,
		Name:             "spec-driven-dev",
		RepositoryID:     &repoID,
		SourcePath:       ".claude/skills/spec-driven-dev/SKILL.md",
		CurrentFileID:    fileID,
		OperationalState: commonv1.HarnessOperationalState_HARNESS_OPERATIONAL_STATE_ENABLED,
		Files: &harnesspb.HarnessArtifactFileSet{
			Files: []*harnesspb.HarnessArtifactFileRef{
				{RelativePath: "SKILL.md", FileId: fileID, IsPrimary: true, Truncated: false},
				{RelativePath: "helper.md", IsPrimary: false, Truncated: true},
			},
		},
	}
	if err := tx.Create(want).Error; err != nil {
		t.Fatalf("creating harness_artifact: %v", err)
	}

	var got model.HarnessArtifact
	if err := tx.First(&got, "id = ?", want.ID).Error; err != nil {
		t.Fatalf("reading harness_artifact back: %v", err)
	}
	if got.IdeType != commonv1.IdeType_IDE_TYPE_CLAUDE_CODE ||
		got.Scope != commonv1.HarnessArtifactScope_HARNESS_ARTIFACT_SCOPE_REPO_COMMITTED ||
		got.Type != commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL ||
		got.OperationalState != commonv1.HarnessOperationalState_HARNESS_OPERATIONAL_STATE_ENABLED ||
		got.ArtifactSource != commonv1.HarnessArtifactSource_HARNESS_ARTIFACT_SOURCE_VCS {
		t.Errorf("enum round-trip mismatch: ide=%v scope=%v type=%v state=%v source=%v",
			got.IdeType, got.Scope, got.Type, got.OperationalState, got.ArtifactSource)
	}
	refs := got.FileRefs()
	if len(refs) != 2 || refs[0].GetRelativePath() != "SKILL.md" || !refs[0].GetIsPrimary() ||
		refs[1].GetRelativePath() != "helper.md" || !refs[1].GetTruncated() {
		t.Errorf("protojson Files round-trip mismatch: %+v", refs)
	}

	// The enum columns must be stored as the value-NAME string, not the ordinal int.
	var typeText string
	if err := tx.Raw("SELECT type FROM harness_artifacts WHERE id = ?", want.ID).Scan(&typeText).Error; err != nil {
		t.Fatalf("reading raw type column: %v", err)
	}
	if typeText != "HARNESS_ARTIFACT_TYPE_SKILL" {
		t.Errorf("type column stored %q, want the value-name string HARNESS_ARTIFACT_TYPE_SKILL", typeText)
	}
}

func TestProtoEnumSerializer_HarnessFileStatusRoundTrip(t *testing.T) {
	tx := openTestTx(t)
	loadSchemaFile(t, tx, "0004_harness_inventory.sql")

	want := &model.HarnessFile{
		OrganizationID: testOrgA,
		ContentHash:    "sha256:abc",
		ByteSize:       42,
		Status:         commonv1.HarnessFileStatus_HARNESS_FILE_STATUS_CONFIRMED,
		Content:        []byte("# SKILL\n"),
	}
	if err := tx.Create(want).Error; err != nil {
		t.Fatalf("creating harness_file: %v", err)
	}
	var got model.HarnessFile
	if err := tx.First(&got, "id = ?", want.ID).Error; err != nil {
		t.Fatalf("reading harness_file back: %v", err)
	}
	if got.Status != commonv1.HarnessFileStatus_HARNESS_FILE_STATUS_CONFIRMED {
		t.Errorf("Status round-trip = %v, want CONFIRMED", got.Status)
	}
	var statusText string
	if err := tx.Raw("SELECT status FROM harness_files WHERE id = ?", want.ID).Scan(&statusText).Error; err != nil {
		t.Fatalf("reading raw status column: %v", err)
	}
	if statusText != "HARNESS_FILE_STATUS_CONFIRMED" {
		t.Errorf("status column stored %q, want HARNESS_FILE_STATUS_CONFIRMED", statusText)
	}
}
