// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/tbhb-dev/nastools/internal/buildmeta"
	"github.com/tbhb-dev/nastools/internal/nastools"
)

// Exit statuses.
const (
	exitOK    = 0
	exitError = 2
)

// streams gathers the process I/O so tests can drive the command without
// touching the process's standard input and output.
type streams struct {
	in  io.Reader
	out io.Writer
	err io.Writer
}

// Run executes the command and returns the process exit status.
func Run(args []string, s streams, ops nastools.Operations, env map[string]string) int {
	cmd := newRootCmd(s, ops, env)
	cmd.SetArgs(args)

	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(s.err, "nastools:", err)
		return exitError
	}
	return exitOK
}

func newRootCmd(s streams, ops nastools.Operations, env map[string]string) *cobra.Command {
	root := &cobra.Command{
		Use:           "nastools",
		Short:         "The nastools command-line tool",
		Version:       buildmeta.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
	}
	root.SetIn(s.in)
	root.SetOut(s.out)
	root.SetErr(s.err)
	root.AddCommand(newVersionCmd())
	path := "nastools.toml"
	check := &cobra.Command{
		Use:   "check",
		Short: "Validate deployment inputs and the Compose model",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return nastools.Check(cmd.Context(), ops, path, env, cmd.OutOrStdout())
		},
	}
	check.Flags().StringVar(&path, "config", path, "Configuration file")
	root.AddCommand(check)

	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the build version, commit, and date",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := buildmeta.Get()
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "nastools %s\ncommit: %s\ndate:   %s\n",
				info.Version, info.Commit, info.Date)
			if err != nil {
				return fmt.Errorf("write version: %w", err)
			}
			return nil
		},
	}
}
