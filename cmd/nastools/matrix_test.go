// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tbhb-dev/nastools/internal/nastools"
	"github.com/tbhb-dev/nastools/internal/nastools/nastoolstest"
)

const (
	maskPrefix   = "::add-mask::"
	tailnetName  = "node.example.ts.net"
	matrixConfig = `project = "example"
compose = ["compose.yaml"]
required_env = ["REQUIRED_INPUT"]
[ssh]
address_env = "SSH_ADDRESS"
user_env = "SSH_USER"
known_hosts_env = "SSH_KNOWN_HOSTS"
lock_dir = "/example.invalid/locks"
[files]
STATIC_INPUT = "static.txt"
[expand_files]
EXPANDED_INPUT = "expanded.txt"
[[serve]]
service = "svc:web"
https = 443
target = "http://127.0.0.1:3000"
`
)

// canaries lists every private value of the fixture, in the forms that
// nastools output could contain.
func canaries() []string {
	return []string{
		"secret-1.example.invalid", "secret-2.example.invalid", "secret-3.example.invalid",
		"secret-4.example.invalid", "secret-5.example.invalid", "secret-6.example.invalid",
		"secret-7.example.invalid", tailnetName,
	}
}

func matrixEnv() map[string]string {
	return map[string]string{
		"GITHUB_ACTIONS":           "true",
		"NASTOOLS_PRIVATE_STRINGS": "secret-6.example.invalid",
		"REQUIRED_INPUT":           "secret-1.example.invalid",
		"SSH_ADDRESS":              "secret-2.example.invalid:22",
		"SSH_USER":                 "secret-3.example.invalid",
		"SSH_KNOWN_HOSTS": "secret-4.example.invalid ssh-ed25519 " +
			"AAAAC3NzaC1lZDI1NTE5AAAAICZ4niA2YR6ErsZQRahQYkioleR/p9/qUOStEMhVy/K5\n",
		"TEMPLATE_INPUT": "secret-5.example.invalid",
	}
}

func matrixFake() *nastoolstest.Fake {
	return &nastoolstest.Fake{
		Files: map[string]string{
			"nastools.toml": matrixConfig,
			"expanded.txt":  "value=${TEMPLATE_INPUT}\n",
			"static.txt":    "secret-7.example.invalid\n",
		},
		RemoteStatus: `{"Services":{"svc:web":{"Web":{"` + tailnetName + `:443":{}}}}}`,
		Canaries:     canaries(),
	}
}

// fixture changes the config or injects a fault. unmasked lists the values
// whose only source the change removes. broken marks a config that fails
// the strict parse.
type fixture struct {
	name     string
	change   func(*nastoolstest.Fake)
	unmasked []string
	broken   bool
}

