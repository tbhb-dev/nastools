// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package nastools_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tbhb-dev/nastools/internal/nastools"
	"github.com/tbhb-dev/nastools/internal/nastools/nastoolstest"
)

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
			LockDir:       new("/example.invalid/locks"),
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
	f := &nastoolstest.Fake{Files: map[string]string{
		"deployment/nastools.toml":       configText,
		"deployment/config/static.txt":   "unchanged $REQUIRED_INPUT\r\n",
		"deployment/config/expanded.txt": "value=${REQUIRED_INPUT}\n",
	}, State: nastools.GitState{Commit: "abc123", Dirty: true}}
	env := environment()
	env["INHERITED_INPUT"] = "inherited.example.invalid"
	require.NoError(
		t,
		nastools.Execute(
			t.Context(),
			f,
			"deployment/nastools.toml",
			env,
			&f.Out,
			nil,
			nastools.Command{Name: nastools.CheckName},
		),
	)
	assert.Equal(t, []string{
		"read:deployment/nastools.toml", "read:deployment/config/expanded.txt",
		"read:deployment/config/static.txt", "git", "compose",
	}, f.Calls)
	assert.Equal(t, []string{
		"--project-directory", "deployment", "--project-name", "example",
		"--file", "deployment/compose.yaml", "--file", "/config/override.yaml", "config", "-q",
	}, f.Args)
	assert.Contains(t, f.Env, "STATIC_INPUT=unchanged $REQUIRED_INPUT\r\n")
	assert.Contains(t, f.Env, "EXPANDED_INPUT=value=required.example.invalid\n")
	assert.Contains(t, f.Env, "INHERITED_INPUT=inherited.example.invalid")
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
		{"git failure", strings.Split(configText, "[files]")[0], nastoolstest.ErrPrivate, nil, "git", []string{"read:nastools.toml", "git"}},
		{"compose failure", strings.Split(configText, "[files]")[0], nil, nastoolstest.ErrPrivate, "compose config", []string{"read:nastools.toml", "git", "compose"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &nastoolstest.Fake{Files: map[string]string{}, GitErr: tc.gitErr, ComposeErr: tc.composeErr}
			if tc.text != "" {
				f.Files["nastools.toml"] = tc.text
				f.Files["config/expanded.txt"] = "$REQUIRED_INPUT"
			}
			env := environment()
			err := nastools.Execute(
				t.Context(),
				f,
				"nastools.toml",
				env,
				&f.Out,
				nil,
				nastools.Command{Name: nastools.CheckName},
			)
			require.ErrorIs(t, err, nastools.ErrInvalid)
			assert.Contains(t, err.Error(), tc.want)
			assert.NotContains(t, err.Error(), nastoolstest.ErrPrivate.Error())
			assert.Equal(t, tc.calls, f.Calls)
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
			f := &nastoolstest.Fake{}
			_, err := nastools.Prepare(t.Context(), f, "nastools.toml", config(), env)
			require.ErrorIs(t, err, nastools.ErrInvalid)
			assert.Contains(t, err.Error(), name)
			assert.Empty(t, f.Calls)
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
			f := &nastoolstest.Fake{
				Files: map[string]string{"/config/static.txt": tc.static, "deployment/expanded.txt": tc.expanded},
				State: nastools.GitState{Commit: "abc123", Dirty: tc.dirty},
			}
			got, err := nastools.Prepare(t.Context(), f, "deployment/nastools.toml", c, environment())
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.NotContains(t, f.Calls, "git")
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

func TestExpansionFailureOrder(t *testing.T) {
	t.Parallel()
	c := config()
	c.ExpandFiles = map[string]string{"A_INPUT": "a.txt", "B_INPUT": "b.txt"}
	f := &nastoolstest.Fake{Files: map[string]string{"a.txt": "$MISSING"}}
	_, err := nastools.Prepare(t.Context(), f, "nastools.toml", c, environment())
	require.ErrorContains(t, err, "expand_files.A_INPUT")
	assert.Equal(t, []string{"read:a.txt"}, f.Calls)
}
