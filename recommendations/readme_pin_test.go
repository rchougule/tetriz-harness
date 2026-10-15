// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// readFileRel reads a file relative to this test file, so the test is independent of the working
// directory.
func readFileRel(t *testing.T, rel string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller(0) failed")
	body, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), rel))
	require.NoErrorf(t, err, "reading %s", rel)
	return string(body)
}

// fencedBlock returns the content of a fenced code block in md: the first block of the given language
// (which == "first") or the last one (which == "last").
func fencedBlock(t *testing.T, md, lang, which string) string {
	t.Helper()
	fence := "```" + lang
	var blocks []string
	lines := strings.Split(md, "\n")
	for i := 0; i < len(lines); i++ {
		if lines[i] != fence {
			continue
		}
		var buf []string
		for j := i + 1; j < len(lines); j++ {
			if lines[j] == "```" {
				i = j
				break
			}
			buf = append(buf, lines[j])
		}
		blocks = append(blocks, strings.Join(buf, "\n"))
	}
	require.NotEmptyf(t, blocks, "no ```%s block found", lang)
	if which == "last" {
		return blocks[len(blocks)-1]
	}
	return blocks[0]
}

// TestReadmeQuickstartMatchesScanExample pins the README quickstart's scan command to the scan-a-repo
// example's command (the part before --dsn, which carries only the connection), so the two cannot
// drift. "The quickstart is the DETECTED on-disk scan" is enforced here, not trusted.
func TestReadmeQuickstartMatchesScanExample(t *testing.T) {
	stem := func(cmd string) string {
		cmd = strings.ReplaceAll(cmd, "\\", " ")     // drop shell line-continuation backslashes
		cmd = strings.Join(strings.Fields(cmd), " ") // collapse whitespace and newlines
		idx := strings.Index(cmd, "--dsn")
		require.GreaterOrEqual(t, idx, 0, "scan command has no --dsn: %q", cmd)
		return strings.TrimSpace(cmd[:idx])
	}

	readme := readFileRel(t, "README.md")
	example := readFileRel(t, "examples/scan-a-repo/README.md")

	// The README's quickstart scan command is the sh block that starts the scan; the example's is its
	// only sh block.
	var quickstart string
	for _, block := range strings.Split(readme, "```sh") {
		if strings.Contains(block, "recommendations/cmd/recommendations scan") {
			quickstart = strings.SplitN(block, "```", 2)[0]
			break
		}
	}
	require.Contains(t, quickstart, "scan", "no scan command in the README quickstart")

	require.Equal(t, stem(fencedBlock(t, example, "sh", "first")), stem(quickstart),
		"the README quickstart scan command and the scan-a-repo example command have drifted")
}

// TestReadmeExampleOutputsMatch pins the output blocks the README shows for its go-run examples to
// the examples' own README output blocks, so the README copies cannot drift from the examples the
// runner actually checks.
func TestReadmeExampleOutputsMatch(t *testing.T) {
	readme := readFileRel(t, "README.md")
	for _, name := range []string{"read-harness-inventory", "scan-from-go", "tune-detectors"} {
		exampleOut := fencedBlock(t, readFileRel(t, filepath.Join("examples", name, "README.md")), "text", "last")
		require.Containsf(t, readme, exampleOut,
			"the README's output block for %s does not match examples/%s/README.md", name, name)
	}
}
