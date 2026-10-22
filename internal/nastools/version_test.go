// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package nastools_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tbhb-dev/nastools/internal/nastools"
	"github.com/tbhb-dev/nastools/internal/nastools/nastoolstest"
)

func probeFake() *nastoolstest.Fake {
	return &nastoolstest.Fake{Files: map[string]string{"nastools.toml": strings.Split(configText, "[files]")[0]}}
}

func remoteCommands() []nastools.Command {
	return []nastools.Command{
		{Name: nastools.CheckName}, {Name: nastools.Deploy}, {Name: nastools.Status}, {Name: nastools.Logs},
	}
}

// TestComposeProbeRunsFirst checks that a failed probe stops each command
// before any other Compose call and before the transport opens.
func TestComposeProbeRunsFirst(t *testing.T) {
	t.Parallel()
	for _, command := range remoteCommands() {
		t.Run(command.Name, func(t *testing.T) {
			t.Parallel()
			f := probeFake()
			f.VersionErr = nastoolstest.ErrPrivate
			err := nastools.Execute(t.Context(), f, "nastools.toml", environment(), &f.Out, &f.Out, command)
			require.ErrorIs(t, err, nastools.ErrInvalid)
			assert.Contains(t, err.Error(), "compose version probe")
			assert.NotContains(t, err.Error(), nastoolstest.ErrPrivate.Error())
			assert.NotNil(t, f.VersionEnv)
			assert.Empty(t, f.Commands)
			assert.Equal(t, []string{"read:nastools.toml", "git"}, f.Calls)
			assert.Zero(t, f.Closed)
		})
	}
}

// TestComposeProbeFollowsInputChecks checks that input failures still stop
// a command before the probe runs.
func TestComposeProbeFollowsInputChecks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		change  func(*nastoolstest.Fake)
		command nastools.Command
	}{
		{"git", func(f *nastoolstest.Fake) { f.GitErr = nastoolstest.ErrPrivate }, nastools.Command{Name: nastools.Deploy}},
		{"negative wait", func(*nastoolstest.Fake) {}, nastools.Command{Name: nastools.Deploy, LockWait: -time.Second}},
		{"negative tail", func(*nastoolstest.Fake) {}, nastools.Command{Name: nastools.Logs, Tail: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := probeFake()
			tc.change(f)
			err := nastools.Execute(t.Context(), f, "nastools.toml", environment(), &f.Out, &f.Out, tc.command)
			require.ErrorIs(t, err, nastools.ErrInvalid)
			assert.Nil(t, f.VersionEnv)
		})
	}
}

// TestComposeProbeEnvironment checks that the probe gets the prepared
// environment without the variables that select an engine.
func TestComposeProbeEnvironment(t *testing.T) {
	t.Parallel()
	engine := []string{"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_TLS", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"}
	env := environment()
	env["INHERITED_INPUT"] = "inherited.example.invalid"
	for _, name := range engine {
		env[name] = "engine.example.invalid"
	}
	inputs, err := nastools.Prepare(t.Context(), probeFake(), "nastools.toml", config(), env)
	require.NoError(t, err)
	want := slices.DeleteFunc(slices.Clone(inputs.Env), func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		return slices.Contains(engine, name)
	})
	require.Contains(t, want, "INHERITED_INPUT=inherited.example.invalid")
	require.Len(t, want, len(inputs.Env)-len(engine))
	for _, command := range remoteCommands() {
		t.Run(command.Name, func(t *testing.T) {
			t.Parallel()
			f := probeFake()
			require.NoError(t, nastools.Execute(t.Context(), f, "nastools.toml", env, &f.Out, &f.Out, command))
			assert.Equal(t, want, f.VersionEnv)
		})
	}
}

func TestComposeVersionPolicy(t *testing.T) {
	t.Parallel()
	long := "Docker Compose version v2.40.3 built for an example platform\nsecond line"
	for _, tc := range []struct {
		name   string
		output string
		shown  string // What a refusal shows. Empty means accepted.
	}{
		{"minimum", "2.23.1\n", ""},
		{"above", "2.40.3\n", ""},
		{"major jump", "5.0.0\n", ""},
		{"leading v", "v2.40.3\n", ""},
		{"desktop build", "2.29.7-desktop.1\n", ""},
		{"distribution build", "2.24.6+ds1-0ubuntu1\n", ""},
		{"surrounding space", " 2.23.1 \r\nsecond line\n", ""},
		{"below patch", "2.23.0\n", `"2.23.0"`},
		{"below minor", "2.9.9\n", `"2.9.9"`},
		{"below major", "1.29.2\n", `"1.29.2"`},
		{"below with suffix", "v2.22.9-desktop.1\n", `"v2.22.9-desktop.1"`},
		{"empty", "", `""`},
		{"two parts", "2.23\n", `"2.23"`},
		{"too large", "99999999999999999999.0.0\n", `"99999999999999999999.0.0"`},
		{"multi-line unparsable", long, strconv.Quote(long[:40])},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := probeFake()
			f.ComposeVersion = []byte(tc.output)
			err := nastools.Execute(t.Context(), f, "nastools.toml", environment(), &f.Out, &f.Out,
				nastools.Command{Name: nastools.CheckName})
			if tc.shown == "" {
				require.NoError(t, err)
				assert.Len(t, f.Commands, 1)
				return
			}
			require.ErrorIs(t, err, nastools.ErrInvalid)
			assert.Equal(t, "compose version: read "+tc.shown+", need 2.23.1 or newer: invalid or missing input",
				err.Error())
			assert.Empty(t, f.Commands)
		})
	}
}

// TestMinComposeMatchesReadme keeps the constant equal to the release that
// the README states.
func TestMinComposeMatchesReadme(t *testing.T) {
	t.Parallel()
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	require.NoError(t, err)
	stated := regexp.MustCompile(`these routes from Compose (\d+\.\d+\.\d+) onward, and nastools refuses an older release`).
		FindSubmatch(readme)
	require.NotNil(t, stated)
	assert.Equal(t, nastools.MinCompose, string(stated[1]))
}
