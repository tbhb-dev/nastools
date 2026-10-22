// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

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
