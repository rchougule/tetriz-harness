// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"fmt"
	"path"
	"sort"
	"strings"

	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
)

// Every domain-shaped input the recommender reads is ORG-level — the harness profile's
// primary_domain, the org's languages — so nothing told the matcher that a deployment-config
// repo is not an application repo, and a review pipeline written for product teams could be
// recommended for one. RepoKind is that missing per-repo signal.
//
// It is decided here rather than in the prompt on purpose: a directory's presence is weak
// evidence a model will over-read (the recommend prompt correctly forbids asserting fit from
// paths), while a measured share is a fact. The tree is paths-only, so this reads file
// families, never contents.

// repoShapeFamily is one bucket of the tree's file mix. The three families are chosen to be
// mutually exclusive per path, so the shares sum to at most 1 and a dominant family means
// what it says.
type repoShapeFamily struct {
	name  string
	shape promptlibraryv1.RepoShape
	// exts matches on the path's extension, dirs on any directory segment, and bases on the
	// whole basename — a Jenkinsfile carries no extension.
	exts  map[string]struct{}
	dirs  map[string]struct{}
	bases map[string]struct{}
}

// repoShapeFamilies is the classification vocabulary. Deliberately small: a family earns its
// place only when a wrong call on it would misdirect a recommendation, and every entry here is
// a family the catalog actually distinguishes between.
var repoShapeFamilies = []repoShapeFamily{
	{
		name:  "infrastructure",
		shape: promptlibraryv1.RepoShape_REPO_SHAPE_INFRASTRUCTURE,
		exts: map[string]struct{}{
			".tf": {}, ".tfvars": {}, ".hcl": {},
		},
		dirs: map[string]struct{}{
			"terraform": {}, "helm": {}, "k8s": {}, "kubernetes": {}, "manifests": {},
			"ansible": {}, "charts": {}, "kustomize": {}, "packer": {},
		},
		bases: map[string]struct{}{
			"jenkinsfile": {}, "dockerfile": {}, "docker-compose.yml": {},
			"docker-compose.yaml": {},
		},
	},
	{
		name:  "application",
		shape: promptlibraryv1.RepoShape_REPO_SHAPE_APPLICATION,
		exts: map[string]struct{}{
			".go": {}, ".ts": {}, ".tsx": {}, ".js": {}, ".jsx": {}, ".py": {}, ".java": {},
			".kt": {}, ".rb": {}, ".rs": {}, ".c": {}, ".cc": {}, ".cpp": {}, ".h": {},
			".hpp": {}, ".cs": {}, ".php": {}, ".swift": {}, ".scala": {}, ".ex": {},
			".dart": {}, ".m": {}, ".mm": {},
		},
	},
	{
		name:  "documentation",
		shape: promptlibraryv1.RepoShape_REPO_SHAPE_DOCUMENTATION,
		exts: map[string]struct{}{
			".md": {}, ".mdx": {}, ".rst": {}, ".adoc": {},
		},
	},
}

// Thresholds. A family names the repo only when it holds a majority of the CLASSIFIED files AND
// the runner-up is not itself substantial — a 50/50 monorepo is MIXED, while 75/25 is not, or
// nearly every real repo would come back MIXED and the signal would carry nothing. Failing
// both, the shape stays UNSPECIFIED and the recommender falls back to its other repo-fit
// signals: an honest unknown beats a confident wrong kind, because the prompt tells the model
// to trust this value over its own reading of the paths.
//
// The floor is the same discipline the rest of this pipeline uses for a bucket over a small
// sample: a share taken over three or four files is not evidence of anything, so below the
// floor no shape is decided at all. The evidence string still carries the count, so a reader
// can see why.
const (
	repoShapeDominantShare    = 0.5
	repoShapeSubstantialShare = 1.0 / 3.0
	repoShapeMinClassified    = 5
)

