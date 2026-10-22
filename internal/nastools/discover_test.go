// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package nastools_test

import (
	"io/fs"
	"maps"
	"slices"
	"strings"
	"testing"
	"unicode"

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
		"SSH_LOCK_DIR":    "/secret-10.example.invalid/locks",
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
	private := nastools.Discover(f.ReadFile, "deployment/nastools.toml", env)
	return slices.Concat(private.Values, private.Hosts)
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
		"lock directory variable": discoverBase + "required_env = [\"REQUIRED_INPUT\"]\n" +
			strings.Replace(ssh, "lock_dir = \"/example.invalid/locks\"", "lock_dir_env = \"SSH_LOCK_DIR\"", 1),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assertSuperset(t, config)
		})
	}
}

// TestDiscoverLockDirEnv checks that discovery finds the lock directory
// value with the key in any case, with or without a syntax error.
func TestDiscoverLockDirEnv(t *testing.T) {
	t.Parallel()
	for name, config := range map[string]string{
		"lower case":                   "[ssh]\nlock_dir_env = \"SSH_LOCK_DIR\"\n",
		"upper case":                   "[SSH]\nLOCK_DIR_ENV = \"SSH_LOCK_DIR\"\n",
		"lower case with syntax error": "project = = 'example'\n[ssh]\nlock_dir_env = \"SSH_LOCK_DIR\"\n",
		"upper case with syntax error": "project = = 'example'\n[SSH]\nLOCK_DIR_ENV = \"SSH_LOCK_DIR\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := discover(discoverFiles(discoverBase+config), discoverEnv())
			assert.Contains(t, got, "/secret-10.example.invalid/locks")
			assert.NotContains(t, got, "secret-9.example.invalid")
		})
	}
}

// FuzzDiscoverAnyCase runs the TestDiscoverSuperset check on valid configs
// whose keys use any case, as the strict decoder accepts. Bit n of casing
// upper-cases letter n of the keys, wrapping after 64.
func FuzzDiscoverAnyCase(f *testing.F) {
	for i := range uint64(64) {
		f.Add(i*0x9e3779b97f4a7c15, i%2 == 0)
	}
	f.Fuzz(func(t *testing.T, casing uint64, tables bool) {
		bit := 0
		k := func(key string) string {
			runes := []rune(key)
			for i := range runes {
				if casing>>(bit%64)&1 == 1 {
					runes[i] = unicode.ToUpper(runes[i])
				}
				bit++
			}
			return string(runes)
		}
		config := k("project") + " = \"example\"\n" + k("compose") + " = [\"compose.yaml\"]\n"
		if tables {
			config += k("required_env") + " = [\"REQUIRED_INPUT\"]\n[" + k("ssh") + "]\n" +
				k("address_env") + " = \"SSH_ADDRESS\"\n" + k("user_env") + " = \"SSH_USER\"\n" +
				k("known_hosts_env") + " = \"SSH_KNOWN_HOSTS\"\n" + k("lock_dir") + " = \"/example.invalid/locks\"\n" +
				"[" + k("files") + "]\nSTATIC_INPUT = \"static.txt\"\n[" + k("expand_files") + "]\nA_OUTPUT = \"a.txt\"\n"
		} else {
			config += k(
				"expand_files",
			) + ".B_OUTPUT = \"b.txt\"\n" + k(
				"files",
			) + " = { STATIC_INPUT = \"static.txt\" }\n" +
				k(
					"required_env",
				) + " = [\"REQUIRED_INPUT\"]\n" + k(
				"ssh",
			) + " = { " +
				k(
					"address_env",
				) + " = \"SSH_ADDRESS\", " + k(
				"user_env",
			) + " = \"SSH_USER\", " +
				k(
					"known_hosts_env",
				) + " = \"SSH_KNOWN_HOSTS\", " + k(
				"lock_dir",
			) + " = \"/example.invalid/locks\" }\n"
		}
		assertSuperset(t, config)
	})
}