func fixtures() []fixture {
	config := func(change func(string) string) func(*nastoolstest.Fake) {
		return func(f *nastoolstest.Fake) { f.Files["nastools.toml"] = change(f.Files["nastools.toml"]) }
	}
	replace := func(old, replacement string) func(*nastoolstest.Fake) {
		return config(func(text string) string { return strings.Replace(text, old, replacement, 1) })
	}
	// Without its table, the template comes from a declaration on one line.
	expandOnly := func(declaration string) func(*nastoolstest.Fake) {
		return config(func(text string) string {
			text = strings.Replace(text, "[expand_files]\nEXPANDED_INPUT = \"expanded.txt\"\n", "", 1)
			return "project = = 'x'\n" + declaration + "\n" + text
		})
	}
	return []fixture{
		{"valid", func(*nastoolstest.Fake) {}, nil, false},
		{"syntax error", config(func(text string) string { return "project = = 'x'\n" + text }), nil, true},
		{"syntax error, dotted", expandOnly(`expand_files.EXPANDED_INPUT = "expanded.txt"`), nil, true},
		{"syntax error, inline", expandOnly(`expand_files = { EXPANDED_INPUT = 'expanded.txt' }`), nil, true},
		{"unknown field first", config(func(text string) string { return "unknown = 'x'\n" + text }), nil, true},
		{"upper-case keys", config(strings.NewReplacer(
			"required_env", "REQUIRED_ENV", "[ssh]", "[SSH]", "address_env", "ADDRESS_ENV",
			"[files]", "[FILES]", "[expand_files]", "[EXPAND_FILES]",
		).Replace), nil, false},
		{"invalid serve", replace("https = 443", "https = 0"), nil, false},
		{"duplicate output", replace("EXPANDED_INPUT =", "STATIC_INPUT ="), nil, false},
		{"missing env", replace(`["REQUIRED_INPUT"]`, `["REQUIRED_INPUT", "ABSENT_INPUT"]`), nil, false},
		{"template read", func(f *nastoolstest.Fake) { delete(f.Files, "expanded.txt") }, []string{
			"secret-5.example.invalid",
		}, false},
		{"static read", func(f *nastoolstest.Fake) { delete(f.Files, "static.txt") }, []string{
			"secret-7.example.invalid",
		}, false},
		{"git", func(f *nastoolstest.Fake) { f.GitErr = nastoolstest.ErrPrivate }, nil, false},
		{"compose config", func(f *nastoolstest.Fake) { f.FailCommand = "config" }, nil, false},
		{"compose pull", func(f *nastoolstest.Fake) { f.FailCommand = "pull" }, nil, false},
		{"compose up", func(f *nastoolstest.Fake) { f.FailCommand = "up" }, nil, false},
		{"compose ps", func(f *nastoolstest.Fake) { f.FailCommand = "ps" }, nil, false},
		{"compose logs", func(f *nastoolstest.Fake) { f.FailCommand = "logs" }, nil, false},
		{"open", func(f *nastoolstest.Fake) { f.OpenErr = nastoolstest.ErrPrivate }, nil, false},
		{"lock error", func(f *nastoolstest.Fake) { f.LockErrors = []error{nastoolstest.ErrPrivate} }, nil, false},
		{"lock busy", func(f *nastoolstest.Fake) { f.LockErrors = []error{nastools.ErrBusy} }, nil, false},
		{"serve", func(f *nastoolstest.Fake) { f.RemoteFail = "--https=443" }, nil, false},
		{"serve status", func(f *nastoolstest.Fake) { f.RemoteFail = "status" }, nil, false},
		{"bad status JSON", func(f *nastoolstest.Fake) {
			f.RemoteStatus = "not json " + strings.Join(canaries(), " ")
		}, nil, false},
	}
}

// faultyWriter fails every write from the numbered one on, and records a
// refused mask.
// It has no WriteString, so io.WriteString reaches Write.
type faultyWriter struct {
	buf        bytes.Buffer
	failAt     int
	writes     int
	maskFailed bool
}

func (w *faultyWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.failAt > 0 && w.writes >= w.failAt {
		w.maskFailed = w.maskFailed || bytes.Contains(p, []byte(maskPrefix))
		return 0, errBrokenPipe
	}
	w.buf.Write(p)
	return len(p), nil
}

func (w *faultyWriter) String() string { return w.buf.String() }

type matrixCase struct {
	name     string
	fixture  fixture
	command  string
	shape    string
	args     []string
	failAt   int
	noConfig bool
}

// matrixCases crosses every command and argument shape with every config
// change and injected fault, with standard output healthy, failing at once, or failing
// just after the discovery masks.
func matrixCases() []matrixCase {
	const canary = "secret-6.example.invalid"
	commands := map[string][]string{
		"check": {"check"}, "deploy": {"deploy"}, "status": {"status"}, "logs": {"logs"},
		"logs svc": {"logs", "web"}, "version": {"version"}, "unknown": {"unknown-command"},
		"help": {"help"}, "complete": {"__complete"}, "complete no desc": {"__completeNoDesc"},
	}
	shapes := map[string][]string{
		"valid":          nil,
		"bad tail":       {"--tail=" + canary},
		"bad lock wait":  {"--lock-wait", canary},
		"unknown flag":   {"--" + canary},
		"extra argument": {canary, canary},
		// The template references this value, so only discovery knows it.
		"template value": {"secret-5.example.invalid", "secret-5.example.invalid"},
		"help":           {"--help"},
		"short help":     {"-h"},
		"missing config": {"--config=missing.toml"},
	}
	// The fixture's discovery writes nine masks.
	writers := map[string]int{"healthy": 0, "first write fails": 1, "write after masks fails": 10}
	all := fixtures()
	cases := make([]matrixCase, 0, len(all)*len(commands)*len(shapes)*len(writers))
	for _, fx := range all {
		for command, commandArgs := range commands {
			for shape, shapeArgs := range shapes {
				for writer, failAt := range writers {
					cases = append(cases, matrixCase{
						name:     fx.name + "/" + command + "/" + shape + "/" + writer,
						fixture:  fx,
						command:  command,
						shape:    shape,
						args:     slices.Concat(commandArgs, shapeArgs),
						failAt:   failAt,
						noConfig: shape == "missing config",
					})
				}
			}
		}
	}
	return cases
}

