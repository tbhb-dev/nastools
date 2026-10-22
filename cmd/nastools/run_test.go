// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tbhb-dev/nastools/internal/buildmeta"
	"github.com/tbhb-dev/nastools/internal/nastools/nastoolstest"
)

type harness struct {
	code   int
	stdout string
	stderr string
}

func exec(t *testing.T, args ...string) harness {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(
		t.Context(),
		args,
		streams{in: strings.NewReader(""), out: &out, err: &errOut},
		&nastoolstest.Fake{},
		nil,
	)
	return harness{code: code, stdout: out.String(), stderr: errOut.String()}
}

var errBrokenPipe = errors.New("broken pipe")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errBrokenPipe }

func TestVersionCommand(t *testing.T) {
	t.Parallel()

	got := exec(t, "version")
	info := buildmeta.Get()
	want := "nastools " + info.Version + "\ncommit: " + info.Commit + "\ndate:   " + info.Date + "\n"
	assert.Equal(t, exitOK, got.code)
	assert.Equal(t, want, got.stdout)
	assert.Empty(t, got.stderr)
}

func TestVersionFlag(t *testing.T) {
	t.Parallel()

	got := exec(t, "--version")
	assert.Equal(t, exitOK, got.code)
	assert.Contains(t, got.stdout, buildmeta.Version)
}

func TestUnknownCommandFails(t *testing.T) {
	t.Parallel()

	got := exec(t, "nonsense")
	assert.Equal(t, exitError, got.code)
	assert.Empty(t, got.stdout)
	assert.Contains(t, got.stderr, "nastools: ")
}

func TestVersionRejectsArguments(t *testing.T) {
	t.Parallel()

	got := exec(t, "version", "extra")
	assert.Equal(t, exitError, got.code)
}

func TestVersionWriteFailure(t *testing.T) {
	t.Parallel()

	var errOut bytes.Buffer
	code := Run(
		t.Context(),
		[]string{"version"},
		streams{in: strings.NewReader(""), out: failingWriter{}, err: &errOut},
		&nastoolstest.Fake{},
		nil,
	)
	assert.Equal(t, exitError, code)
	assert.Contains(t, errOut.String(), "write version")
}

func TestCheckUnreadableConfig(t *testing.T) {
	t.Parallel()

	var out, errOut bytes.Buffer
	f := &nastoolstest.Fake{}
	path := "private.example.invalid/config.toml"
	code := Run(t.Context(), []string{"check", "--config", path},
		streams{in: strings.NewReader(""), out: &out, err: &errOut}, f,
		map[string]string{"NASTOOLS_PRIVATE_STRINGS": "private.example.invalid"})
	assert.Equal(t, exitError, code)
	assert.Equal(t, []string{"read:" + path}, f.Calls)
	assert.Empty(t, out.String())
	assert.Contains(t, errOut.String(), "nastools.toml: unreadable input")
	assert.NotContains(t, errOut.String(), "private.example.invalid")
}

// TestBrokenConfigHidesArguments checks that under Actions, when the config
// doesn't parse, no error echoes the arguments, whatever discovery found.
func TestBrokenConfigHidesArguments(t *testing.T) {
	t.Parallel()
	const argument = "argument.example.invalid"
	for _, args := range [][]string{
		{"version", argument},
		{argument},
		{"help", argument},
		{"version", "--" + argument},
		{"deploy", "--lock-wait", argument},
	} {
		for _, broken := range []bool{false, true} {
			t.Run(fmt.Sprint(args, broken), func(t *testing.T) {
				t.Parallel()
				config := "project = 'example'\n"
				if broken {
					config = "project = = 'example'\n"
				}
				var out, errOut bytes.Buffer
				code := Run(t.Context(), args, streams{in: strings.NewReader(""), out: &out, err: &errOut},
					&nastoolstest.Fake{Files: map[string]string{"nastools.toml": config}},
					map[string]string{"GITHUB_ACTIONS": "true"})
				assert.Equal(t, exitError, code)
				assert.Empty(t, out.String())
				if broken || args[0] == "deploy" {
					assert.Equal(t, "nastools: invalid or missing input\n", errOut.String())
				} else {
					assert.Contains(t, errOut.String(), argument)
				}
			})
		}
	}
}

// TestConfigArgument checks the config path against the flag parser of the
// command tree, for every argument list that parses.
func TestConfigArgument(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "nastools.toml"},
		{[]string{"deploy"}, "nastools.toml"},
		{[]string{"deploy", "--config", "a.toml"}, "a.toml"},
		{[]string{"deploy", "--config=a.toml"}, "a.toml"},
		{[]string{"deploy", "--config="}, ""},
		{[]string{"deploy", "--config", "a.toml", "--config=b.toml"}, "b.toml"},
		{[]string{"deploy", "--config", "--", "--config", "b.toml"}, "b.toml"},
		{[]string{"deploy", "--config", "--config=b.toml"}, "--config=b.toml"},
		{[]string{"deploy", "--config", "-h"}, "-h"},
		{[]string{"deploy", "--", "--config", "b.toml"}, "nastools.toml"},
		{[]string{"deploy", "--config=a.toml", "--", "--config=b.toml"}, "a.toml"},
		{[]string{"--config", "a.toml", "deploy"}, "a.toml"},
		{[]string{"logs", "web", "--follow", "--config", "a.toml"}, "a.toml"},
		{[]string{"deploy", "--lock-wait", "1s", "--config", "a.toml"}, "a.toml"},
		// The flag parser stops at these errors, and nothing reads a config.
		{[]string{"deploy", "--config"}, "nastools.toml"},
		{[]string{"deploy", "--unknown", "--config", "a.toml"}, "a.toml"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, configArgument(tc.args))
			root := newRootCmd(t.Context(), streams{in: strings.NewReader(""), out: io.Discard, err: io.Discard},
				&nastoolstest.Fake{}, nil)
			target, flags, err := root.Find(tc.args)
			if err != nil || target == root || target.ParseFlags(flags) != nil {
				return
			}
			got, err := target.Flags().GetString("config")
			require.NoError(t, err)
			assert.Equal(t, got, configArgument(tc.args))
		})
	}
}
