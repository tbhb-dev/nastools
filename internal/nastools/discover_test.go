// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package nastools_test

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tbhb-dev/nastools/internal/nastools"
	"github.com/tbhb-dev/nastools/internal/nastools/nastoolstest"
)

const discoverBase = `project = "example"
compose = ["compose.yaml"]
`

func discoverEnv() map[string]string {
	return map[string]string{
		"REQUIRED_INPUT":  "secret-1.example.invalid",
		"SSH_ADDRESS":     "secret-2.example.invalid:22",
		"SSH_USER":        "secret-3.example.invalid",
		"SSH_KNOWN_HOSTS": knownHostsText,
		"TEMPLATE_A":      "secret-5.example.invalid",
		"TEMPLATE_B":      "secret-6.example.invalid",
		"UNUSED_INPUT":    "secret-9.example.invalid",
	}
}

func discoverFiles(config string) map[string]string {
	return map[string]string{
		"deployment/nastools.toml": config,
		"deployment/a.txt":         "a=${TEMPLATE_A}\n",
		"deployment/b.txt":         "b=$TEMPLATE_B $TEMPLATE_B\n",
		"deployment/static.txt":    "short\n  secret-7.example.invalid  \r\n}\n",
	}
}

func discover(files, env map[string]string) []string {
	f := &nastoolstest.Fake{Files: files}
	return nastools.Discover(f.ReadFile, "deployment/nastools.toml", env)
}

// TestDiscoverSuperset checks that Discover finds every value whose absence
// the strict path notices, and every static file line it would redact.
func TestDiscoverSuperset(t *testing.T) {
	t.Parallel()
	ssh := "[ssh]\naddress_env = \"SSH_ADDRESS\"\nuser_env = \"SSH_USER\"\n" +
		"known_hosts_env = \"SSH_KNOWN_HOSTS\"\nlock_dir = \"/example.invalid/locks\"\n"
	for name, config := range map[string]string{
		"tables": discoverBase + "required_env = [\"REQUIRED_INPUT\"]\n" + ssh +
			"[files]\nSTATIC_INPUT = \"static.txt\"\n[expand_files]\nA_OUTPUT = \"a.txt\"\nB_OUTPUT = \"b.txt\"\n",
		"inline": discoverBase + "required_env = [\n  \"REQUIRED_INPUT\",\n]\n" +
			"expand_files = { A_OUTPUT = \"a.txt\" }\nfiles = { STATIC_INPUT = \"static.txt\" }\n" + ssh,
		"dotted": discoverBase + "ssh.address_env = \"SSH_ADDRESS\"\nssh.user_env = \"SSH_USER\"\n" +
			"ssh.known_hosts_env = \"SSH_KNOWN_HOSTS\"\nssh.lock_dir = \"/example.invalid/locks\"\n" +
			"expand_files.B_OUTPUT = \"b.txt\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := discoverEnv()
			got := discover(discoverFiles(config), env)
			strict := func(env map[string]string) error {
				f := &nastoolstest.Fake{Files: discoverFiles(config)}
				return nastools.Execute(t.Context(), f, "deployment/nastools.toml", env, nil, nil,
					nastools.Command{Name: nastools.CheckName})
			}
			require.NoError(t, strict(env))
			for _, name := range slices.Sorted(maps.Keys(env)) {
				reduced := maps.Clone(env)
				delete(reduced, name)
				if strict(reduced) != nil {
					assert.Contains(t, got, env[name], name)
				}
			}
			assert.NotContains(t, got, env["UNUSED_INPUT"])
			if strings.Contains(config, "static.txt") {
				assert.Contains(t, got, "secret-7.example.invalid")
			}
			assert.NotContains(t, got, "short")
		})
	}
}

func TestDiscoverTolerance(t *testing.T) {
	t.Parallel()
	all := []string{
		"secret-1.example.invalid", "secret-2.example.invalid:22", "secret-3.example.invalid",
		knownHostsText, "secret-5.example.invalid", "secret-6.example.invalid", "secret-7.example.invalid",
	}
	tail := "required_env = [\"REQUIRED_INPUT\"]\n[ssh]\naddress_env = \"SSH_ADDRESS\"\n" +
		"user_env = 'SSH_USER'\nknown_hosts_env = \"SSH_KNOWN_HOSTS\"\n" +
		"[ files ] # static\nSTATIC_INPUT = \"static.txt\"\n[expand_files]\nA_OUTPUT = \"a.txt\"\nB_OUTPUT = \"b.txt\"\n"
	for _, tc := range []struct {
		name   string
		config string
		files  func(map[string]string)
		want   []string
	}{
		{"syntax error", "project = = 'example'\n" + tail, nil, all},
		{"syntax error after tables", tail + "[[serve]]\nservice = svc:web\n", nil, all},
		{"unknown field first", "unknown = 'example'\n" + tail, nil, all},
		{"wrong types", "wait_seconds = 'soon'\ncompose = 'compose.yaml'\n" +
			strings.Replace(tail, "[\"REQUIRED_INPUT\"]", "\"REQUIRED_INPUT\"", 1), nil, all},
		{"missing ssh", "required_env = [\"REQUIRED_INPUT\"]\n[expand_files]\nA_OUTPUT = \"a.txt\"\n", nil, []string{
			"secret-1.example.invalid", "secret-5.example.invalid",
		}},
		{"unreadable first template", tail, func(files map[string]string) {
			delete(files, "deployment/a.txt")
		}, slices.DeleteFunc(slices.Clone(all), func(v string) bool { return v == "secret-5.example.invalid" })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := discoverFiles(tc.config)
			if tc.files != nil {
				tc.files(files)
			}
			got := discover(files, discoverEnv())
			for _, value := range tc.want {
				assert.Contains(t, got, value)
			}
			assert.NotContains(t, got, "secret-9.example.invalid")
		})
	}
}

func TestDiscoverWithoutConfig(t *testing.T) {
	t.Parallel()
	env := map[string]string{"NASTOOLS_PRIVATE_STRINGS": " extra.example.invalid, second.example.invalid"}
	assert.Equal(t, []string{"extra.example.invalid", "second.example.invalid"}, discover(nil, env))
}

func FuzzDiscover(f *testing.F) {
	f.Add(discoverBase+"required_env = ['REQUIRED_INPUT']\n[expand_files]\nX = 'a.txt'\n", "${TEMPLATE_A}")
	f.Add("[files\nX = \"static.txt\"\n'REQUIRED_INPUT' = \"\n", "$")
	f.Add("[ssh]\naddress_env = [[1], {a = 'SSH_ADDRESS'}]\n", "${")
	f.Fuzz(func(t *testing.T, config, template string) {
		read := func(path string) ([]byte, error) {
			switch {
			case path == "nastools.toml":
				return []byte(config), nil
			case template == "":
				return nil, nastoolstest.ErrPrivate
			}
			return []byte(template), nil
		}
		assert.NotPanics(t, func() { nastools.Discover(read, "nastools.toml", discoverEnv()) })
	})
}

func TestCachedReadsOnce(t *testing.T) {
	t.Parallel()
	f := &nastoolstest.Fake{Files: map[string]string{"nastools.toml": "example"}}
	ops := nastools.Cached(f)
	for range 2 {
		data, err := ops.ReadFile("nastools.toml")
		require.NoError(t, err)
		assert.Equal(t, "example", string(data))
		_, err = ops.ReadFile("missing.toml")
		require.ErrorIs(t, err, nastoolstest.ErrPrivate)
	}
	assert.Equal(t, []string{"read:nastools.toml", "read:missing.toml"}, f.Calls)
}
