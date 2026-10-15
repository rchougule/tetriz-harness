// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
)

// strippedDirSegments are path segments whose subtree is generated / vendored /
// binary — not citable source, so excluded from the tree (design notes #14b/#50).
// Matched as a path segment (a "/foo/" or leading "foo/" component) so "vendor"
// the directory is stripped but "vendor_client.go" the file is not.
var strippedDirSegments = map[string]struct{}{
	"node_modules": {}, "vendor": {}, "dist": {}, "build": {}, "out": {},
	".git": {}, ".idea": {}, ".vscode": {}, "target": {}, "bin": {},
	"__pycache__": {}, ".next": {}, "coverage": {}, ".gradle": {}, "Pods": {},
}

// strippedExtensions are file extensions for generated / minified / binary
// artifacts — not citable source (design notes #14b/#50). Includes generated proto
// stubs (*.pb.go matches via the ".pb.go" suffix check, see isGenerated).
var strippedExtensions = map[string]struct{}{
	".min.js": {}, ".min.css": {}, ".map": {},
	".png": {}, ".jpg": {}, ".jpeg": {}, ".gif": {}, ".svg": {}, ".ico": {},
	".webp": {}, ".pdf": {}, ".zip": {}, ".gz": {}, ".tar": {}, ".jar": {},
	".woff": {}, ".woff2": {}, ".ttf": {}, ".eot": {}, ".mp4": {}, ".mov": {},
	".lock": {}, ".sum": {}, ".class": {}, ".o": {}, ".a": {}, ".so": {}, ".dll": {},
	".pyc": {}, ".pyo": {},
}

// strippedBasenames are exact filenames that are lockfiles / generated manifests —
// not citable source.
var strippedBasenames = map[string]struct{}{
	"package-lock.json": {}, "yarn.lock": {}, "pnpm-lock.yaml": {},
	"go.sum": {}, "composer.lock": {}, "gemfile.lock": {}, "poetry.lock": {},
	"cargo.lock": {},
}

// extensionLanguage maps a source-file extension to its language name, for the
// primary_language / language_hints signal. Unmapped extensions don't count.
var extensionLanguage = map[string]string{
	".go": "Go", ".ts": "TypeScript", ".tsx": "TypeScript", ".js": "JavaScript",
	".jsx": "JavaScript", ".py": "Python", ".java": "Java", ".kt": "Kotlin",
	".rb": "Ruby", ".rs": "Rust", ".c": "C", ".h": "C", ".cpp": "C++",
	".cc": "C++", ".hpp": "C++", ".cs": "C#", ".php": "PHP", ".swift": "Swift",
	".scala": "Scala", ".sql": "SQL", ".sh": "Shell", ".proto": "Protocol Buffers",
	".m": "Objective-C", ".dart": "Dart", ".ex": "Elixir", ".clj": "Clojure",
}

// repoTreeBlob is the stable JSON contract for a repository's paths-only tree
// document (the shape a VCS ingest writes into vcs_repositories.tree_content).
type repoTreeBlob struct {
	TreeSHA   string          `json:"tree_sha"`
	Branch    string          `json:"branch"`
	FetchedAt time.Time       `json:"fetched_at"`
	Truncated bool            `json:"truncated"`
	Entries   []repoTreeEntry `json:"entries"`
}

// repoTreeEntry is one node of the flat recursive tree. Type is "blob" (file) or
// "tree" (directory); only blobs carry citable paths.
type repoTreeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	Size *int64 `json:"size,omitempty"`
	// SHA is the file's git blob SHA — the per-file change-detection baseline for VCS
	// content ingestion. Empty when the source tree carries no per-entry SHA.
	SHA string `json:"sha"`
}

// blobEntryType is repoTreeEntry.Type for a file; a directory is "tree". Only blobs carry
// citable paths, so every walk skips non-blob entries.
const blobEntryType = "blob"