func assertSuperset(t *testing.T, config string) {
	t.Helper()
	env := discoverEnv()
	got := discover(discoverFiles(config), env)
	strict := func(env map[string]string) error {
		f := &nastoolstest.Fake{Files: discoverFiles(config)}
		return nastools.Execute(t.Context(), f, "deployment/nastools.toml", env, nil, nil,
			nastools.Command{Name: nastools.CheckName})
	}
	require.NoError(t, strict(env), config)
	noticed := 0
	for _, name := range slices.Sorted(maps.Keys(env)) {
		reduced := maps.Clone(env)
		delete(reduced, name)
		if strict(reduced) != nil {
			noticed++
			assert.Contains(t, got, env[name], name)
		}
	}
	assert.GreaterOrEqual(t, noticed, 4, config)
	assert.NotContains(t, got, env["UNUSED_INPUT"])
	if strings.Contains(config, "static.txt") {
		assert.Contains(t, got, "secret-7.example.invalid")
	}
	assert.NotContains(t, got, "short")
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
	type toleranceCase struct {
		name   string
		config string
		files  func(map[string]string)
		want   []string
	}
	fixed := []toleranceCase{
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
	}
	generated := make([]toleranceCase, 0, 21)
	// The strict decoder matches a key in any case, so each variant must
	// still yield every value, with or without a syntax error.
	for _, variant := range [][2]string{
		{"required_env", "REQUIRED_ENV"},
		{"[ssh]", "[SSH]"},
		{"address_env", "ADDRESS_ENV"},
		{"[ files ]", "[ FILES ]"},
		{"[expand_files]", "[EXPAND_FILES]"},
	} {
		config := strings.Replace(tail, variant[0], variant[1], 1)
		generated = append(generated,
			toleranceCase{variant[1], config, nil, all},
			toleranceCase{variant[1] + " with unknown field", "unknown = 'example'\n" + config, nil, all},
			toleranceCase{variant[1] + " with syntax error", "project = = 'example'\n" + config, nil, all})
	}
	for _, form := range []string{
		"expand_files.A_OUTPUT = \"a.txt\"\nEXPAND_FILES . B_OUTPUT = 'b.txt'\n",
		"expand_files = { A_OUTPUT = \"a.txt\", B_OUTPUT = 'b.txt' }\n",
		"Expand_Files = {A_OUTPUT='a.txt',B_OUTPUT=\"b.txt\"}\n",
	} {
		for _, broken := range []string{"project = = 'example'\n", "unknown = 'example'\n"} {
			generated = append(generated, toleranceCase{
				broken + form, broken + form + "[files]\nSTATIC_INPUT = 'static.txt'\n",
				nil,
				[]string{"secret-5.example.invalid", "secret-6.example.invalid", "secret-7.example.invalid"},
			})
		}
	}
	for _, tc := range slices.Concat(fixed, generated) {
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

// TestDiscoverHosts checks that only the address and known-hosts values
// count as hosts, unless the config fails to parse.
func TestDiscoverHosts(t *testing.T) {
	t.Parallel()
	config := discoverBase + "required_env = [\"REQUIRED_INPUT\"]\n[ssh]\naddress_env = \"SSH_ADDRESS\"\n" +
		"user_env = \"SSH_USER\"\nknown_hosts_env = \"SSH_KNOWN_HOSTS\"\n[files]\nSTATIC_INPUT = \"static.txt\"\n"
	env := discoverEnv()
	f := &nastoolstest.Fake{Files: discoverFiles(config)}
	private := nastools.Discover(f.ReadFile, "deployment/nastools.toml", env)
	assert.Equal(t, []string{env["SSH_ADDRESS"], env["SSH_KNOWN_HOSTS"]}, private.Hosts)
	assert.Equal(t, []string{"", env["REQUIRED_INPUT"], env["SSH_USER"], "secret-7.example.invalid"}, private.Values)

	f = &nastoolstest.Fake{Files: discoverFiles("project = = 'x'\n" + config)}
	private = nastools.Discover(f.ReadFile, "deployment/nastools.toml", env)
	assert.Equal(t, []string{env["REQUIRED_INPUT"], env["SSH_ADDRESS"], env["SSH_USER"], env["SSH_KNOWN_HOSTS"]},
		private.Hosts)
}

// TestDiscoverBoundedReads checks that discovery skips a named pipe and a
// device without opening them, and drops a file over the size limit, read
// only to its limit. The strict path then finds each input unreadable.
func TestDiscoverBoundedReads(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("secret-8.example.invalid\n", 1<<20/25+1)
	config := discoverBase + "[files]\nZERO = \"zero\"\nPIPE = \"pipe\"\nBIG = \"big.txt\"\n" +
		"STATIC_INPUT = \"static.txt\"\n[expand_files]\nENDLESS = \"endless\"\n"
	for name, path := range map[string]string{
		"zero": "deployment/zero", "pipe": "deployment/pipe", "big": "deployment/big.txt",
		"endless": "deployment/endless", "config": "deployment/nastools.toml",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			files := discoverFiles(config)
			files["deployment/big.txt"] = big
			f := &nastoolstest.Fake{Files: files, Streams: map[string]fs.FileMode{
				"deployment/zero":    fs.ModeDevice | fs.ModeCharDevice,
				"deployment/pipe":    fs.ModeNamedPipe,
				"deployment/endless": 0,
			}}
			if name == "config" {
				f.Streams[path] = fs.ModeNamedPipe
			}
			ops := nastools.Cached(f)
			private := nastools.Discover(ops.ReadFile, "deployment/nastools.toml", discoverEnv())
			if name == "config" {
				assert.Equal(t, nastools.Private{Values: []string{""}}, private)
				assert.False(t, nastools.Parses(ops.ReadFile, path))
			} else {
				assert.Contains(t, private.Values, "secret-7.example.invalid")
			}
			assert.NotContains(t, private.Values, "secret-8.example.invalid")
			assert.NotContains(t, f.Opened, "deployment/zero")
			assert.NotContains(t, f.Opened, "deployment/pipe")
			_, err := ops.ReadFile(path)
			require.Error(t, err)
		})
	}
}

func TestReadInputLimit(t *testing.T) {
	t.Parallel()
	f := &nastoolstest.Fake{Files: map[string]string{
		"limit.txt": strings.Repeat("x", 1<<20),
		"over.txt":  strings.Repeat("x", 1<<20+1),
	}}
	data, err := nastools.ReadInput(f, "limit.txt")
	require.NoError(t, err)
	assert.Len(t, data, 1<<20)
	_, err = nastools.ReadInput(f, "over.txt")
	require.Error(t, err)
	_, err = nastools.ReadInput(f, "missing.txt")
	require.ErrorIs(t, err, nastoolstest.ErrPrivate)
}
