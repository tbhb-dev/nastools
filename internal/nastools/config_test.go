// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package nastools_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tbhb-dev/nastools/internal/nastools"
)

func TestConfigPolicy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*nastools.Config)
		field  string
	}{
		{"project", func(c *nastools.Config) { c.Project = "" }, "project"},
		{"compose absent", func(c *nastools.Config) { c.Compose = nil }, "compose"},
		{"compose empty", func(c *nastools.Config) { c.Compose = []string{""} }, "compose"},
		{"wait", func(c *nastools.Config) { c.WaitSeconds = 0 }, "wait_seconds"},
		{"required name", func(c *nastools.Config) { c.RequiredEnv = []string{""} }, "required_env"},
		{"ssh name", func(c *nastools.Config) { c.SSH.UserEnv = "" }, "ssh"},
		{"file name", func(c *nastools.Config) { c.Files = map[string]string{"": "input"} }, "files"},
		{"file path", func(c *nastools.Config) { c.Files = map[string]string{"INPUT": ""} }, "files"},
		{"expanded path", func(c *nastools.Config) { c.ExpandFiles = map[string]string{"INPUT": ""} }, "files"},
		{"duplicate output", func(c *nastools.Config) {
			c.Files = map[string]string{"INPUT": "static"}
			c.ExpandFiles = map[string]string{"INPUT": "expanded"}
		}, "files.INPUT"},
		{"duplicate endpoint", func(c *nastools.Config) {
			c.Serve = []nastools.Endpoint{
				{Service: "svc:example", HTTPS: 443, Target: "http://example.invalid:3000"},
				{Service: "svc:example", HTTPS: 443, Target: "https://example.invalid"},
			}
		}, "serve[1]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := config()
			tc.change(&c)
			f := &fakeOperations{}
			_, err := nastools.Prepare(t.Context(), f, "nastools.toml", c, environment())
			require.ErrorIs(t, err, nastools.ErrInvalid)
			assert.Contains(t, err.Error(), tc.field)
			assert.NotContains(t, err.Error(), "private.example.invalid")
			assert.Empty(t, f.calls)
		})
	}
}

func TestServePolicy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		endpoint nastools.Endpoint
		valid    bool
	}{
		{"https", nastools.Endpoint{Service: "svc:example", HTTPS: 443, Target: "http://example.invalid:3000"}, true},
		{"https target", nastools.Endpoint{Service: "svc:example", HTTPS: 443, Target: "https://example.invalid"}, true},
		{"tcp", nastools.Endpoint{Service: "svc:example", TCP: 4317, Target: "tcp://example.invalid:4317"}, true},
		{"https maximum port", nastools.Endpoint{Service: "svc:example", HTTPS: 65535, Target: "https://example.invalid"}, true},
		{"tcp maximum port", nastools.Endpoint{Service: "svc:example", TCP: 65535, Target: "tcp://example.invalid:4317"}, true},
		{"https port overflow", nastools.Endpoint{Service: "svc:example", HTTPS: 65536, Target: "https://example.invalid"}, false},
		{"tcp port overflow", nastools.Endpoint{Service: "svc:example", TCP: 65536, Target: "tcp://example.invalid:4317"}, false},
		{"empty service name", nastools.Endpoint{Service: "svc:", HTTPS: 443, Target: "http://example.invalid"}, false},
		{"bad service", nastools.Endpoint{Service: "example", HTTPS: 443, Target: "http://example.invalid"}, false},
		{"both ports", nastools.Endpoint{Service: "svc:example", HTTPS: 443, TCP: 4317, Target: "http://example.invalid"}, false},
		{"no port", nastools.Endpoint{Service: "svc:example", Target: "http://example.invalid"}, false},
		{"negative port", nastools.Endpoint{Service: "svc:example", TCP: -1, Target: "tcp://example.invalid:4317"}, false},
		{"missing target", nastools.Endpoint{Service: "svc:example", HTTPS: 443}, false},
		{"wrong scheme", nastools.Endpoint{Service: "svc:example", HTTPS: 443, Target: "tcp://example.invalid:4317"}, false},
		{"tcp requires port", nastools.Endpoint{Service: "svc:example", TCP: 4317, Target: "tcp://example.invalid"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := config()
			c.Serve = []nastools.Endpoint{tc.endpoint}
			_, err := nastools.Prepare(t.Context(), &fakeOperations{}, "nastools.toml", c, environment())
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "serve[0]")
			}
		})
	}
}

func TestServiceOnDistinctPorts(t *testing.T) {
	t.Parallel()
	c := config()
	c.Serve = []nastools.Endpoint{
		{Service: "svc:otlp", TCP: 4317, Target: "tcp://example.invalid:4317"},
		{Service: "svc:otlp", TCP: 4318, Target: "tcp://example.invalid:4318"},
	}
	_, err := nastools.Prepare(t.Context(), &fakeOperations{}, "nastools.toml", c, environment())
	require.NoError(t, err)
}
