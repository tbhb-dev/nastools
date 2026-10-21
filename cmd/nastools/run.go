// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

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
func Run(ctx context.Context, args []string, s streams, ops nastools.Operations, env map[string]string) int {
	cmd := newRootCmd(ctx, s, ops, env)
	cmd.SetArgs(args)

	target, flags, findErr := cmd.Find(args)
	remote := findErr == nil &&
		slices.Contains([]string{nastools.CheckName, nastools.Deploy, "status", nastools.Logs}, target.Name())
	if remote {
		// Find the config even when flag parsing stops before reaching it.
		target.InitDefaultHelpFlag()
		parseErr := target.ParseFlags(flags)
		if parseErr != nil || target.ValidateArgs(target.Flags().Args()) != nil || target.Flags().Changed("help") {
			if _, err := nastools.Load(ops, configArgument(flags), env, s.out); err != nil {
				fmt.Fprintln(s.err, "nastools:", err)
				return exitError
			}
		}
	}
	if err := cmd.ExecuteContext(ctx); err != nil {
		if remote && !errors.Is(err, nastools.ErrInvalid) {
			err = nastools.ErrInvalid
		}
		fmt.Fprintln(s.err, "nastools:", err)
		return exitError
	}
	return exitOK
}

func configArgument(flags []string) string {
	configPath := "nastools.toml"
	for i, flag := range flags {
		if flag == "--" {
			break
		}
		if flag == "--config" && i+1 < len(flags) {
			configPath = flags[i+1]
		} else if value, ok := strings.CutPrefix(flag, "--config="); ok {
			configPath = value
		}
	}
	return configPath
}

func newRootCmd(ctx context.Context, s streams, ops nastools.Operations, env map[string]string) *cobra.Command {
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
	for _, name := range []string{nastools.CheckName, nastools.Deploy, "status", nastools.Logs} {
		const defaultTail = 100
		request := nastools.Command{Name: name, Tail: defaultTail}
		configPath := "nastools.toml"
		command := &cobra.Command{
			Use: name, Short: "Run Compose " + name, Args: cobra.NoArgs,
			RunE: func(_ *cobra.Command, args []string) error {
				if len(args) > 0 {
					request.Service = args[0]
				}
				return nastools.Execute(ctx, ops, configPath, env, s.out, s.err, request)
			},
		}
		command.Flags().StringVar(&configPath, "config", configPath, "Configuration file")
		if name == nastools.Deploy {
			command.Flags().
				DurationVar(&request.LockWait, "lock-wait", 0, "Time to wait for a deployment")
		}
		if name == nastools.Logs {
			command.Use = "logs [service]"
			command.Args = cobra.MaximumNArgs(1)
			command.Flags().IntVar(&request.Tail, "tail", request.Tail, "Lines per service")
			command.Flags().BoolVar(&request.Follow, "follow", false, "Follow log output")
		}
		root.AddCommand(command)
	}

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
