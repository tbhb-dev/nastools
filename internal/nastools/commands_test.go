// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package nastools_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tbhb-dev/nastools/internal/nastools"
)

func TestRemoteCommandPolicy(t *testing.T) {
	t.Parallel()
	base := []string{
		"--project-directory",
		".",
		"--project-name",
		"example",
		"--file",
		"compose.yaml",
		"--file",
		"/config/override.yaml",
	}
	for _, tc := range []struct {
		name    string
		command nastools.Command
		want    [][]string
	}{
		{"deploy", nastools.Command{Name: "deploy"}, [][]string{
			{"config", "-q"},
			{"pull", "--ignore-buildable"},
			{"up", "--wait", "--wait-timeout", "180", "--remove-orphans"},
			{"ps"},
		}},
		{"status", nastools.Command{Name: "status"}, [][]string{{"ps"}}},
		{"logs", nastools.Command{Name: "logs", Service: "--example", Tail: 25, Follow: true}, [][]string{{"logs", "--tail", "25", "--follow", "--", "--example"}}},
		{"all logs", nastools.Command{Name: "logs"}, [][]string{{"logs", "--tail", "0"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeOperations{files: map[string]string{"nastools.toml": strings.Split(configText, "[files]")[0]}}
			env := environment()
			env["GITHUB_ACTIONS"] = "true"
			env["DOCKER_HOST"] = "tcp://example.invalid:2375"
			env["DOCKER_CONTEXT"] = "example.invalid"
			require.NoError(t, nastools.Execute(t.Context(), f, "nastools.toml", env, &f.out, &f.out, tc.command))
			assertCommands(t, f, base, tc.want, env)
			assert.Contains(t, f.out.String(), "::add-mask::"+strings.ReplaceAll(knownHostsText, "\n", "%0A"))
			assert.Len(t, f.commands, len(tc.want))
			assert.Equal(t, 1, f.closed)
			assert.Equal(t, knownHostsText, f.knownHosts)
			masks := nastools.MaskCommands(nastools.PrivateValues(config(), env), true)
			assert.True(t, strings.HasPrefix(f.out.String(), masks+"compose output\ncompose diagnostic\n"))
			if tc.name == "deploy" {
				assert.Equal(
					t,
					[]string{
						"read:nastools.toml",
						"git",
						"compose",
						"open",
						"lock",
						"compose",
						"compose",
						"compose",
						"close",
					},
					f.calls,
				)
				assert.Equal(t, []string{"/example.invalid/locks/example.lock"}, f.lockPaths)
			} else {
				assert.Empty(t, f.lockPaths)
			}
		})
	}
}

func TestRemoteFailureCleanup(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		change  func(*fakeOperations)
		command nastools.Command
		want    []string
		closed  int
	}{
		{"input", func(f *fakeOperations) { f.files = nil }, nastools.Command{Name: "deploy"}, []string{"read:nastools.toml"}, 0},
		{"config", func(f *fakeOperations) { f.failCommand = "config" }, nastools.Command{Name: "deploy"}, []string{"read:nastools.toml", "git", "compose"}, 0},
		{"open", func(f *fakeOperations) { f.openErr = errPrivate }, nastools.Command{Name: "deploy"}, []string{"read:nastools.toml", "git", "compose", "open", "close"}, 1},
		{"lock", func(f *fakeOperations) { f.lockErrors = []error{errPrivate} }, nastools.Command{Name: "deploy"}, []string{"read:nastools.toml", "git", "compose", "open", "lock", "close"}, 1},
		{"pull", func(f *fakeOperations) { f.failCommand = "pull" }, nastools.Command{Name: "deploy"}, []string{"read:nastools.toml", "git", "compose", "open", "lock", "compose", "close"}, 1},
		{"up", func(f *fakeOperations) { f.failCommand = "up" }, nastools.Command{Name: "deploy"}, []string{"read:nastools.toml", "git", "compose", "open", "lock", "compose", "compose", "close"}, 1},
		{"ps", func(f *fakeOperations) { f.failCommand = "ps" }, nastools.Command{Name: "status"}, []string{"read:nastools.toml", "git", "open", "compose", "close"}, 1},
		{"logs interrupted", func(f *fakeOperations) { f.composeErr = context.Canceled }, nastools.Command{Name: "logs", Follow: true}, []string{"read:nastools.toml", "git", "open", "compose", "close"}, 1},
		{"negative wait", func(*fakeOperations) {}, nastools.Command{Name: "deploy", LockWait: -time.Second}, []string{"read:nastools.toml", "git"}, 0},
		{"negative tail", func(*fakeOperations) {}, nastools.Command{Name: "logs", Tail: -1}, []string{"read:nastools.toml", "git"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeOperations{files: map[string]string{"nastools.toml": strings.Split(configText, "[files]")[0]}}
			tc.change(f)
			env := environment()
			env["GITHUB_ACTIONS"] = "true"
			env["NASTOOLS_PRIVATE_STRINGS"] = "extra.example.invalid"
			err := nastools.Execute(t.Context(), f, "nastools.toml", env, &f.out, &f.out, tc.command)
			require.ErrorIs(t, err, nastools.ErrInvalid)
			assert.NotContains(t, err.Error(), errPrivate.Error())
			assert.Equal(t, tc.want, f.calls)
			assert.Equal(t, tc.closed, f.closed)
			c := config()
			if tc.name == "input" {
				c = nastools.Config{}
			}
			assert.True(
				t,
				strings.HasPrefix(f.out.String(), nastools.MaskCommands(nastools.PrivateValues(c, env), true)),
			)
		})
	}
}

