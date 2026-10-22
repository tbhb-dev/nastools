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
	"github.com/tbhb-dev/nastools/internal/redact"
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

// Run executes the command and returns the process exit status. Every byte
// of output passes the gate, which knows the discovered values before any.
func Run(ctx context.Context, args []string, s streams, ops nastools.Operations, env map[string]string) int {
	actions := env["GITHUB_ACTIONS"] != ""
	gate := redact.New(s.out, s.err, actions, redact.TailnetName)
	ops = nastools.Cached(ops)
	path := configArgument(args)
	private := nastools.Discover(ops.ReadFile, path, env)
	gate.Add(private.Values...)
	gate.AddHosts(private.Hosts...)
	code := exitError
	if gate.Err() != nil {
		fmt.Fprintln(gate.Stderr(), "nastools: mask output:", nastools.ErrInvalid)
	} else {
		// Discovery can miss a value that a config that doesn't parse
		// declares, so no error may then echo the arguments.
		hideArgs := actions && !nastools.Parses(ops.ReadFile, path)
		code = run(ctx, args, streams{in: s.in, out: gate.Stdout(), err: gate.Stderr()}, ops, env, hideArgs)
	}
	if gate.Close() != nil {
		return exitError
	}
	return code
}

func run(ctx context.Context, args []string, s streams, ops nastools.Operations, env map[string]string,
	hideArgs bool,
) int {
	// Cobra's hidden completion commands write diagnostics straight to the
	// process's standard error, past the gate.
	if slices.ContainsFunc(args, func(arg string) bool {
		return arg == cobra.ShellCompRequestCmd || arg == cobra.ShellCompNoDescRequestCmd
	}) {
		fmt.Fprintln(s.err, "nastools:", nastools.ErrInvalid)
		return exitError
	}
	cmd := newRootCmd(ctx, s, ops, env)
	cmd.SetArgs(args)
	target, err := cmd.ExecuteContextC(ctx)
	if err != nil {
		remote := slices.Contains([]string{nastools.CheckName, nastools.Deploy, "status", nastools.Logs}, target.Name())
		if (remote || hideArgs) && !errors.Is(err, nastools.ErrInvalid) {
			err = nastools.ErrInvalid
		}
		fmt.Fprintln(s.err, "nastools:", err)
		return exitError
	}
	return exitOK
}

// configArgument reads the config path the way the flag parser does. The
// token after the config flag is always its value, and a bare double dash
// ends the flags.
func configArgument(args []string) string {
	configPath := "nastools.toml"
	for i := 0; i < len(args) && args[i] != "--"; i++ {
		if args[i] == "--config" && i+1 < len(args) {
			i++
			configPath = args[i]
		} else if value, ok := strings.CutPrefix(args[i], "--config="); ok {
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
	root.SetHelpCommand(newHelpCmd())
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

// newHelpCmd replaces Cobra's help command, which prints an unknown topic
// back and writes its own errors straight to the process's standard error.
func newHelpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "help [command]",
		Short: "Help about any command",
		RunE: func(cmd *cobra.Command, args []string) error {
			target, _, err := cmd.Root().Find(args)
			if err != nil {
				return fmt.Errorf("help: %w", err)
			}
			target.InitDefaultHelpFlag()
			target.InitDefaultVersionFlag()
			target.HelpFunc()(target, nil)
			return nil
		},
	}
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
