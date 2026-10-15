// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Command read-harness-inventory classifies the harness surface files in a repository on disk and
// prints the inventory, with no database, no model and no network call. It is the offline half of the
// DETECTED scan: ondisk.Read walks a repository root, classifies every known harness file per tool
// (CLAUDE.md, rules, skills, commands, agents, a settings.json fanned into its permission and hook
// parts), records every file path the path lenses resolve against, and collects any declared MCP
// servers. The DETECTED detectors run on exactly this inventory, so reading it is how you see what the
// scanner will look at before you stand up a database.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/ondisk"
)

func main() {
	// The bundled sample harness lives beside the on-disk reader; resolve it relative to this file so
	// the example runs from any working directory.
	_, thisFile, _, _ := runtime.Caller(0)
	sampleRepo := filepath.Join(filepath.Dir(thisFile), "..", "..", "ondisk", "testdata", "sample-repo")

	inv, err := ondisk.Read(sampleRepo, commonv1.HarnessArtifactScope_HARNESS_ARTIFACT_SCOPE_REPO_COMMITTED)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}

	fmt.Printf("artifacts: %d over %d files\n", len(inv.Artifacts), len(inv.RepoPaths))

	countByType := map[string]int{}
	for _, a := range inv.Artifacts {
		countByType[a.Type.String()]++
	}
	types := make([]string, 0, len(countByType))
	for t := range countByType {
		types = append(types, t)
	}
	sort.Strings(types)
	fmt.Println("by type:")
	for _, t := range types {
		fmt.Printf("  %s: %d\n", t, countByType[t])
	}

	sort.Strings(inv.MCPServerNames)
	fmt.Printf("mcp servers declared: %v\n", inv.MCPServerNames)
}