// classifyRepoKind decides one repo's shape from its tree. It returns nil when the tree is
// nil or names no repo — a caller carries nothing rather than an entry that claims a shape for
// a repo it cannot identify.
//
// Shares are taken over the files this vocabulary CLASSIFIED, not over the whole tree: a repo
// full of lockfiles, fixtures and images would otherwise dilute every family below the
// threshold and report UNSPECIFIED for a repo whose code-vs-config mix is perfectly clear.
// The evidence names that denominator against the whole listing, so a thin classified set
// cannot hide behind a lopsided percentage.
func classifyRepoKind(tree *promptlibraryv1.RepoTree) *promptlibraryv1.RepoKind {
	if tree == nil || tree.GetRepoFullName() == "" {
		return nil
	}
	counts := map[string]int{}
	classified := 0
	for repoPath := range DecodeRepoTreePaths(tree) {
		if family, ok := familyForPath(repoPath); ok {
			counts[family]++
			classified++
		}
	}
	kind := &promptlibraryv1.RepoKind{RepoFullName: tree.GetRepoFullName()}
	if classified == 0 {
		return kind
	}
	kind.ShapeEvidence = shareEvidence(counts, classified, len(DecodeRepoTreePaths(tree)))
	if classified < repoShapeMinClassified {
		return kind // evidence, but no decided shape — the sample cannot carry one
	}
	kind.Shape = dominantShape(counts, classified)
	return kind
}

// familyForPath returns the family a path belongs to. A path matches at most one family: the
// vocabulary is scanned in order, and infrastructure precedes application so a Dockerfile
// sitting beside source code counts once, as config.
func familyForPath(repoPath string) (string, bool) {
	lower := strings.ToLower(repoPath)
	ext := path.Ext(lower)
	base := path.Base(lower)
	segments := strings.Split(lower, "/")
	for _, family := range repoShapeFamilies {
		if _, ok := family.exts[ext]; ok {
			return family.name, true
		}
		if _, ok := family.bases[base]; ok {
			return family.name, true
		}
		for _, segment := range segments[:max(len(segments)-1, 0)] {
			if _, ok := family.dirs[segment]; ok {
				return family.name, true
			}
		}
	}
	return "", false
}

// dominantShape applies the thresholds. Ordering is by share then name so a tie is decided the
// same way on every run — an unstable shape would make the recommendation unreproducible from
// its recorded input.
func dominantShape(counts map[string]int, classified int) promptlibraryv1.RepoShape {
	type ranked struct {
		name  string
		share float64
	}
	shares := make([]ranked, 0, len(counts))
	for name, count := range counts {
		shares = append(shares, ranked{name: name, share: float64(count) / float64(classified)})
	}
	sort.Slice(shares, func(i, j int) bool {
		if shares[i].share != shares[j].share {
			return shares[i].share > shares[j].share
		}
		return shares[i].name < shares[j].name
	})
	runnerUp := 0.0
	if len(shares) > 1 {
		runnerUp = shares[1].share
	}
	if shares[0].share >= repoShapeDominantShare && runnerUp < repoShapeSubstantialShare {
		return shapeForFamily(shares[0].name)
	}
	if runnerUp >= repoShapeSubstantialShare {
		return promptlibraryv1.RepoShape_REPO_SHAPE_MIXED
	}
	return promptlibraryv1.RepoShape_REPO_SHAPE_UNSPECIFIED
}

func shapeForFamily(name string) promptlibraryv1.RepoShape {
	for _, family := range repoShapeFamilies {
		if family.name == name {
			return family.shape
		}
	}
	return promptlibraryv1.RepoShape_REPO_SHAPE_UNSPECIFIED
}

// shareEvidence renders each family's share of the CLASSIFIED files, then names that denominator
// against the whole listing — "classified 5 of 100 files". Both numbers are load-bearing and
// neither is enough alone: a share over a handful of files is a far weaker claim than the same
// share over hundreds, and a share over 5 classified files in a 100-file tree is weaker again.
// Without the second number, a repo holding five Go files beside ninety-five images reads as
// "application 100%", which invites exactly the over-trust the percentage was meant to justify.
// Ordered so the output is stable.
func shareEvidence(counts map[string]int, classified, total int) []string {
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)
	evidence := make([]string, 0, len(names)+1)
	for _, name := range names {
		evidence = append(evidence, fmt.Sprintf("%s %d%%", name, int(100*float64(counts[name])/float64(classified))))
	}
	return append(evidence, fmt.Sprintf("classified %d of %d files", classified, total))
}
