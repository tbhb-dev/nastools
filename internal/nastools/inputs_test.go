// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package nastools_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tbhb-dev/nastools/internal/nastools"
)

var errPrivate = errors.New("private.example.invalid")

type fakeOperations struct {
	files           map[string]string
	state           nastools.GitState
	gitErr          error
	composeErr      error
	calls           []string
	args            []string
	env             []string
	out             bytes.Buffer
	outputAtCompose string
	knownHosts      string
	openErr         error
	lockErrors      []error
	pauseErr        error
	failCommand     string
	commands        [][]string
	lockPaths       []string
	environments    [][]string
	remotes         [][]string
	remoteStatus    string
	remoteFail      string
	remoteOut       string
	remoteDiag      string
	lockBlock       bool
	onPause         func()
	closed          int
}

func (f *fakeOperations) ReadFile(path string) ([]byte, error) {
	f.calls = append(f.calls, "read:"+path)
	data, ok := f.files[path]
	if !ok {
		return nil, errPrivate
	}
	return []byte(data), nil
}

func (f *fakeOperations) GitState(context.Context, string) (nastools.GitState, error) {
	f.calls = append(f.calls, "git")
	return f.state, f.gitErr
}

func (f *fakeOperations) Compose(_ context.Context, args, env []string, out, errOut io.Writer) error {
	f.calls = append(f.calls, "compose")
	f.args, f.env = slices.Clone(args), slices.Clone(env)
	f.outputAtCompose = f.out.String()
	f.commands = append(f.commands, slices.Clone(args))
	f.environments = append(f.environments, slices.Clone(env))
	if out != nil {
		if _, err := io.WriteString(out, "compose output\n"); err != nil {
			return errPrivate
		}
		if _, err := io.WriteString(errOut, "compose diagnostic\n"); err != nil {
			return errPrivate
		}
	}
	if slices.Contains(args, f.failCommand) {
		return errPrivate
	}
	return f.composeErr
}

func config() nastools.Config {
	return nastools.Config{
		Project:     "example",
		Compose:     []string{"compose.yaml"},
		WaitSeconds: 180,
		RequiredEnv: []string{"REQUIRED_INPUT"},
		SSH: nastools.SSH{
			AddressEnv:    "SSH_ADDRESS",
			UserEnv:       "SSH_USER",
			KnownHostsEnv: "SSH_KNOWN_HOSTS",
			LockDir:       "/example.invalid/locks",
		},
	}
}

func environment() map[string]string {
	return map[string]string{
		"REQUIRED_INPUT": "required.example.invalid", "SSH_ADDRESS": "host.example.invalid:22",
		"SSH_USER": "user.example.invalid", "SSH_KNOWN_HOSTS": knownHostsText,
	}
}

const knownHostsText = "host.example.invalid ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAICZ4niA2YR6ErsZQRahQYkioleR/p9/qUOStEMhVy/K5\n" +
	"[host.example.invalid]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAICZ4niA2YR6ErsZQRahQYkioleR/p9/qUOStEMhVy/K5\n"

const configText = `project = "example"
compose = ["compose.yaml", "/config/override.yaml"]
required_env = ["REQUIRED_INPUT"]
[ssh]
address_env = "SSH_ADDRESS"
user_env = "SSH_USER"
known_hosts_env = "SSH_KNOWN_HOSTS"
lock_dir = "/example.invalid/locks"
[files]
STATIC_INPUT = "config/static.txt"
[expand_files]
EXPANDED_INPUT = "config/expanded.txt"
`

func TestCheckPolicy(t *testing.T) {
	t.Parallel()
	f := &fakeOperations{files: map[string]string{
		"deployment/nastools.toml":       configText,
		"deployment/config/static.txt":   "unchanged $REQUIRED_INPUT\r\n",
		"deployment/config/expanded.txt": "value=${REQUIRED_INPUT}\n",
	}, state: nastools.GitState{Commit: "abc123", Dirty: true}}
	env := environment()
	env["GITHUB_ACTIONS"] = "true"
	env["INHERITED_INPUT"] = "inherited.example.invalid"
	require.NoError(
		t,
		nastools.Execute(
			t.Context(),
			f,
			"deployment/nastools.toml",
			env,
			&f.out,
			nil,
			nastools.Command{Name: nastools.CheckName},
		),
	)
	assert.Equal(t, []string{
		"read:deployment/nastools.toml", "read:deployment/config/expanded.txt",
		"read:deployment/config/static.txt", "git", "compose",
	}, f.calls)
	assert.Equal(t, []string{
		"--project-directory", "deployment", "--project-name", "example",
		"--file", "deployment/compose.yaml", "--file", "/config/override.yaml", "config", "-q",
	}, f.args)
	assert.Contains(t, f.env, "STATIC_INPUT=unchanged $REQUIRED_INPUT\r\n")
	assert.Contains(t, f.env, "EXPANDED_INPUT=value=required.example.invalid\n")
	assert.Contains(t, f.env, "INHERITED_INPUT=inherited.example.invalid")
	assert.Equal(t, nastools.MaskCommands(nastools.PrivateValues(config(), env), true), f.outputAtCompose)
}