func TestLockWaitPolicy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		wait     time.Duration
		errors   []error
		pauseErr error
		attempts int
		wantBusy bool
	}{
		{"busy", 0, []error{nastools.ErrBusy}, nil, 1, true},
		{"retry", time.Minute, []error{nastools.ErrBusy, nastools.ErrBusy, nil}, nil, 3, false},
		{"deadline", time.Minute, []error{nastools.ErrBusy}, context.DeadlineExceeded, 1, true},
		{"cancel", time.Minute, []error{nastools.ErrBusy}, context.Canceled, 1, true},
		{"failure is not busy", time.Minute, []error{errPrivate}, nil, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeOperations{
				files:      map[string]string{"nastools.toml": strings.Split(configText, "[files]")[0]},
				lockErrors: tc.errors,
				pauseErr:   tc.pauseErr,
			}
			err := nastools.Execute(
				t.Context(),
				f,
				"nastools.toml",
				environment(),
				&f.out,
				&f.out,
				nastools.Command{Name: "deploy", LockWait: tc.wait},
			)
			if tc.name == "retry" {
				require.NoError(t, err)
				assert.Len(t, f.commands, 4)
			} else {
				require.ErrorIs(t, err, nastools.ErrInvalid)
				assert.Len(t, f.commands, 1)
			}
			if tc.wantBusy {
				require.ErrorIs(t, err, nastools.ErrBusy)
			}
			assert.Len(t, f.lockPaths, tc.attempts)
			assert.Equal(t, 1, f.closed)
		})
	}
}

func TestRemoteMaskFailure(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"deploy", "status", "logs"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := &fakeOperations{files: map[string]string{"nastools.toml": configText}}
			env := environment()
			env["GITHUB_ACTIONS"] = "true"
			err := nastools.Execute(
				t.Context(),
				f,
				"nastools.toml",
				env,
				failingWriter{},
				&f.out,
				nastools.Command{Name: name},
			)
			require.ErrorContains(t, err, "mask output")
			assert.Equal(t, []string{"read:nastools.toml"}, f.calls)
		})
	}
}

func assertCommands(t *testing.T, f *fakeOperations, base []string, want [][]string, env map[string]string) {
	t.Helper()
	for i, command := range want {
		assert.Equal(t, append(slices.Clone(base), command...), f.commands[i])
		for _, private := range nastools.PrivateValues(config(), env) {
			assert.NotContains(t, strings.Join(f.commands[i], " "), private)
		}
		if command[0] == "config" {
			assert.Contains(t, f.environments[i], "DOCKER_CONTEXT=example.invalid")
			continue
		}
		assert.Contains(t, f.environments[i], "DOCKER_HOST=unix:///example.invalid/socket")
		assert.Contains(t, f.environments[i], "REQUIRED_INPUT=required.example.invalid")
		for _, entry := range f.environments[i] {
			assert.False(t, strings.HasPrefix(entry, "DOCKER_CONTEXT="))
		}
	}
}
