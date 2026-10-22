// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package main

import (
	"bytes"
	"errors"
	"slices"
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

func exec(t *testing.T, args ...string) harness {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(t.Context(), args, streams{in: strings.NewReader(""), out: &out, err: &errOut}, nil, nil)
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
		nil,
		nil,
	)
	assert.Equal(t, exitError, code)
	assert.Contains(t, errOut.String(), "write version")
}

type unreadableConfigOperations struct {
	nastools.Operations

	path  string
	data  []byte
	files map[string]string
}

var errPrivateConfig = errors.New("private.example.invalid")

func (f *unreadableConfigOperations) ReadFile(path string) ([]byte, error) {
	f.path = path
	if data, ok := f.files[path]; ok {
		return []byte(data), nil
	}
	if f.data != nil {
		return f.data, nil
	}
	return nil, errPrivateConfig
}

func TestCheckUnreadableConfig(t *testing.T) {
	t.Parallel()

	var out, errOut bytes.Buffer
	f := &unreadableConfigOperations{}
	path := "private.example.invalid/config.toml"
	code := Run(t.Context(), []string{"check", "--config", path},
		streams{in: strings.NewReader(""), out: &out, err: &errOut}, f,
		map[string]string{"NASTOOLS_PRIVATE_STRINGS": "private.example.invalid"})
	assert.Equal(t, 2, code)
	assert.Equal(t, path, f.path)
	assert.Empty(t, out.String())
	assert.Contains(t, errOut.String(), "nastools.toml: unreadable input")
	assert.NotContains(t, errOut.String(), "private.example.invalid")
}

func TestRemoteCommandsMaskErrors(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"deploy"},
		{"status"},
		{"logs", "example", "--tail", "20", "--follow"},
		{"deploy", "--lock-wait", "2s"},
		{"deploy", "--lock-wait", "private.example.invalid"},
		{"logs", "--unknown=private.example.invalid"},
		{"status", "private.example.invalid"},
		{"deploy", "--config"},
		{"deploy", "--unknown", "--", "--config", "ignored"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			f := &unreadableConfigOperations{}
			code := Run(t.Context(), args, streams{out: &out, err: &out}, f, map[string]string{
				"GITHUB_ACTIONS": "true", "NASTOOLS_PRIVATE_STRINGS": "private.example.invalid",
			})
			assert.Equal(t, exitError, code)
			assert.Equal(
				t,
				"::add-mask::private.example.invalid\nnastools: nastools.toml: unreadable input: invalid or missing input\n",
				out.String(),
			)
		})
	}
}

func TestRemoteFlagAndArgumentErrors(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"deploy"},
		{"status"},
		{"logs", "example", "--tail", "20", "--follow"},
		{"deploy", "--lock-wait", "2s"},
		{"deploy", "--lock-wait", "private.example.invalid"},
		{"logs", "--unknown=private.example.invalid"},
		{"status", "private.example.invalid"},
		{"deploy", "--config"},
		{"deploy", "--unknown", "--", "--config", "ignored"},
		{"logs", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			f := &unreadableConfigOperations{data: []byte(`project = ""`)}
			code := Run(t.Context(), args, streams{out: &out, err: &out}, f, map[string]string{
				"GITHUB_ACTIONS": "true", "NASTOOLS_PRIVATE_STRINGS": "private.example.invalid",
			})
			if slices.Contains(args, "--help") {
				assert.Equal(t, exitOK, code)
			} else {
				assert.Equal(t, exitError, code)
			}
			assert.True(t, strings.HasPrefix(out.String(), "::add-mask::private.example.invalid\n"))
			assert.NotContains(
				t,
				strings.TrimPrefix(out.String(), "::add-mask::private.example.invalid\n"),
				"private.example.invalid",
			)
		})
	}
}

func TestRemoteConfigAfterFlagError(t *testing.T) {
	t.Parallel()
	for _, prefix := range [][]string{
		{"deploy", "--unknown=required.example.invalid"},
		{"deploy", "--lock-wait", "required.example.invalid"},
		{"logs", "--help"},
	} {
		for _, configFlag := range [][]string{
			{"--config", "example.invalid/custom.toml"},
			{"--config=example.invalid/custom.toml"},
		} {
			args := append(slices.Clone(prefix), configFlag...)
			t.Run(strings.Join(args, " "), func(t *testing.T) {
				t.Parallel()
				var out bytes.Buffer
				f := &unreadableConfigOperations{data: []byte(`required_env = ["REQUIRED"]
[ssh]
address_env = "ADDRESS"
user_env = "USER"
known_hosts_env = "KNOWN_HOSTS"
`)}
				code := Run(t.Context(), args, streams{out: &out, err: &out}, f, map[string]string{
					"GITHUB_ACTIONS": "true", "REQUIRED": "required.example.invalid",
					"ADDRESS": "nas.example.invalid:22", "USER": "user.example.invalid",
					"KNOWN_HOSTS": "key.example.invalid",
				})
				assert.Equal(t, "example.invalid/custom.toml", f.path)
				masks := "::add-mask::key.example.invalid\n::add-mask::nas.example.invalid:22\n" +
					"::add-mask::required.example.invalid\n::add-mask::user.example.invalid\n"
				assert.True(t, strings.HasPrefix(out.String(), masks))
				if slices.Contains(prefix, "--help") {
					assert.Equal(t, exitOK, code)
					assert.Contains(t, out.String(), "Usage:")
				} else {
					assert.Equal(t, exitError, code)
					assert.Equal(t, masks+"nastools: invalid or missing input\n", out.String())
				}
			})
		}
	}
}

func TestRemoteFlagErrorsMaskTemplateReferences(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"deploy", "--lock-wait", "invalid"},
		{"status", "extra"},
		{"logs", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			f := &unreadableConfigOperations{files: map[string]string{
				"nastools.toml": `required_env = ["REQUIRED"]
[ssh]
address_env = "ADDRESS"
user_env = "USER"
known_hosts_env = "KNOWN_HOSTS"
[expand_files]
EXPANDED = "template.txt"
`,
				"template.txt": "${TEMPLATE_ONLY}",
			}}
			Run(t.Context(), args, streams{out: &out, err: &out}, f, map[string]string{
				"GITHUB_ACTIONS": "true", "REQUIRED": "required.example.invalid",
				"ADDRESS": "nas.example.invalid:22", "USER": "user.example.invalid",
				"KNOWN_HOSTS": "key.example.invalid", "TEMPLATE_ONLY": "template.example.invalid",
			})
			masks := "::add-mask::key.example.invalid\n::add-mask::nas.example.invalid:22\n" +
				"::add-mask::required.example.invalid\n::add-mask::user.example.invalid\n" +
				"::add-mask::template.example.invalid\n"
			assert.True(t, strings.HasPrefix(out.String(), masks))
			assert.NotContains(t, strings.TrimPrefix(out.String(), masks), "template.example.invalid")
		})
	}
}
