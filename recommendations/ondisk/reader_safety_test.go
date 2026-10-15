// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package ondisk

import (
	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	"os"
	"path/filepath"
	"testing"
)

func TestRead_ExternalSymlinkDoesNotEnterInventory(t *testing.T) {
	root, external := t.TempDir(), t.TempDir()
	secret := filepath.Join(external, "private.txt")
	if err := os.WriteFile(secret, []byte("synthetic-private-content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "CLAUDE.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	inv, err := Read(root, commonv1.HarnessArtifactScope_HARNESS_ARTIFACT_SCOPE_REPO_COMMITTED)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Artifacts) != 0 {
		t.Fatal("external symlink body entered inventory")
	}
}

func TestReadBody_RejectsExternalReplacement(t *testing.T) {
	root, external := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "CLAUDE.md"), []byte("synthetic-private-content"), 0600); err != nil {
		t.Fatal(err)
	}
	anchor, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = anchor.Close() }()
	// Simulate a directory replaced after the walk observed an ordinary entry.
	if err := os.Symlink(external, filepath.Join(root, "replaced")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if body, ok := readBody(anchor, "replaced/CLAUDE.md"); ok || len(body) != 0 {
		t.Fatal("external replacement body escaped root")
	}
}

func TestReadBoundedBody_GrowthAfterStat(t *testing.T) {
	// The descriptor may grow after its size was checked. The read itself must
	// enforce the limit without trusting the earlier size.
	if body, ok := readBoundedBody(&endlessBody{}); ok || len(body) != 0 {
		t.Fatal("growing body passed cap")
	}
}

type endlessBody struct{}

func (*endlessBody) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}
