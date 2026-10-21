// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

// Command nastools is the nastools command-line tool.
//
// See Run in run.go.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	process "os/exec"

	"github.com/tbhb-dev/nastools/internal/nastools"
)

func main() {
	env := make(map[string]string)
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		env[name] = value
	}
	os.Exit(Run(os.Args[1:], streams{in: os.Stdin, out: os.Stdout, err: os.Stderr}, operations{}, env))
}

type operations struct{}

func (operations) ReadFile(path string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("read: %w", nastools.ErrInvalid)
	}
	return data, nil
}

func (operations) GitState(ctx context.Context, dir string) (nastools.GitState, error) {
	commit, err := process.CommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return nastools.GitState{}, fmt.Errorf("git commit: %w", nastools.ErrInvalid)
	}
	status, err := process.CommandContext(ctx, "git", "-C", dir, "status", "--porcelain", "--untracked-files=all").
		Output()
	if err != nil {
		return nastools.GitState{}, fmt.Errorf("git status: %w", nastools.ErrInvalid)
	}
	return nastools.GitState{Commit: strings.TrimSpace(string(commit)), Dirty: len(status) != 0}, nil
}

func (operations) Compose(ctx context.Context, args, env []string) error {
	cmd := process.CommandContext(ctx, "docker", "compose")
	cmd.Args = append(cmd.Args, args...)
	// All paths in the policy request are relative to the caller or absolute.
	// Changing cmd.Dir here would resolve a relative config directory twice.
	cmd.Env = env
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("compose: %w", nastools.ErrInvalid)
	}
	return nil
}
