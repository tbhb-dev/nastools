// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package nastools_test

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tbhb-dev/nastools/internal/nastools"
)

const serveConfigText = `
[[serve]]
service = "svc:web"
https = 443
target = "http://127.0.0.1:3000"
[[serve]]
service = "svc:data"
tcp = 4317
target = "tcp://127.0.0.1:4317"
`

const serveStatusJSON = `{"TCP":{"443":{"HTTPS":true}},"Services":{` +
	`"svc:web":{"Web":{"web.example.invalid:443":{}}},` +
	`"svc:data":{"TCP":{"4317":{}}},` +
	`"svc:other":{"TCP":{"9":{}}}}}`

// Remote records one remote command, writing the canned Serve status for a status query.
func (f *fakeOperations) Remote(_ context.Context, argv []string, out, _ io.Writer) error {
	f.calls = append(f.calls, "remote")
	f.remotes = append(f.remotes, slices.Clone(argv))
	if slices.Contains(argv, "status") {
		if _, err := io.WriteString(out, f.remoteStatus); err != nil {
			return errPrivate
		}
	}
	if f.remoteFail != "" && slices.Contains(argv, f.remoteFail) {
		return errPrivate
	}
	return nil
}

func serveFake() *fakeOperations {
	return &fakeOperations{
		files:        map[string]string{"nastools.toml": strings.Split(configText, "[files]")[0] + serveConfigText},
		remoteStatus: serveStatusJSON,
	}
}

func TestServeAfterReadiness(t *testing.T) {
	t.Parallel()
	f := serveFake()
	env := environment()
	env["GITHUB_ACTIONS"] = "true"
	require.NoError(
		t,
		nastools.Execute(t.Context(), f, "nastools.toml", env, &f.out, &f.out, nastools.Command{Name: "deploy"}),
	)
	assert.Equal(t, []string{
		"read:nastools.toml", "git", "compose", "open", "lock",
		"compose", "compose", "remote", "remote", "compose", "remote", "close",
	}, f.calls)
	assert.Equal(t, [][]string{
		{"tailscale", "serve", "--service=svc:web", "--https=443", "http://127.0.0.1:3000"},
		{"tailscale", "serve", "--service=svc:data", "--tcp=4317", "tcp://127.0.0.1:4317"},
		{"tailscale", "serve", "status", "--json"},
	}, f.remotes)
	assert.True(
		t,
		strings.HasPrefix(f.out.String(), nastools.MaskCommands(nastools.PrivateValues(config(), env), true)),
	)
}

func TestServeFailureShortCircuits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		change      func(*fakeOperations)
		wantRemotes int
	}{
		{"up fails", func(f *fakeOperations) { f.failCommand = "up" }, 0},
		{"first endpoint fails", func(f *fakeOperations) { f.remoteFail = "--https=443" }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := serveFake()
			tc.change(f)
			err := nastools.Execute(
				t.Context(),
				f,
				"nastools.toml",
				environment(),
				&f.out,
				&f.out,
				nastools.Command{Name: "deploy"},
			)
			require.ErrorIs(t, err, nastools.ErrInvalid)
			assert.NotContains(t, err.Error(), errPrivate.Error())
			assert.Len(t, f.remotes, tc.wantRemotes)
			assert.Equal(t, 1, f.closed)
			assert.NotContains(t, slices.Concat(f.commands...), "ps")
		})
	}
}

func TestStatusScopesServe(t *testing.T) {
	t.Parallel()
	f := serveFake()
	require.NoError(
		t,
		nastools.Execute(
			t.Context(),
			f,
			"nastools.toml",
			environment(),
			&f.out,
			&f.out,
			nastools.Command{Name: "status"},
		),
	)
	assert.Equal(t, [][]string{{"tailscale", "serve", "status", "--json"}}, f.remotes)
	assert.Contains(t, f.out.String(), `"svc:web"`)
	assert.Contains(t, f.out.String(), `"svc:data"`)
	assert.NotContains(t, f.out.String(), "svc:other")
	assert.NotContains(t, f.out.String(), "HTTPS")
}

func TestServeStatusFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, status, fail string }{
		{"command", serveStatusJSON, "status"},
		{"malformed", "not json private.example.invalid", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := serveFake()
			f.remoteStatus, f.remoteFail = tc.status, tc.fail
			err := nastools.Execute(
				t.Context(),
				f,
				"nastools.toml",
				environment(),
				&f.out,
				&f.out,
				nastools.Command{Name: "status"},
			)
			require.ErrorIs(t, err, nastools.ErrInvalid)
			assert.NotContains(t, err.Error(), "private.example.invalid")
		})
	}
}

func TestNoServeWithoutEndpoints(t *testing.T) {
	t.Parallel()
	f := &fakeOperations{files: map[string]string{"nastools.toml": strings.Split(configText, "[files]")[0]}}
	require.NoError(
		t,
		nastools.Execute(
			t.Context(),
			f,
			"nastools.toml",
			environment(),
			&f.out,
			&f.out,
			nastools.Command{Name: "deploy"},
		),
	)
	assert.Empty(t, f.remotes)
}

func TestCommitLabelReachesCompose(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		state nastools.GitState
		want  string
	}{
		{"clean", nastools.GitState{Commit: "abc123"}, "NASTOOLS_COMMIT=abc123"},
		{"dirty", nastools.GitState{Commit: "abc123", Dirty: true}, "NASTOOLS_COMMIT=abc123-dirty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := serveFake()
			f.state = tc.state
			require.NoError(
				t,
				nastools.Execute(
					t.Context(),
					f,
					"nastools.toml",
					environment(),
					&f.out,
					&f.out,
					nastools.Command{Name: "deploy"},
				),
			)
			for _, env := range f.environments {
				assert.Contains(t, env, tc.want)
			}
			pull := slices.IndexFunc(f.commands, func(c []string) bool { return slices.Contains(c, "pull") })
			require.Positive(t, pull)
			assert.Contains(t, f.environments[pull], tc.want)
		})
	}
}
