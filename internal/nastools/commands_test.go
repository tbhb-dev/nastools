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
	"github.com/tbhb-dev/nastools/internal/nastools/nastoolstest"
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
			f := &nastoolstest.Fake{Files: map[string]string{"nastools.toml": strings.Split(configText, "[files]")[0]}}
			env := environment()
			env["DOCKER_HOST"] = "tcp://example.invalid:2375"
			for _, name := range []string{"DOCKER_CONTEXT", "DOCKER_TLS", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"} {
				env[name] = "example.invalid"
			}
			require.NoError(t, nastools.Execute(t.Context(), f, "nastools.toml", env, &f.Out, &f.Out, tc.command))
			assertCommands(t, f, base, tc.want, env)
			assert.Len(t, f.Commands, len(tc.want))
			assert.Equal(t, 1, f.Closed)
			assert.Equal(t, knownHostsText, f.KnownHosts)
			assert.True(t, strings.HasPrefix(f.Out.String(), "compose output\ncompose diagnostic\n"))
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
					f.Calls,
				)
				assert.Equal(t, []string{"/example.invalid/locks/example.lock"}, f.LockPaths)
			} else {
				assert.Empty(t, f.LockPaths)
			}
		})
	}
}

func TestRemoteFailureCleanup(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		change  func(*nastoolstest.Fake)
		command nastools.Command
		want    []string
		closed  int
	}{
		{"input", func(f *nastoolstest.Fake) { f.Files = nil }, nastools.Command{Name: "deploy"}, []string{"read:nastools.toml"}, 0},
		{"config", func(f *nastoolstest.Fake) { f.FailCommand = "config" }, nastools.Command{Name: "deploy"}, []string{"read:nastools.toml", "git", "compose"}, 0},
		{"open", func(f *nastoolstest.Fake) { f.OpenErr = nastoolstest.ErrPrivate }, nastools.Command{Name: "deploy"}, []string{"read:nastools.toml", "git", "compose", "open", "close"}, 1},
		{"lock", func(f *nastoolstest.Fake) { f.LockErrors = []error{nastoolstest.ErrPrivate} }, nastools.Command{Name: "deploy"}, []string{"read:nastools.toml", "git", "compose", "open", "lock", "close"}, 1},
		{"pull", func(f *nastoolstest.Fake) { f.FailCommand = "pull" }, nastools.Command{Name: "deploy"}, []string{"read:nastools.toml", "git", "compose", "open", "lock", "compose", "close"}, 1},
		{"up", func(f *nastoolstest.Fake) { f.FailCommand = "up" }, nastools.Command{Name: "deploy"}, []string{"read:nastools.toml", "git", "compose", "open", "lock", "compose", "compose", "close"}, 1},
		{"ps", func(f *nastoolstest.Fake) { f.FailCommand = "ps" }, nastools.Command{Name: "status"}, []string{"read:nastools.toml", "git", "open", "compose", "close"}, 1},
		{"logs interrupted", func(f *nastoolstest.Fake) { f.ComposeErr = context.Canceled }, nastools.Command{Name: "logs", Follow: true}, []string{"read:nastools.toml", "git", "open", "compose", "close"}, 1},
		{"negative wait", func(*nastoolstest.Fake) {}, nastools.Command{Name: "deploy", LockWait: -time.Second}, []string{"read:nastools.toml", "git"}, 0},
		{"negative tail", func(*nastoolstest.Fake) {}, nastools.Command{Name: "logs", Tail: -1}, []string{"read:nastools.toml", "git"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &nastoolstest.Fake{Files: map[string]string{"nastools.toml": strings.Split(configText, "[files]")[0]}}
			tc.change(f)
			err := nastools.Execute(t.Context(), f, "nastools.toml", environment(), &f.Out, &f.Out, tc.command)
			require.ErrorIs(t, err, nastools.ErrInvalid)
			assert.NotContains(t, err.Error(), nastoolstest.ErrPrivate.Error())
			assert.Equal(t, tc.want, f.Calls)
			assert.Equal(t, tc.closed, f.Closed)
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
		{"failure is not busy", time.Minute, []error{nastoolstest.ErrPrivate}, nil, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &nastoolstest.Fake{
				Files:      map[string]string{"nastools.toml": strings.Split(configText, "[files]")[0]},
				LockErrors: tc.errors,
				PauseErr:   tc.pauseErr,
			}
			err := nastools.Execute(
				t.Context(),
				f,
				"nastools.toml",
				environment(),
				&f.Out,
				&f.Out,
				nastools.Command{Name: "deploy", LockWait: tc.wait},
			)
			if tc.name == "retry" {
				require.NoError(t, err)
				assert.Len(t, f.Commands, 4)
			} else {
				require.ErrorIs(t, err, nastools.ErrInvalid)
				assert.Len(t, f.Commands, 1)
			}
			if tc.wantBusy {
				require.ErrorIs(t, err, nastools.ErrBusy)
			}
			assert.Len(t, f.LockPaths, tc.attempts)
			assert.Equal(t, 1, f.Closed)
		})
	}
}

