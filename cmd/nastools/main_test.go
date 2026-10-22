// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tbhb-dev/nastools/internal/nastools"
)

func TestReadFileBounded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	small, big := filepath.Join(dir, "small.txt"), filepath.Join(dir, "big.txt")
	require.NoError(t, os.WriteFile(small, []byte("example\n"), 0o600))
	require.NoError(t, os.WriteFile(big, []byte(strings.Repeat("x", 1<<20+1)), 0o600))
	ops := &operations{}
	data, err := ops.ReadFile(small)
	require.NoError(t, err)
	assert.Equal(t, "example\n", string(data))
	for _, path := range []string{big, dir, filepath.Join(dir, "missing.txt")} {
		_, err = ops.ReadFile(path)
		require.ErrorIs(t, err, nastools.ErrInvalid, path)
	}
}

// TestOpenFileRefusesPipe opens a named pipe directly, as if it replaced a
// regular file after Stat. The open must neither block nor succeed.
func TestOpenFileRefusesPipe(t *testing.T) {
	t.Parallel()
	pipe := filepath.Join(t.TempDir(), "pipe")
	require.NoError(t, syscall.Mkfifo(pipe, 0o600))
	done := make(chan error, 1)
	go func() {
		file, err := (&operations{}).OpenFile(pipe)
		if err == nil {
			_ = file.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		require.ErrorIs(t, err, nastools.ErrInvalid)
	case <-time.After(10 * time.Second):
		t.Fatal("open blocked on a named pipe")
	}
}
