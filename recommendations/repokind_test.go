// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
)

// treeOf builds a RepoTree whose directory_structure lists paths one per line — the standalone
// form DecodeRepoTreePaths round-trips, so a test states real paths rather than the grouped
// encoding.
func treeOf(name string, paths ...string) *promptlibraryv1.RepoTree {
	return &promptlibraryv1.RepoTree{
		RepoFullName:       name,
		DirectoryStructure: strings.Join(paths, "\n"),
	}
}

// TestClassifyRepoKind pins the signal that was missing when a role-based review pipeline was
// recommended for a GitOps repo: every other domain input is org-level, so nothing could say
// this repo holds deployment config rather than product code.
func TestClassifyRepoKind(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		tree      *promptlibraryv1.RepoTree
		wantShape promptlibraryv1.RepoShape
	}{
		{
			// The motivating case. An application-development artifact must not read as fitting
			// this repo.
			name: "a gitops repo is infrastructure",
			tree: treeOf("acme/gitops",
				"terraform/main.tf", "terraform/variables.tf", "terraform/outputs.tf",
				"helm/values.yaml", "k8s/deployment.yaml", "Jenkinsfile", "README.md"),
			wantShape: promptlibraryv1.RepoShape_REPO_SHAPE_INFRASTRUCTURE,
		},
		{
			name: "a source-dominated repo is an application",
			tree: treeOf("acme/app",
				"cmd/main.go", "internal/service.go", "internal/service_test.go",
				"pkg/util.go", "web/index.tsx", "README.md"),
			wantShape: promptlibraryv1.RepoShape_REPO_SHAPE_APPLICATION,
		},
		{
			name: "a prose repo is documentation",
			tree: treeOf("acme/docs",
				"README.md", "guides/setup.md", "guides/deploy.md", "adr/0001-choice.md",
				"adr/0002-choice.md", "rfc/plan.md"),
			wantShape: promptlibraryv1.RepoShape_REPO_SHAPE_DOCUMENTATION,
		},
		{
			// A monorepo carrying an app AND its deployment config: no single kind describes it,
			// and claiming one would misdirect either way.
			name: "an app beside its own deployment config is mixed",
			tree: treeOf("acme/core",
				"cmd/main.go", "internal/a.go", "internal/b.go",
				"terraform/main.tf", "helm/values.yaml", "k8s/deploy.yaml"),
			wantShape: promptlibraryv1.RepoShape_REPO_SHAPE_MIXED,
		},
		{
			// A bare plurality is not a decision — an honest unknown beats a confident kind,
			// because the prompt tells the model to trust this value. 4/10 leads but neither
			// reaches a majority nor leaves a substantial runner-up behind.
			name: "a plurality with no majority stays unspecified",
			tree: treeOf("acme/spread",
				"a.go", "b.go", "c.go", "d.go",
				"x.tf", "y.tf", "z.tf",
				"one.md", "two.md", "three.md"),
			wantShape: promptlibraryv1.RepoShape_REPO_SHAPE_UNSPECIFIED,
		},
		{
			// Below the sample floor no shape is decided at all: a share over four files is
			// not evidence, however lopsided it looks.
			name: "too few classified files stays unspecified",
			tree: treeOf("acme/thin",
				"main.go", "deploy.tf", "notes.md", "chart/values.yaml"),
			wantShape: promptlibraryv1.RepoShape_REPO_SHAPE_UNSPECIFIED,
		},
		{
			name:      "a tree with nothing classifiable stays unspecified",
			tree:      treeOf("acme/assets", "logo.png", "font.woff2", "data.csv"),
			wantShape: promptlibraryv1.RepoShape_REPO_SHAPE_UNSPECIFIED,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := classifyRepoKind(tt.tree)
			require.NotNil(t, got)
			assert.Equal(t, tt.tree.GetRepoFullName(), got.GetRepoFullName())
			assert.Equal(t, tt.wantShape, got.GetShape())
		})
	}
}

// TestClassifyRepoKindEvidence pins that a decided shape can be checked without the tree — a
// wrong shape has to be diagnosable from the recorded input alone, and a share over a handful
// of files is a weaker claim than the same share over hundreds.
func TestClassifyRepoKindEvidence(t *testing.T) {
	t.Parallel()
	got := classifyRepoKind(treeOf("acme/gitops",
		"terraform/main.tf", "terraform/variables.tf", "terraform/outputs.tf",
		"terraform/iam.tf", "terraform/vpc.tf", "terraform/rds.tf",
		"cmd/main.go", "cmd/helper.go"))

	require.NotNil(t, got)
	assert.Equal(t, promptlibraryv1.RepoShape_REPO_SHAPE_INFRASTRUCTURE, got.GetShape())
	assert.Equal(t, []string{"application 25%", "infrastructure 75%", "classified 8 of 8 files"},
		got.GetShapeEvidence(), "evidence names every family's share and the sample it is over")
}

// TestClassifyRepoKindEvidenceNamesItsDenominator covers the case a percentage alone
// misrepresents: a handful of classified files inside a large tree of assets reads as a
// unanimous share. The evidence has to name what the share was taken over, or the reader — the
// model included — over-trusts a thin signal.
func TestClassifyRepoKindEvidenceNamesItsDenominator(t *testing.T) {
	t.Parallel()
	paths := []string{"cmd/a.go", "cmd/b.go", "cmd/c.go", "cmd/d.go", "cmd/e.go"}
	for i := range 95 {
		paths = append(paths, fmt.Sprintf("assets/img%d.png", i))
	}

	got := classifyRepoKind(treeOf("acme/assets-heavy", paths...))

	require.NotNil(t, got)
	assert.Equal(t, promptlibraryv1.RepoShape_REPO_SHAPE_APPLICATION, got.GetShape())
	assert.Equal(t, []string{"application 100%", "classified 5 of 100 files"}, got.GetShapeEvidence(),
		"a 100% share over 5 of 100 files must say so, not just report 100%")
}

// TestClassifyRepoKindUnidentifiableRepo: a tree with no repo name cannot have its shape
// attributed to anything, so nothing is carried rather than an entry naming an empty repo.
func TestClassifyRepoKindUnidentifiableRepo(t *testing.T) {
	t.Parallel()
	assert.Nil(t, classifyRepoKind(nil))
	assert.Nil(t, classifyRepoKind(treeOf("", "cmd/main.go")))
}

// TestClassifyRepoKindIsDeterministic guards reproducibility: the shape rides into the LLM call
// and the run's recorded the content store input, so a tie broken by map iteration order would make one run's
// recommendation unexplainable from another's stored context.
func TestClassifyRepoKindIsDeterministic(t *testing.T) {
	t.Parallel()
	tree := treeOf("acme/core",
		"cmd/main.go", "internal/a.go", "terraform/main.tf", "helm/values.yaml", "docs/x.md")
	first := classifyRepoKind(tree)
	for range 20 {
		got := classifyRepoKind(tree)
		assert.Equal(t, first.GetShape(), got.GetShape())
		assert.Equal(t, first.GetShapeEvidence(), got.GetShapeEvidence())
	}
}
