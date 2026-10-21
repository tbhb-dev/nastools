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
	"github.com/tbhb-dev/nastools/internal/nastools"
)

type harness struct {
	code   int
	stdout string
	stderr string
}

func exec(args ...string) harness {
	var out, errOut bytes.Buffer
	code := Run(args, streams{in: strings.NewReader(""), out: &out, err: &errOut}, nil, nil)
	return harness{code: code, stdout: out.String(), stderr: errOut.String()}
}

var errBrokenPipe = errors.New("broken pipe")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errBrokenPipe }

func TestVersionCommand(t *testing.T) {
	t.Parallel()

	got := exec("version")
	info := buildmeta.Get()
	want := "nastools " + info.Version + "\ncommit: " + info.Commit + "\ndate:   " + info.Date + "\n"
	assert.Equal(t, exitOK, got.code)
	assert.Equal(t, want, got.stdout)
	assert.Empty(t, got.stderr)
}

func TestVersionFlag(t *testing.T) {
	t.Parallel()

	got := exec("--version")
	assert.Equal(t, exitOK, got.code)
	assert.Contains(t, got.stdout, buildmeta.Version)
}

func TestUnknownCommandFails(t *testing.T) {
	t.Parallel()

	got := exec("nonsense")
	assert.Equal(t, exitError, got.code)
	assert.Empty(t, got.stdout)
	assert.Contains(t, got.stderr, "nastools: ")
}

func TestVersionRejectsArguments(t *testing.T) {
	t.Parallel()

	got := exec("version", "extra")
	assert.Equal(t, exitError, got.code)
}

func TestVersionWriteFailure(t *testing.T) {
	t.Parallel()

	var errOut bytes.Buffer
	code := Run([]string{"version"}, streams{in: strings.NewReader(""), out: failingWriter{}, err: &errOut}, nil, nil)
	assert.Equal(t, exitError, code)
	assert.Contains(t, errOut.String(), "write version")
}

type unreadableConfigOperations struct {
	nastools.Operations

	path string
}

var errPrivateConfig = errors.New("private.example.invalid")

func (f *unreadableConfigOperations) ReadFile(path string) ([]byte, error) {
	f.path = path
	return nil, errPrivateConfig
}

func TestCheckUnreadableConfig(t *testing.T) {
	t.Parallel()

	var out, errOut bytes.Buffer
	f := &unreadableConfigOperations{}
	path := "private.example.invalid/config.toml"
	code := Run([]string{"check", "--config", path},
		streams{in: strings.NewReader(""), out: &out, err: &errOut}, f,
		map[string]string{"NASTOOLS_PRIVATE_STRINGS": "private.example.invalid"})
	assert.Equal(t, 2, code)
	assert.Equal(t, path, f.path)
	assert.Empty(t, out.String())
	assert.Contains(t, errOut.String(), "nastools.toml: unreadable input")
	assert.NotContains(t, errOut.String(), "private.example.invalid")
}
