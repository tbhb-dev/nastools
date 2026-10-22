// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package redact_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNoStreamDevicePaths backs up the lint rule, which can't see call
// arguments. No source outside main.go may contain a device path of the
// process streams, such as a `/dev/stderr` argument to os.OpenFile.
func TestNoStreamDevicePaths(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	var sources []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		switch {
		case entry.IsDir() && path != root && (name == "vendor" || strings.HasPrefix(name, ".")):
			return filepath.SkipDir
		case strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") &&
			filepath.ToSlash(path) != "../../cmd/nastools/main.go":
			sources = append(sources, path)
		}
		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, sources)
	for _, path := range sources {
		for _, literal := range stringLiterals(t, path) {
			for _, device := range []string{"/dev/stdout", "/dev/stderr", "/dev/fd/", "/proc/self/fd/"} {
				assert.NotContains(t, literal, device, path)
			}
		}
	}
}

func stringLiterals(t *testing.T, path string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	require.NoError(t, err)
	var literals []string
	ast.Inspect(file, func(node ast.Node) bool {
		if literal, ok := node.(*ast.BasicLit); ok && literal.Kind == token.STRING {
			value, unquoteErr := strconv.Unquote(literal.Value)
			require.NoError(t, unquoteErr)
			literals = append(literals, value)
		}
		return true
	})
	return literals
}