func assertCommands(t *testing.T, f *nastoolstest.Fake, base []string, want [][]string, env map[string]string) {
	t.Helper()
	for i, command := range want {
		assert.Equal(t, append(slices.Clone(base), command...), f.Commands[i])
		for _, private := range environment() {
			assert.NotContains(t, strings.Join(f.Commands[i], " "), private)
		}
		if command[0] == "config" {
			assert.Contains(t, f.Environments[i], "DOCKER_HOST="+env["DOCKER_HOST"])
			for _, name := range []string{"DOCKER_CONTEXT", "DOCKER_TLS", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"} {
				assert.Contains(t, f.Environments[i], name+"="+env[name])
			}
			continue
		}
		assert.Contains(t, f.Environments[i], "DOCKER_HOST=unix:///example.invalid/socket")
		assert.Contains(t, f.Environments[i], "REQUIRED_INPUT=required.example.invalid")
		for _, entry := range f.Environments[i] {
			name, _, _ := strings.Cut(entry, "=")
			assert.NotContains(
				t,
				[]string{"DOCKER_CONTEXT", "DOCKER_TLS", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"},
				name,
			)
			if name == "DOCKER_HOST" {
				assert.Equal(t, "DOCKER_HOST=unix:///example.invalid/socket", entry)
			}
		}
	}
}

func TestLockWaitExpiresDuringAttempt(t *testing.T) {
	t.Parallel()
	f := &nastoolstest.Fake{
		Files:     map[string]string{"nastools.toml": strings.Split(configText, "[files]")[0]},
		LockBlock: true,
	}
	err := nastools.Execute(
		t.Context(), f, "nastools.toml", environment(), &f.Out, &f.Out,
		nastools.Command{Name: "deploy", LockWait: time.Millisecond},
	)
	require.ErrorIs(t, err, nastools.ErrBusy)
	require.ErrorIs(t, err, nastools.ErrInvalid)
	assert.Len(t, f.LockPaths, 1)
	assert.Equal(t, 1, f.Closed)
}

func TestLockParentCancelStaysGeneric(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(*nastoolstest.Fake, context.CancelFunc)
	}{
		{"during pause", func(f *nastoolstest.Fake, cancel context.CancelFunc) {
			f.LockErrors = []error{nastools.ErrBusy}
			f.OnPause = cancel
			f.PauseErr = context.Canceled
		}},
		{"during long lock wait", func(f *nastoolstest.Fake, cancel context.CancelFunc) {
			f.LockBlock = true
			time.AfterFunc(time.Millisecond, cancel)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			f := &nastoolstest.Fake{Files: map[string]string{"nastools.toml": strings.Split(configText, "[files]")[0]}}
			tc.setup(f, cancel)
			err := nastools.Execute(
				ctx, f, "nastools.toml", environment(), &f.Out, &f.Out,
				nastools.Command{Name: "deploy", LockWait: time.Hour},
			)
			require.ErrorIs(t, err, nastools.ErrInvalid)
			require.NotErrorIs(t, err, nastools.ErrBusy)
			assert.Equal(t, 1, f.Closed)
		})
	}
}
