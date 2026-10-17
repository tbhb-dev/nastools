// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package buildmeta_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tbhb-dev/nastools/internal/buildmeta"
)

func TestGetReturnsPackageVars(t *testing.T) {
	t.Parallel()
	got := buildmeta.Get()
	assert.Equal(t, buildmeta.Version, got.Version)
	assert.Equal(t, buildmeta.Commit, got.Commit)
	assert.Equal(t, buildmeta.Date, got.Date)
}
