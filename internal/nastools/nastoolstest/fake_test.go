// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package nastoolstest_test

import (
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tbhb-dev/nastools/internal/nastools"
	"github.com/tbhb-dev/nastools/internal/nastools/nastoolstest"
)

var errBroken = errors.New("broken pipe")

// chunks records each write separately.
type chunks struct {
	writes []string
	fail   bool
}

func (c *chunks) Write(p []byte) (int, error) {
	if c.fail {
		return 0, errBroken
	}
	c.writes = append(c.writes, string(p))
	return len(p), nil
}

// TestCanariesReachBothWriters guards the Run matrix against passing
// vacuously: the canaries must really arrive, split across writes.
func TestCanariesReachBothWriters(t *testing.T) {
	t.Parallel()
	canaries := []string{"secret-1.example.invalid", "node.example.ts.net"}
	f := &nastoolstest.Fake{Canaries: canaries, RemoteStatus: "{}", FailCommand: "up"}
	out, errOut := &chunks{}, &chunks{}
	require.NoError(t, f.Compose(t.Context(), []string{"pull"}, nil, out, errOut))
	require.NoError(t, f.Remote(t.Context(), []string{"tailscale", "serve", "status"}, out, errOut))
	err := f.Compose(t.Context(), []string{"up"}, nil, out, errOut)
	require.ErrorIs(t, err, nastoolstest.ErrPrivate)
	for _, canary := range canaries {
		assert.Contains(t, err.Error(), canary)
		assert.Equal(t, 2, strings.Count(strings.Join(out.writes, ""), canary))
		assert.Equal(t, 3, strings.Count(strings.Join(errOut.writes, ""), canary))
		for _, write := range append(out.writes, errOut.writes...) {
			assert.NotContains(t, write, canary)
		}
	}
	assert.True(t, strings.HasPrefix(strings.Join(out.writes, ""), "compose output\n"))
}

func TestFaultsCarryCanaries(t *testing.T) {
	t.Parallel()
	f := &nastoolstest.Fake{
		Canaries:   []string{"secret-1.example.invalid"},
		GitErr:     nastoolstest.ErrPrivate,
		OpenErr:    nastoolstest.ErrPrivate,
		PauseErr:   nastoolstest.ErrPrivate,
		LockErrors: []error{nastools.ErrBusy},
		RemoteFail: "serve",
	}
	_, readErr := f.ReadFile("missing.toml")
	_, gitErr := f.GitState(t.Context(), ".")
	_, openErr := f.Open(t.Context(), nastools.SSH{}, nil)
	lockErr := f.Lock(t.Context(), "/example.invalid/locks/example.lock")
	remoteErr := f.Remote(t.Context(), []string{"tailscale", "serve"}, &chunks{}, &chunks{})
	for _, err := range []error{readErr, gitErr, openErr, lockErr, remoteErr, f.Pause(t.Context())} {
		assert.Contains(t, err.Error(), "secret-1.example.invalid")
	}
	require.ErrorIs(t, lockErr, nastools.ErrBusy)
	require.NoError(t, f.Lock(t.Context(), "/example.invalid/locks/example.lock"))
	f.Close()
	assert.Equal(t, 1, f.Closed)
}

func TestWriteFailuresCarryCanaries(t *testing.T) {
	t.Parallel()
	f := &nastoolstest.Fake{Canaries: []string{"secret-1.example.invalid"}, RemoteStatus: "{}"}
	broken := &chunks{fail: true}
	for _, err := range []error{
		f.Compose(t.Context(), []string{"ps"}, nil, broken, &chunks{}),
		f.Compose(t.Context(), []string{"ps"}, nil, &chunks{}, broken),
		f.Remote(t.Context(), []string{"status"}, broken, &chunks{}),
		f.Remote(t.Context(), []string{"serve"}, &chunks{}, broken),
	} {
		require.ErrorIs(t, err, nastoolstest.ErrPrivate)
		assert.Contains(t, err.Error(), "secret-1.example.invalid")
	}
}

func TestFileAPI(t *testing.T) {
	t.Parallel()
	f := &nastoolstest.Fake{
		Files:   map[string]string{"file.txt": "example"},
		Streams: map[string]fs.FileMode{"zero": fs.ModeDevice, "pipe": fs.ModeNamedPipe},
	}
	data, err := f.ReadFile("file.txt")
	require.NoError(t, err)
	assert.Equal(t, "example", string(data))
	_, err = f.ReadFile("missing.txt")
	require.ErrorIs(t, err, nastoolstest.ErrPrivate)
	info, err := f.Stat("zero")
	require.NoError(t, err)
	assert.Equal(t, fs.ModeDevice, info.Mode())
	zero, err := f.OpenFile("zero")
	require.NoError(t, err)
	buf := []byte("xyz")
	n, err := io.ReadFull(zero, buf)
	require.NoError(t, err)
	assert.Equal(t, []byte{0, 0, 0}, buf[:n])
	require.NoError(t, zero.Close())
	_, err = f.OpenFile("pipe")
	require.Error(t, err)
	_, err = f.OpenFile("missing.txt")
	require.ErrorIs(t, err, nastoolstest.ErrPrivate)
	assert.Equal(t, []string{"read:file.txt", "read:missing.txt", "read:zero"}, f.Calls)
	assert.Equal(t, []string{"file.txt", "zero", "pipe", "missing.txt"}, f.Opened)
}

// TestVersionProbe checks the probe's output, environment and error, and
// that the probe leaves the recording fields empty.
func TestVersionProbe(t *testing.T) {
	t.Parallel()
	probe := []string{"version", "--short"}
	f := &nastoolstest.Fake{
		Canaries:    []string{"secret-1.example.invalid"},
		ComposeErr:  nastoolstest.ErrPrivate,
		FailCommand: "version",
	}
	var out strings.Builder
	require.NoError(t, f.Compose(t.Context(), probe, []string{"A=1"}, &out, nil))
	assert.Equal(t, "5.6.1\n", out.String())
	assert.Equal(t, []string{"A=1"}, f.VersionEnv)
	assert.Empty(t, f.Calls)
	assert.Empty(t, f.Commands)
	assert.Empty(t, f.Environments)
	assert.Nil(t, f.Args)
	assert.Nil(t, f.Env)

	f.ComposeVersion = []byte{}
	out.Reset()
	require.NoError(t, f.Compose(t.Context(), probe, nil, &out, nil))
	assert.Empty(t, out.String())

	f.ComposeVersion = []byte("2.23.0\n")
	f.VersionErr = nastoolstest.ErrPrivate
	err := f.Compose(t.Context(), probe, nil, &out, nil)
	require.ErrorIs(t, err, nastoolstest.ErrPrivate)
	assert.Contains(t, err.Error(), "secret-1.example.invalid")
	assert.Equal(t, "2.23.0\n", out.String())

	err = f.Compose(t.Context(), probe, nil, &chunks{fail: true}, nil)
	require.ErrorIs(t, err, nastoolstest.ErrPrivate)
	assert.Contains(t, err.Error(), "secret-1.example.invalid")
}
