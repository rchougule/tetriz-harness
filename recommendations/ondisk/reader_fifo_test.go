//go:build darwin || linux

// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package ondisk

import (
	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestReadBody_FIFORejectedWithoutWriter(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "CLAUDE.md"), 0600); err != nil {
		t.Fatal(err)
	}
	anchor, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = anchor.Close() }()
	// No writer exists: a blocking open would hang until the test deadline.
	if body, ok := readBody(anchor, "CLAUDE.md"); ok || len(body) != 0 {
		t.Fatal("FIFO accepted as body")
	}
	inv, err := Read(root, commonv1.HarnessArtifactScope_HARNESS_ARTIFACT_SCOPE_REPO_COMMITTED)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Artifacts) != 0 || len(inv.RepoPaths) != 0 {
		t.Fatal("FIFO entered inventory")
	}
}
