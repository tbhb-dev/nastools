// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package nastools_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tbhb-dev/nastools/internal/nastools"
	"github.com/tbhb-dev/nastools/internal/nastools/nastoolstest"
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

func serveFake() *nastoolstest.Fake {
	return &nastoolstest.Fake{
		Files:        map[string]string{"nastools.toml": strings.Split(configText, "[files]")[0] + serveConfigText},
		RemoteStatus: serveStatusJSON,
	}
}

func TestServeAfterReadiness(t *testing.T) {
	t.Parallel()
	f := serveFake()
	require.NoError(
		t,
		nastools.Execute(
			t.Context(),
			f,
			"nastools.toml",
			environment(),
			&f.Out,
			&f.Out,
			nastools.Command{Name: "deploy"},
		),
	)
	assert.Equal(t, []string{
		"read:nastools.toml", "git", "compose", "open", "lock",
		"compose", "compose", "remote", "remote", "compose", "remote", "close",
	}, f.Calls)
	assert.Equal(t, [][]string{
		{"tailscale", "serve", "--service=svc:web", "--https=443", "http://127.0.0.1:3000"},
		{"tailscale", "serve", "--service=svc:data", "--tcp=4317", "tcp://127.0.0.1:4317"},
		{"tailscale", "serve", "status", "--json"},
	}, f.Remotes)
}

func TestServeFailureShortCircuits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		change      func(*nastoolstest.Fake)
		wantRemotes int
	}{
		{"up fails", func(f *nastoolstest.Fake) { f.FailCommand = "up" }, 0},
		{"first endpoint fails", func(f *nastoolstest.Fake) { f.RemoteFail = "--https=443" }, 1},
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
				&f.Out,
				&f.Out,
				nastools.Command{Name: "deploy"},
			)
			require.ErrorIs(t, err, nastools.ErrInvalid)
			assert.NotContains(t, err.Error(), nastoolstest.ErrPrivate.Error())
			assert.Len(t, f.Remotes, tc.wantRemotes)
			assert.Equal(t, 1, f.Closed)
			assert.NotContains(t, slices.Concat(f.Commands...), "ps")
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
			&f.Out,
			&f.Out,
			nastools.Command{Name: "status"},
		),
	)
	assert.Equal(t, [][]string{{"tailscale", "serve", "status", "--json"}}, f.Remotes)
	assert.Contains(t, f.Out.String(), `"svc:web"`)
	assert.Contains(t, f.Out.String(), `"svc:data"`)
	assert.NotContains(t, f.Out.String(), "svc:other")
	assert.NotContains(t, f.Out.String(), "HTTPS")
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
			f.RemoteStatus, f.RemoteFail = tc.status, tc.fail
			err := nastools.Execute(
				t.Context(),
				f,
				"nastools.toml",
				environment(),
				&f.Out,
				&f.Out,
				nastools.Command{Name: "status"},
			)
			require.ErrorIs(t, err, nastools.ErrInvalid)
			assert.NotContains(t, err.Error(), "private.example.invalid")
		})
	}
}

func TestNoServeWithoutEndpoints(t *testing.T) {
	t.Parallel()
	f := &nastoolstest.Fake{Files: map[string]string{"nastools.toml": strings.Split(configText, "[files]")[0]}}
	require.NoError(
		t,
		nastools.Execute(
			t.Context(),
			f,
			"nastools.toml",
			environment(),
			&f.Out,
			&f.Out,
			nastools.Command{Name: "deploy"},
		),
	)
	assert.Empty(t, f.Remotes)
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
			f.State = tc.state
			require.NoError(
				t,
				nastools.Execute(
					t.Context(),
					f,
					"nastools.toml",
					environment(),
					&f.Out,
					&f.Out,
					nastools.Command{Name: "deploy"},
				),
			)
			for _, env := range f.Environments {
				assert.Contains(t, env, tc.want)
			}
			pull := slices.IndexFunc(f.Commands, func(c []string) bool { return slices.Contains(c, "pull") })
			require.Positive(t, pull)
			assert.Contains(t, f.Environments[pull], tc.want)
		})
	}
}