func TestRunNeverPrintsPrivateValues(t *testing.T) {
	t.Parallel()
	for _, tc := range matrixCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := matrixFake()
			tc.fixture.change(f)
			out, errOut := &faultyWriter{failAt: tc.failAt}, &faultyWriter{}
			code := Run(t.Context(), tc.args, streams{in: strings.NewReader(""), out: out, err: errOut}, f,
				matrixEnv())
			masks := map[string]bool{"secret-6.example.invalid": true}
			if !tc.noConfig {
				masks = expectedMasks(tc.fixture.unmasked)
			}
			// nastools can't know a value whose only source it can't read, so
			// it may echo that value from its arguments.
			private := slices.DeleteFunc(canaries(), func(value string) bool {
				return slices.Contains(tc.fixture.unmasked, value) && slices.Contains(tc.args, value)
			})
			assertGated(t, out, errOut.String(), masks, private)
			if out.maskFailed {
				assert.NotEqual(t, exitOK, code)
			}
			if tc.failAt == 0 {
				assertExit(t, tc, code, errOut.String())
			}
		})
	}
}

// assertExit checks the exit status of every case whose outcome the fixture
// doesn't decide, and that an error that could echo the arguments prints
// only the generic message.
func assertExit(t *testing.T, tc matrixCase, code int, stderr string) {
	t.Helper()
	remote := !slices.Contains([]string{"version", "unknown", "help", "complete", "complete no desc"}, tc.command)
	complete := strings.HasPrefix(tc.command, "complete")
	switch {
	case complete:
		assert.Equal(t, exitError, code)
		assert.Equal(t, "nastools: invalid or missing input\n", stderr)
	case tc.shape == "help" || tc.shape == "short help":
		assert.Equal(t, tc.command == "unknown", code == exitError)
	case tc.shape != "valid", tc.command == "unknown":
		assert.Equal(t, exitError, code)
	case !remote:
		assert.Equal(t, exitOK, code)
	}
	if code != exitOK && (remote || tc.fixture.broken || tc.noConfig) {
		assert.True(t, strings.HasSuffix(stderr, ": invalid or missing input\n"), stderr)
	}
}

func expectedMasks(unmasked []string) map[string]bool {
	masks := make(map[string]bool)
	for _, value := range []string{
		"secret-1.example.invalid", "secret-2.example.invalid:22", "secret-2.example.invalid",
		"secret-3.example.invalid", "secret-4.example.invalid", "secret-5.example.invalid",
		"secret-6.example.invalid", "secret-7.example.invalid",
	} {
		masks[value] = true
	}
	for _, value := range unmasked {
		delete(masks, value)
	}
	return masks
}

// assertGated checks that neither stream contains a canary outside a mask
// command, and that with standard output healthy every expected mask comes before
// the first other line.
func assertGated(t *testing.T, out *faultyWriter, stderr string, masks map[string]bool, private []string) {
	t.Helper()
	before := make(map[string]bool)
	other := false
	for line := range strings.Lines(out.String()) {
		value, isMask := strings.CutPrefix(line, maskPrefix)
		if isMask && !other {
			before[strings.TrimSuffix(value, "\n")] = true
		}
		other = other || !isMask
		for _, canary := range private {
			if !isMask {
				assert.NotContains(t, line, canary)
			}
		}
	}
	for _, canary := range private {
		assert.NotContains(t, stderr, canary)
	}
	if out.failAt == 0 {
		for value := range masks {
			assert.True(t, before[value], "mask for %s", value)
		}
	}
}

