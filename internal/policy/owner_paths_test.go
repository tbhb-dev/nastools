// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

// Package policy_test checks the owner paths in .github/CODEOWNERS and
// .agents/agent-skills.conf against the tracked files: each names files
// that exist, and every file CODEOWNERS names is also in `owner_paths`.
package policy_test

import (
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// repoRoot is the repository root relative to this package's directory,
// where go test runs.
const repoRoot = "../.."

func TestCodeownersRulesMatchTrackedFiles(t *testing.T) {
	t.Parallel()
	files := trackedFiles(t)
	for _, pattern := range codeownersPatterns(t) {
		assert.NotEmpty(t, codeownersMatches(t, pattern, files), "CODEOWNERS rule %q matches no tracked file", pattern)
	}
}

func TestOwnerPathsMatchTrackedFiles(t *testing.T) {
	t.Parallel()
	files := trackedFiles(t)
	for _, alt := range ownerPathAlternatives(t) {
		assert.NotEmpty(t, regexpMatches(t, alt, files), "owner_paths entry %q matches no tracked file", alt)
	}
}

// TestCodeownersWithinOwnerPaths requires every file a CODEOWNERS rule
// matches to match `owner_paths` too. The `owner_paths` key may hold more,
// such as the workflows it keeps until their changes are classified.
func TestCodeownersWithinOwnerPaths(t *testing.T) {
	t.Parallel()
	files := trackedFiles(t)
	ownerPaths := regexp.MustCompile(ownerPathsValue(t))
	for _, pattern := range codeownersPatterns(t) {
		for _, file := range codeownersMatches(t, pattern, files) {
			assert.Regexp(t, ownerPaths, file, "CODEOWNERS rule %q names a file owner_paths lacks", pattern)
		}
	}
}

func trackedFiles(t *testing.T) []string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "ls-files", "-z")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	require.NoError(t, err)
	return strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
}

func readRepoFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, name))
	require.NoError(t, err)
	return string(data)
}

// codeownersPatterns returns the pattern of each rule in .github/CODEOWNERS.
func codeownersPatterns(t *testing.T) []string {
	t.Helper()
	var patterns []string
	for line := range strings.Lines(readRepoFile(t, ".github/CODEOWNERS")) {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		patterns = append(patterns, fields[0])
	}
	require.NotEmpty(t, patterns)
	return patterns
}

// codeownersMatches returns the files an anchored CODEOWNERS pattern
// matches. A trailing slash marks a directory; otherwise the pattern is a
// glob whose * stays within one path segment, and a match on a directory
// covers everything below it.
func codeownersMatches(t *testing.T, pattern string, files []string) []string {
	t.Helper()
	rel, ok := strings.CutPrefix(pattern, "/")
	require.True(t, ok, "CODEOWNERS rule %q is not anchored at the root", pattern)
	var matched []string
	for _, file := range files {
		if dir, isDir := strings.CutSuffix(rel, "/"); isDir {
			if strings.HasPrefix(file, dir+"/") {
				matched = append(matched, file)
			}
			continue
		}
		globbed, err := path.Match(rel, file)
		require.NoError(t, err)
		if globbed || strings.HasPrefix(file, rel+"/") {
			matched = append(matched, file)
		}
	}
	return matched
}

// ownerPathsValue returns the value of the `owner_paths` key.
func ownerPathsValue(t *testing.T) string {
	t.Helper()
	for line := range strings.Lines(readRepoFile(t, ".agents/agent-skills.conf")) {
		if v, ok := strings.CutPrefix(line, "owner_paths = "); ok {
			return strings.TrimSpace(v)
		}
	}
	require.Fail(t, "agent-skills.conf has no owner_paths key")
	return ""
}

// ownerPathAlternatives splits the `owner_paths` value, which has the form
// `^(a|b|c)`, into its top-level alternatives.
func ownerPathAlternatives(t *testing.T) []string {
	t.Helper()
	value := ownerPathsValue(t)
	inner, ok := strings.CutPrefix(value, "^(")
	require.True(t, ok, "owner_paths %q does not start with ^(", value)
	inner, ok = strings.CutSuffix(inner, ")")
	require.True(t, ok, "owner_paths %q does not end with )", value)

	var alts []string
	depth, start := 0, 0
	for i, r := range inner {
		switch r {
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case '|':
			if depth == 0 {
				alts = append(alts, inner[start:i])
				start = i + 1
			}
		}
	}
	return append(alts, inner[start:])
}

func regexpMatches(t *testing.T, alt string, files []string) []string {
	t.Helper()
	re, err := regexp.Compile("^(?:" + alt + ")")
	require.NoError(t, err)
	var matched []string
	for _, file := range files {
		if re.MatchString(file) {
			matched = append(matched, file)
		}
	}
	return matched
}