func TestCheckShortCircuit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		text       string
		gitErr     error
		composeErr error
		want       string
		calls      []string
	}{
		{"unreadable config", "", nil, nil, "nastools.toml", []string{"read:nastools.toml"}},
		{"unknown field", "unknown = 'private.example.invalid'\n" + configText, nil, nil, "nastools.toml", []string{"read:nastools.toml"}},
		{"missing file", configText, nil, nil, "files.STATIC_INPUT", []string{"read:nastools.toml", "read:config/expanded.txt", "read:config/static.txt"}},
		{"git failure", strings.Split(configText, "[files]")[0], errPrivate, nil, "git", []string{"read:nastools.toml", "git"}},
		{"compose failure", strings.Split(configText, "[files]")[0], nil, errPrivate, "compose config", []string{"read:nastools.toml", "git", "compose"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeOperations{files: map[string]string{}, gitErr: tc.gitErr, composeErr: tc.composeErr}
			if tc.text != "" {
				f.files["nastools.toml"] = tc.text
				f.files["config/expanded.txt"] = "$REQUIRED_INPUT"
			}
			env := environment()
			env["GITHUB_ACTIONS"] = "true"
			env["NASTOOLS_PRIVATE_STRINGS"] = "extra.example.invalid"
			err := nastools.Execute(
				t.Context(),
				f,
				"nastools.toml",
				env,
				&f.out,
				nil,
				nastools.Command{Name: nastools.CheckName},
			)
			require.ErrorIs(t, err, nastools.ErrInvalid)
			assert.Contains(t, err.Error(), tc.want)
			assert.NotContains(t, err.Error(), errPrivate.Error())
			assert.Equal(t, tc.calls, f.calls)
			c := config()
			if tc.text == "" {
				c = nastools.Config{}
			}
			assert.Equal(t, nastools.MaskCommands(nastools.PrivateValues(c, env), true), f.out.String())
		})
	}
}

func TestInputRequirements(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"REQUIRED_INPUT", "SSH_ADDRESS", "SSH_USER", "SSH_KNOWN_HOSTS"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := environment()
			delete(env, name)
			f := &fakeOperations{}
			_, err := nastools.Prepare(t.Context(), f, "nastools.toml", config(), env)
			require.ErrorIs(t, err, nastools.ErrInvalid)
			assert.Contains(t, err.Error(), name)
			assert.Empty(t, f.calls)
		})
	}
}

func TestFilesAndProvenance(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		static   string
		expanded string
		dirty    bool
		wantErr  string
	}{
		{"clean", "literal $MISSING\n", "${REQUIRED_INPUT}", false, ""},
		{"modified or untracked", "", "$REQUIRED_INPUT", true, ""},
		{"missing expansion input", "", "$MISSING", false, "expand_files.EXPANDED_INPUT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := config()
			c.Files = map[string]string{"STATIC_INPUT": "/config/static.txt"}
			c.ExpandFiles = map[string]string{"EXPANDED_INPUT": "expanded.txt"}
			f := &fakeOperations{
				files: map[string]string{"/config/static.txt": tc.static, "deployment/expanded.txt": tc.expanded},
				state: nastools.GitState{Commit: "abc123", Dirty: tc.dirty},
			}
			got, err := nastools.Prepare(t.Context(), f, "deployment/nastools.toml", c, environment())
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.NotContains(t, f.calls, "git")
				return
			}
			require.NoError(t, err)
			want := "abc123"
			if tc.dirty {
				want += "-dirty"
			}
			assert.Equal(t, want, got.Commit)
			assert.Contains(t, got.Env, "STATIC_INPUT="+tc.static)
			assert.Contains(t, got.Env, "EXPANDED_INPUT=required.example.invalid")
		})
	}
}

