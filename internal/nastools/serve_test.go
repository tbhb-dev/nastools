// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package nastools_test

import (
	"bytes"
	"context"
	"io"
	"slices"
	"strconv"
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
func (f *fakeOperations) Remote(_ context.Context, argv []string, out, errOut io.Writer) error {
	f.calls = append(f.calls, "remote")
	f.remotes = append(f.remotes, slices.Clone(argv))
	output := f.remoteOut
	if slices.Contains(argv, "status") {
		output = f.remoteStatus
	}
	if _, err := io.WriteString(out, output); err != nil {
		return errPrivate
	}
	if _, err := io.WriteString(errOut, f.remoteDiag); err != nil {
		return errPrivate
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
		{"pull fails", func(f *fakeOperations) { f.failCommand = "pull" }, 0},
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
		{"empty", "", ""},
		{"array", `["private.example.invalid"]`, ""},
		{"services array", `{"Services":["private.example.invalid"]}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := serveFake()
			f.remoteStatus, f.remoteFail, f.remoteDiag = tc.status, tc.fail, "diagnostic.example.invalid"
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
			assert.Equal(t, "compose output\ncompose diagnostic\n", f.out.String())
		})
	}
}

func TestServeStatusShapes(t *testing.T) {
	t.Parallel()
	for _, status := range []string{`null`, `{}`, `{"Services":null}`, `{"Services":{}}`, `{"Services":{"svc:other":{}}}`} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			f := serveFake()
			f.remoteStatus = status
			require.NoError(
				t,
				nastools.Execute(
					t.Context(), f, "nastools.toml", environment(), &f.out, &f.out, nastools.Command{Name: "status"},
				),
			)
			assert.Equal(t, "compose output\ncompose diagnostic\n{}\n", f.out.String())
		})
	}
}

func TestServeErrorsNameTheEntry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ fail, want, other string }{
		{"--https=443", "serve[0]", "serve[1]"},
		{"--tcp=4317", "serve[1]", "serve[0]"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			f := serveFake()
			f.remoteFail = tc.fail
			err := nastools.Execute(
				t.Context(), f, "nastools.toml", environment(), &f.out, &f.out, nastools.Command{Name: "deploy"},
			)
			require.ErrorIs(t, err, nastools.ErrInvalid)
			assert.Contains(t, err.Error(), tc.want)
			assert.NotContains(t, err.Error(), tc.other)
			assert.NotContains(t, err.Error(), errPrivate.Error())
			assert.NotContains(t, err.Error(), "svc:")
		})
	}
}

func TestServeOutputMasksServeNames(t *testing.T) {
	t.Parallel()
	const (
		web   = "web.example.ts.net"
		other = "other.example.ts.net"
		diag  = "diag.example.ts.net"
	)
	for _, actions := range []bool{true, false} {
		t.Run(strconv.FormatBool(actions), func(t *testing.T) {
			t.Parallel()
			f := serveFake()
			f.remoteOut = "Available at https://" + web + "/\n"
			f.remoteDiag = "warning " + diag + "\n"
			f.remoteStatus = strings.Replace(
				strings.Replace(serveStatusJSON, "web.example.invalid", web, 1), `"svc:other":{"TCP":{"9":{}}}`,
				`"svc:other":{"Web":{"`+other+`:443":{}}}`, 1,
			)
			env := environment()
			if actions {
				env["GITHUB_ACTIONS"] = "true"
			}
			require.NoError(
				t,
				nastools.Execute(
					t.Context(),
					f,
					"nastools.toml",
					env,
					&f.out,
					&f.out,
					nastools.Command{Name: "deploy"},
				),
			)
			got := f.out.String()
			assert.Equal(t, actions, strings.Contains(got, "::add-mask::"+web+"\n"))
			assert.Equal(t, actions, strings.Contains(got, "::add-mask::"+diag+"\n"))
			assert.NotContains(t, got, other, "unscoped Service IDs aren't printed or masked")
			if actions {
				// The first appearance of each name is its mask command.
				for _, name := range []string{web, diag} {
					assert.Equal(
						t,
						strings.Index(got, "::add-mask::"+name)+len("::add-mask::"),
						strings.Index(got, name),
					)
				}
			}
			assert.Contains(t, got, "Available at https://"+web+"/\n")
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

// maskFailWriter rejects the mask command for a *.ts.net hostname.
type maskFailWriter struct{ written []string }

func (w *maskFailWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(".ts.net")) {
		return 0, errPrivate
	}
	w.written = append(w.written, string(p))
	return len(p), nil
}

func TestServeMaskFailureWritesNothing(t *testing.T) {
	t.Parallel()
	f := serveFake()
	f.remoteDiag = "serving at node.example.ts.net\n"
	env := environment()
	env["GITHUB_ACTIONS"] = "true"
	var out maskFailWriter
	var errOut bytes.Buffer
	err := nastools.Execute(t.Context(), f, "nastools.toml", env, &out, &errOut, nastools.Command{Name: "deploy"})
	require.ErrorIs(t, err, nastools.ErrInvalid)
	assert.NotContains(t, err.Error(), "ts.net")
	assert.NotContains(t, errOut.String(), "ts.net")
	assert.NotContains(t, strings.Join(out.written, ""), "ts.net")
	assert.Len(t, f.remotes, 1)
}