// TestRunRedactsCanaries checks that the matrix can fail: the gate receives
// the fake's canaries on both streams and replaces them.
func TestRunRedactsCanaries(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	f := matrixFake()
	code := Run(t.Context(), []string{"deploy"}, streams{in: strings.NewReader(""), out: &out, err: &errOut}, f,
		matrixEnv())
	assert.Equal(t, exitOK, code)
	assert.Contains(t, out.String(), maskPrefix+tailnetName+"\n")
	assert.Contains(t, out.String(), "output *** *** *** *** *** *** *** ***\n")
	assert.Contains(t, errOut.String(), "output *** *** *** *** *** *** *** ***\n")
	assert.Contains(t, out.String(), `"***:443"`)
}

func TestLocalOutputUnchanged(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	env := matrixEnv()
	delete(env, "GITHUB_ACTIONS")
	code := Run(t.Context(), []string{"deploy"}, streams{in: strings.NewReader(""), out: &out, err: &out}, matrixFake(),
		env)
	assert.Equal(t, exitOK, code)
	assert.NotContains(t, out.String(), maskPrefix)
	assert.Contains(t, out.String(), "output "+strings.Join(canaries(), " ")+"\n")
}

// TestDiscoveryFollowsConfigFlag checks that discovery reads the config the
// flags name, even when flag parsing stops before reaching it.
func TestDiscoveryFollowsConfigFlag(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"deploy", "--unknown", "--config", "deployment/custom.toml"},
		{"deploy", "--lock-wait", "secret-6.example.invalid", "--config=deployment/custom.toml"},
		{"logs", "--help", "--config", "deployment/custom.toml"},
		{"deploy", "--config", "nastools.toml", "--config", "deployment/custom.toml", "--", "--config", "x"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			f := &nastoolstest.Fake{Files: map[string]string{"deployment/custom.toml": matrixConfig}}
			var out bytes.Buffer
			Run(t.Context(), args, streams{in: strings.NewReader(""), out: &out, err: &out}, f, matrixEnv())
			require.NotEmpty(t, f.Calls)
			assert.Equal(t, "read:deployment/custom.toml", f.Calls[0])
			assert.True(t, strings.HasPrefix(out.String(), maskPrefix+"secret-6.example.invalid\n"+
				maskPrefix+"secret-1.example.invalid\n"), out.String())
		})
	}
}

func TestMaskFailureStopsBeforeExecuting(t *testing.T) {
	t.Parallel()
	f := matrixFake()
	out, errOut := &faultyWriter{failAt: 1}, &faultyWriter{}
	code := Run(t.Context(), []string{"deploy"}, streams{in: strings.NewReader(""), out: out, err: errOut}, f,
		matrixEnv())
	assert.Equal(t, exitError, code)
	assert.Equal(t, []string{"read:nastools.toml", "read:expanded.txt", "read:static.txt"}, f.Calls)
	assert.Equal(t, "nastools: mask output: invalid or missing input\n", errOut.String())
}

// stderrOnly writes the host name to standard error alone, so after
// discovery only its mask command goes to standard output.
type stderrOnly struct {
	*nastoolstest.Fake
}

func (s stderrOnly) Compose(ctx context.Context, args, env []string, _, errOut io.Writer) error {
	if errOut != nil {
		if _, err := io.WriteString(errOut, tailnetName+"\n"); err != nil {
			return fmt.Errorf("compose: %w", err)
		}
	}
	if err := s.Fake.Compose(ctx, args, env, nil, nil); err != nil {
		return fmt.Errorf("compose: %w", err)
	}
	return nil
}

func TestLateMaskFailureFailsRun(t *testing.T) {
	t.Parallel()
	f := matrixFake()
	f.Canaries = nil
	f.Files["nastools.toml"] = strings.Split(matrixConfig, "[[serve]]")[0]
	out, errOut := &faultyWriter{failAt: 10}, &faultyWriter{}
	code := Run(t.Context(), []string{"deploy"}, streams{in: strings.NewReader(""), out: out, err: errOut},
		stderrOnly{f}, matrixEnv())
	assert.True(t, out.maskFailed)
	assert.Equal(t, exitError, code)
	assert.Equal(t, strings.Repeat("***\n", 3), errOut.String())
}