// EncodeRepoTree compacts a connector tree blob into a RepoTree (design notes
// #14b/#50): it strips non-citable (generated/binary/vendored) files, detects
// the languages and existing rule-file paths, and builds the directory-grouped,
// paths-only, lossless directory_structure — each directory prefix written once
// (~½ the tokens of flat paths). If the encoding exceeds maxTokens (> 0) it is
// truncated deterministically and truncated=true with reason token_cap; a blob
// the vendor already truncated is used as-is with reason vendor. The second
// return is the reason for the warn metric (TruncationNone when complete).
//
// repoFullName and primaryLanguageHint come from vcs_repositories (the vendor's
// detected language seeds primary_language; the per-extension counts fill the
// hints and provide a fallback when the hint is empty).
func EncodeRepoTree(rawBlob []byte, repoFullName, primaryLanguageHint string, maxTokens int) (*promptlibraryv1.RepoTree, TruncationReason, error) {
	// maxTokens bounds the ENCODED OUTPUT (directory_structure), not this input parse:
	// the whole blob is unmarshalled and walked first. Input size is bounded upstream
	// by the vendor git-tree API, which truncates very large trees and marks the blob
	// truncated=true (the TruncationVendor path) — so this parse is not the OOM
	// backstop for a pathological multi-hundred-MB blob.
	var blob repoTreeBlob
	if err := json.Unmarshal(rawBlob, &blob); err != nil {
		return nil, TruncationNone, fmt.Errorf("decoding repo-tree blob for %s: %w", repoFullName, err)
	}

	// Collect kept source paths; tally languages and rule files in the same pass.
	keptPaths := make([]string, 0, len(blob.Entries))
	ruleFiles := make([]string, 0)
	langCounts := make(map[string]int)
	for _, entry := range blob.Entries {
		if entry.Type != blobEntryType {
			continue // directories are implicit in the paths
		}
		if isStrippedPath(entry.Path) {
			continue
		}
		keptPaths = append(keptPaths, entry.Path)
		if isRuleFile(entry.Path) {
			ruleFiles = append(ruleFiles, entry.Path)
		}
		if lang, ok := extensionLanguage[strings.ToLower(path.Ext(entry.Path))]; ok {
			langCounts[lang]++
		}
	}

	primaryLanguage, languageHints := resolveLanguages(primaryLanguageHint, langCounts)
	directoryStructure, truncatedByCap := encodeDirectoryGrouped(keptPaths, maxTokens)

	reason := TruncationNone
	switch {
	case truncatedByCap:
		reason = TruncationTokenCap
	case blob.Truncated:
		reason = TruncationVendor
	}

	sort.Strings(ruleFiles)
	tree := &promptlibraryv1.RepoTree{
		RepoFullName:    repoFullName,
		PrimaryLanguage: primaryLanguage,
		LanguageHints:   languageHints,
		// FileCount is the full kept-source count — it does NOT shrink when
		// directory_structure is token-capped (Truncated then signals the listing
		// is partial), so it stays an accurate repo-size signal.
		FileCount:          int32(len(keptPaths)),
		RuleFiles:          ruleFiles,
		DirectoryStructure: directoryStructure,
		Truncated:          truncatedByCap || blob.Truncated,
	}
	if !blob.FetchedAt.IsZero() {
		tree.TreeFetchedAt = timestamppb.New(blob.FetchedAt)
	}
	return tree, reason, nil
}

// isStrippedPath reports whether a file path is generated / vendored / binary and
// must be excluded (design notes #14b/#50).
func isStrippedPath(filePath string) bool {
	base := strings.ToLower(path.Base(filePath))
	if _, ok := strippedBasenames[base]; ok {
		return true
	}
	if _, ok := strippedExtensions[lowerExt(base)]; ok {
		return true
	}
	if isGeneratedStub(base) {
		return true
	}
	return hasStrippedDirSegment(filePath)
}

// isGeneratedStub reports whether a file basename is a generated proto/grpc stub (*.pb.go,
// *_pb.ts, *.pb.gw.go, *_pb2.py) — not authored content, so skipped both by the whole-repo strip
// and inside an artifact package (the package policy stores authored text, not generated blobs).
func isGeneratedStub(base string) bool {
	return strings.HasSuffix(base, ".pb.go") || strings.HasSuffix(base, "_pb.ts") ||
		strings.HasSuffix(base, ".pb.gw.go") || strings.HasSuffix(base, "_pb2.py")
}

// hasStrippedDirSegment reports whether any path segment is a stripped dir (node_modules,
// vendor, dist, …) — the directory-segment half of isStrippedPath, without its basename/ext
// checks. Kept separate so a detected artifact package can be rejected when its ancestor tree
// is stripped, without applying the file-level strip to the package's own authored members.
func hasStrippedDirSegment(filePath string) bool {
	for _, segment := range strings.Split(filePath, "/") {
		if _, ok := strippedDirSegments[segment]; ok {
			return true
		}
	}
	return false
}

// hasHiddenSegment reports whether any segment of a convention-relative path starts with "." —
// a dot-prefixed entry every desktop collector loop (and walkSkillsDir) skips, so it is not a
// loadable artifact and must not be enumerated from a committed tree either.
func hasHiddenSegment(relPath string) bool {
	for _, segment := range strings.Split(relPath, "/") {
		if strings.HasPrefix(segment, ".") {
			return true
		}
	}
	return false
}

// lowerExt returns the compound or simple extension used for the stripped-ext
// lookup: it tries the two-part suffix (".min.js") before the simple ext (".js").
func lowerExt(base string) string {
	parts := strings.Split(base, ".")
	if len(parts) >= 3 {
		twoPart := "." + parts[len(parts)-2] + "." + parts[len(parts)-1]
		if _, ok := strippedExtensions[twoPart]; ok {
			return twoPart
		}
	}
	return path.Ext(base)
}

