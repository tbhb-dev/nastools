// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package nastools

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestValidateNilLockDir tests the method itself. Prepare sets SSH.LockDir
// before every call, so no exported function can pass it a nil pointer.
func TestValidateNilLockDir(t *testing.T) {
	t.Parallel()
	err := Config{Project: "example"}.validate()
	require.ErrorIs(t, err, ErrInvalid)
	require.EqualError(t, err, "ssh.lock_dir: invalid or missing input")
}
