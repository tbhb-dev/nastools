// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package redact_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wiringPath is the file whose main function hands the process streams to Run.
const wiringPath = "../../cmd/nastools/main.go"

// TestNoDirectOutput backs up the lint rule. No source outside the main
// function in main.go may use a name that writes to a process stream past
// the gate.
func TestNoDirectOutput(t *testing.T) {
	t.Parallel()
	for _, path := range sources(t) {
		assert.Empty(t, directOutput(t, outsideWiring(path, parse(t, path, nil))), path)
	}
}

// TestDirectOutputOutsideWiring checks that the exemption covers only the
// main function in main.go, not an adapter beside it or another file.
func TestDirectOutputOutsideWiring(t *testing.T) {
	t.Parallel()
	source := `package main

import (
	"os"
	process "os/exec"
)

func main() {
	run(os.Stdout, os.Stderr)
}

func (*operations) Compose() error {
	cmd := process.Command("docker", "compose")
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
`
	assert.Equal(t, []string{"os.Stderr"}, directOutput(t, outsideWiring(wiringPath, parse(t, wiringPath, source))))
	other := "../../cmd/nastools/run.go"
	assert.Equal(t, []string{"os.Stdout", "os.Stderr", "os.Stderr"},
		directOutput(t, outsideWiring(other, parse(t, other, source))))
}

// outsideWiring drops the main function from main.go, so every other
// declaration there stays checked.
func outsideWiring(path string, file *ast.File) *ast.File {
	if filepath.ToSlash(path) == wiringPath {
		file.Decls = slices.DeleteFunc(file.Decls, func(decl ast.Decl) bool {
			fn, ok := decl.(*ast.FuncDecl)
			return ok && fn.Recv == nil && fn.Name.Name == "main"
		})
	}
	return file
}

func TestDirectOutputFindsEachForm(t *testing.T) {
	t.Parallel()
	source := `package example

import (
	"log"
	stdout "os"
	dbg "runtime/debug"
	"syscall"

	"golang.org/x/sys/unix"
)

func example() {
	logger := log.Default()
	logger.Println("x")
	_ = log.Output(1, "x")
	_ = log.Writer()
	dbg.PrintStack()
	_, _ = unix.Write(unix.Stderr, nil)
	_, _, _ = syscall.Syscall(syscall.SYS_WRITE, 2, 0, 0)
	_ = stdout.Stdout
	println("x")
}
`
	file := parse(t, "example.go", source)
	assert.Equal(t, []string{
		"log.Default", "log.Output", "log.Writer", "runtime/debug.PrintStack",
		"golang.org/x/sys/unix.Write", "golang.org/x/sys/unix.Stderr", "syscall.Syscall", "os.Stdout",
		"println",
	}, directOutput(t, file))
}

// directOutput lists each name that file uses, through any import name,
// that writes to a process stream directly, and each call of a printing
// built-in. Forbidding log.Default also covers its methods.
func directOutput(t *testing.T, file *ast.File) []string {
	t.Helper()
	forbidden := map[string][]string{
		"fmt": {"Print", "Printf", "Println"},
		"log": {
			"Print",
			"Printf",
			"Println",
			"Fatal",
			"Fatalf",
			"Fatalln",
			"Panic",
			"Panicf",
			"Panicln",
			"Default",
			"Output",
			"Writer",
		},
		"log/slog": {
			"Debug",
			"Info",
			"Warn",
			"Error",
			"Log",
			"LogAttrs",
			"Default",
			"DebugContext",
			"InfoContext",
			"WarnContext",
			"ErrorContext",
		},
		"os":            {"Stdout", "Stderr", "NewFile"},
		"runtime/debug": {"PrintStack"},
		"syscall": {
			"Stdout",
			"Stderr",
			"Write",
			"Pwrite",
			"Syscall",
			"Syscall6",
			"Syscall9",
			"RawSyscall",
			"RawSyscall6",
		},
		"golang.org/x/sys/unix": {
			"Stdout", "Stderr", "Write", "Writev", "Pwrite", "Pwritev", "Syscall", "Syscall6", "Syscall9",
			"RawSyscall", "RawSyscall6", "SyscallNoError", "RawSyscallNoError",
		},
	}
	imports := make(map[string]string)
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		require.NoError(t, err)
		name := filepath.Base(path)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		imports[name] = path
	}
	var found []string
	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.SelectorExpr:
			if pkg, ok := n.X.(*ast.Ident); ok && slices.Contains(forbidden[imports[pkg.Name]], n.Sel.Name) {
				found = append(found, imports[pkg.Name]+"."+n.Sel.Name)
			}
		case *ast.CallExpr:
			if fun, ok := n.Fun.(*ast.Ident); ok && (fun.Name == "print" || fun.Name == "println") {
				found = append(found, fun.Name)
			}
		}
		return true
	})
	return found
}

// TestNoStreamDevicePaths backs up the lint rule, which can't see call
// arguments. No source may contain a device path, such as a `/dev/stderr`
// argument to os.OpenFile, or the pieces of one.
func TestNoStreamDevicePaths(t *testing.T) {
	t.Parallel()
	for _, path := range sources(t) {
		for _, literal := range stringLiterals(t, path) {
			for _, device := range []string{"/dev/", "/proc/self/"} {
				assert.NotContains(t, literal, device, path)
			}
			assert.NotEqual(t, "/dev", strings.TrimSuffix(literal, "/"), path)
		}
	}
}

// sources lists every Go source outside tests and vendor.
func sources(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("..", "..")
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		switch {
		case entry.IsDir() && path != root && (name == "vendor" || strings.HasPrefix(name, ".")):
			return filepath.SkipDir
		case strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go"):
			paths = append(paths, path)
		}
		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, paths)
	return paths
}

func parse(t *testing.T, path string, source any) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, source, parser.SkipObjectResolution)
	require.NoError(t, err)
	return file
}

func stringLiterals(t *testing.T, path string) []string {
	t.Helper()
	var literals []string
	ast.Inspect(parse(t, path, nil), func(node ast.Node) bool {
		if literal, ok := node.(*ast.BasicLit); ok && literal.Kind == token.STRING {
			value, unquoteErr := strconv.Unquote(literal.Value)
			require.NoError(t, unquoteErr)
			literals = append(literals, value)
		}
		return true
	})
	return literals
}