// resolveLanguages picks the primary language and the hint list. The vendor's
// detected language (hint) wins as primary when present; otherwise the
// most-frequent source extension does. Hints are the remaining languages by
// descending file count, then name for stable ordering.
func resolveLanguages(primaryHint string, langCounts map[string]int) (primary string, hints []string) {
	ranked := make([]string, 0, len(langCounts))
	for lang := range langCounts {
		ranked = append(ranked, lang)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if langCounts[ranked[i]] != langCounts[ranked[j]] {
			return langCounts[ranked[i]] > langCounts[ranked[j]]
		}
		return ranked[i] < ranked[j]
	})

	primary = primaryHint
	if primary == "" && len(ranked) > 0 {
		primary = ranked[0]
	}
	for _, lang := range ranked {
		if lang != primary {
			hints = append(hints, lang)
		}
	}
	return primary, hints
}

// encodeDirectoryGrouped renders the kept paths as the directory-grouped,
// paths-only, lossless structure: each directory written once,
// then its space-free file basenames, one line per directory, sorted —
// "<dir>: a b c" expands to "<dir>/a", "<dir>/b", … (the "." key holds repo-root
// files). A basename containing a space can't be space-joined unambiguously, so it
// is emitted as its own standalone full-path line (no ": "), after its directory's
// grouped line when that dir also has space-free files (or alone when it does not);
// the decoder reads a ": "-free line verbatim, so spaces round-trip losslessly
// while the common case keeps its ~½-token grouping. (A
// basename containing the literal substring ": " is the sole remaining
// non-round-trip — colon-space in a filename is effectively nonexistent in source
// repos.) When maxTokens > 0 and the estimate exceeds it, trailing lines are
// dropped (deterministic, sorted order) and the second return is true.
func encodeDirectoryGrouped(paths []string, maxTokens int) (string, bool) {
	if len(paths) == 0 {
		return "", false
	}
	filesByDir := make(map[string][]string)
	for _, p := range paths {
		dir := path.Dir(p)
		filesByDir[dir] = append(filesByDir[dir], path.Base(p))
	}
	dirs := make([]string, 0, len(filesByDir))
	for dir := range filesByDir {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	// Build all lines (grouped + standalone) in deterministic order, then apply the
	// char cap line-by-line so truncation drops whole trailing lines.
	var lines []string
	for _, dir := range dirs {
		files := filesByDir[dir]
		sort.Strings(files)
		inline := make([]string, 0, len(files))
		var standalone []string
		for _, base := range files {
			if strings.ContainsRune(base, ' ') {
				standalone = append(standalone, joinDirBase(dir, base))
			} else {
				inline = append(inline, base)
			}
		}
		if len(inline) > 0 {
			lines = append(lines, dir+": "+strings.Join(inline, " "))
		}
		lines = append(lines, standalone...) // already sorted (files was sorted)
	}

	var builder strings.Builder
	truncated := false
	maxChars := maxTokens * charsPerToken
	for _, line := range lines {
		withNewline := line + "\n"
		if maxTokens > 0 && builder.Len()+len(withNewline) > maxChars {
			truncated = true
			break
		}
		builder.WriteString(withNewline)
	}
	return strings.TrimRight(builder.String(), "\n"), truncated
}

// joinDirBase reconstructs a full path from a directory key and a basename; the
// "." root key yields a bare basename.
func joinDirBase(dir, base string) string {
	if dir == "." {
		return base
	}
	return dir + "/" + base
}

// DecodeRepoTreePaths reconstructs the set of repo file paths from a RepoTree's
// directory_structure — the lossless inverse of encodeDirectoryGrouped. A grouped
// line "<dir>: a b c" expands to "<dir>/a", "<dir>/b", "<dir>/c" (the "." dir holds
// repo-root files); a line with no ": " is a standalone full path (a basename with
// a space), read verbatim. The quality gate uses the returned set to ground cited
// paths against the project tree. A nil tree or empty directory_structure yields an
// empty (non-nil) set. When the encoding was token-capped (tree.Truncated), only the
// paths still present in the listing are returned — a citation to a dropped path
// then fails grounding, the intended conservative behavior for a partial tree.
func DecodeRepoTreePaths(tree *promptlibraryv1.RepoTree) map[string]struct{} {
	paths := make(map[string]struct{})
	if tree == nil || tree.GetDirectoryStructure() == "" {
		return paths
	}
	for _, line := range strings.Split(tree.GetDirectoryStructure(), "\n") {
		dir, fileList, found := strings.Cut(line, ": ")
		if !found {
			if line != "" {
				paths[line] = struct{}{} // standalone full path (space-containing basename)
			}
			continue
		}
		for _, base := range strings.Fields(fileList) {
			paths[joinDirBase(dir, base)] = struct{}{}
		}
	}
	return paths
}