func TestPrivateValuesAndMasks(t *testing.T) {
	t.Parallel()
	env := environment()
	env["NASTOOLS_PRIVATE_STRINGS"] = " required.example.invalid,extra.example.invalid,xy, ,extra.example.invalid"
	env["SSH_AUTH_SOCK"] = "socket.example.invalid"
	assert.Equal(t, []string{
		"extra.example.invalid", knownHostsText, "host.example.invalid:22",
		"required.example.invalid", "user.example.invalid", "xy",
	}, nastools.PrivateValues(config(), env))
	for _, tc := range []struct {
		name    string
		actions bool
		want    string
	}{
		{"local", false, ""},
		{"actions", true, "::add-mask::value%25%0D%0A::warning::text\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, nastools.MaskCommands([]string{"value%\r\n::warning::text"}, tc.actions))
		})
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errPrivate }

func TestMaskFailureStopsCheck(t *testing.T) {
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
		nil,
		nastools.Command{Name: nastools.CheckName},
	)
	require.ErrorContains(t, err, "mask output")
	assert.NotContains(t, err.Error(), errPrivate.Error())
	assert.Equal(t, []string{"read:nastools.toml"}, f.calls)
}

func TestCheckWithoutActions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		env  map[string]string
	}{
		{"unset", environment()},
		{"empty", map[string]string{"GITHUB_ACTIONS": "", "NASTOOLS_PRIVATE_STRINGS": "extra.example.invalid"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeOperations{}
			err := nastools.Execute(
				t.Context(),
				f,
				"nastools.toml",
				tc.env,
				&f.out,
				nil,
				nastools.Command{Name: nastools.CheckName},
			)
			require.ErrorIs(t, err, nastools.ErrInvalid)
			assert.Empty(t, f.out.String())
		})
	}
}

func TestExpansionFailureOrder(t *testing.T) {
	t.Parallel()
	c := config()
	c.ExpandFiles = map[string]string{"A_INPUT": "a.txt", "B_INPUT": "b.txt"}
	f := &fakeOperations{files: map[string]string{"a.txt": "$MISSING"}}
	_, err := nastools.Prepare(t.Context(), f, "nastools.toml", c, environment())
	require.ErrorContains(t, err, "expand_files.A_INPUT")
	assert.Equal(t, []string{"read:a.txt", "read:b.txt"}, f.calls)
}

func TestMasksCoverEveryReadableTemplate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		files map[string]string
		a     string
	}{
		{"missing variable", map[string]string{"a.txt": "$MISSING", "b.txt": "${EXTRA_INPUT}"}, "a.txt"},
		{"unreadable file", map[string]string{"b.txt": "${EXTRA_INPUT}"}, "a.txt"},
		{"empty path", map[string]string{"b.txt": "${EXTRA_INPUT}"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := config()
			c.ExpandFiles = map[string]string{"A_INPUT": tc.a, "B_INPUT": "b.txt"}
			tc.files["nastools.toml"] = "project = \"example\""
			f := &fakeOperations{files: tc.files}
			env := environment()
			env["EXTRA_INPUT"] = "extra.example.invalid"
			env["GITHUB_ACTIONS"] = "true"
			var out bytes.Buffer
			require.NoError(t, nastools.MaskReferenced(f, "nastools.toml", c, env, &out))
			assert.Equal(t, "::add-mask::extra.example.invalid\n", out.String())

			_, err := nastools.Prepare(t.Context(), f, "nastools.toml", c, env)
			require.ErrorIs(t, err, nastools.ErrInvalid)
			assert.NotContains(t, err.Error(), "extra.example.invalid")
		})
	}
}

func TestDuplicateOutputKeepsTemplateMasks(t *testing.T) {
	t.Parallel()
	c := config()
	c.Files = map[string]string{"A_INPUT": "static.txt"}
	c.ExpandFiles = map[string]string{"A_INPUT": "a.txt"}
	f := &fakeOperations{files: map[string]string{"a.txt": "${EXTRA_INPUT}"}}
	env := environment()
	env["EXTRA_INPUT"] = "extra.example.invalid"
	inputs, err := nastools.Prepare(t.Context(), f, "nastools.toml", c, env)
	require.ErrorContains(t, err, "duplicate output")
	assert.Equal(t, []string{"extra.example.invalid"}, inputs.Referenced)
}

func TestMaskReferencedWriteFailure(t *testing.T) {
	t.Parallel()
	c := config()
	c.ExpandFiles = map[string]string{"A_INPUT": "a.txt"}
	f := &fakeOperations{files: map[string]string{"a.txt": "${EXTRA_INPUT}"}}
	env := environment()
	env["EXTRA_INPUT"] = "extra.example.invalid"
	env["GITHUB_ACTIONS"] = "true"
	err := nastools.MaskReferenced(f, "nastools.toml", c, env, failingWriter{})
	require.ErrorIs(t, err, nastools.ErrInvalid)
	assert.NotContains(t, err.Error(), "extra.example.invalid")
}

func (f *fakeOperations) Open(_ context.Context, config nastools.SSH, env map[string]string) (string, error) {
	f.calls = append(f.calls, "open")
	f.knownHosts = env[config.KnownHostsEnv]
	return "/example.invalid/socket", f.openErr
}

func (f *fakeOperations) Lock(ctx context.Context, path string) error {
	f.calls = append(f.calls, "lock")
	f.lockPaths = append(f.lockPaths, path)
	if f.lockBlock {
		<-ctx.Done()
		return errPrivate
	}
	if len(f.lockErrors) == 0 {
		return nil
	}
	err := f.lockErrors[0]
	f.lockErrors = f.lockErrors[1:]
	return err
}

func (f *fakeOperations) Pause(context.Context) error {
	f.calls = append(f.calls, "pause")
	if f.onPause != nil {
		f.onPause()
	}
	return f.pauseErr
}

func (f *fakeOperations) Close() { f.closed++; f.calls = append(f.calls, "close") }

func TestExpandedReferencesAreMasked(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		template string
		actions  bool
		wantErr  bool
		want     string
	}{
		{"referenced", "${EXTRA_INPUT}", true, false, "::add-mask::extra.example.invalid\n"},
		{"already masked", "${REQUIRED_INPUT}", true, false, ""},
		{"missing sibling", "${EXTRA_INPUT} ${MISSING}", true, true, "::add-mask::extra.example.invalid\n"},
		{"outside actions", "${EXTRA_INPUT}", false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeOperations{files: map[string]string{
				"nastools.toml":       configText,
				"config/static.txt":   "static",
				"config/expanded.txt": tc.template,
			}}
			env := environment()
			env["EXTRA_INPUT"] = "extra.example.invalid"
			if tc.actions {
				env["GITHUB_ACTIONS"] = "true"
			}
			err := nastools.Execute(
				t.Context(), f, "nastools.toml", env, &f.out, &f.out, nastools.Command{Name: nastools.CheckName},
			)
			masks := nastools.MaskCommands(nastools.PrivateValues(config(), env), tc.actions) + tc.want
			if tc.wantErr {
				require.ErrorIs(t, err, nastools.ErrInvalid)
				assert.NotContains(t, err.Error(), "extra.example.invalid")
				assert.Equal(t, masks, f.out.String())
				return
			}
			require.NoError(t, err)
			assert.Equal(t, masks, f.outputAtCompose)
		})
	}
}

func TestMasksSurviveEarlyValidationFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		config  string
		without string
	}{
		{
			"invalid service ID",
			configText + "[[serve]]\nservice = \"web\"\nhttps = 443\ntarget = \"http://127.0.0.1:3000\"\n",
			"",
		},
		{"missing required input", configText, "REQUIRED_INPUT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeOperations{files: map[string]string{
				"nastools.toml":       tc.config,
				"config/static.txt":   "static",
				"config/expanded.txt": "${EXTRA_INPUT}",
			}}
			env := environment()
			env["EXTRA_INPUT"] = "extra.example.invalid"
			env["GITHUB_ACTIONS"] = "true"
			delete(env, tc.without)
			err := nastools.Execute(
				t.Context(), f, "nastools.toml", env, &f.out, &f.out, nastools.Command{Name: nastools.CheckName},
			)
			require.ErrorIs(t, err, nastools.ErrInvalid)
			assert.NotContains(t, err.Error(), "extra.example.invalid")
			masks := nastools.MaskCommands(nastools.PrivateValues(config(), env), true)
			assert.Equal(t, masks+"::add-mask::extra.example.invalid\n", f.out.String())
		})
	}
}
