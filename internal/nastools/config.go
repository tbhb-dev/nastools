// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

// Package nastools owns deployment input and command policy.
package nastools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	Project     string            `toml:"project"`
	Compose     []string          `toml:"compose"`
	WaitSeconds int               `toml:"wait_seconds"`
	RequiredEnv []string          `toml:"required_env"`
	SSH         SSH               `toml:"ssh"`
	Files       map[string]string `toml:"files"`
	ExpandFiles map[string]string `toml:"expand_files"`
	Serve       []Endpoint        `toml:"serve"`
}

type SSH struct {
	AddressEnv    string `toml:"address_env"`
	UserEnv       string `toml:"user_env"`
	KnownHostsEnv string `toml:"known_hosts_env"`
}

type Endpoint struct {
	Service string `toml:"service"`
	HTTPS   int    `toml:"https"`
	TCP     int    `toml:"tcp"`
	Target  string `toml:"target"`
}

var ErrInvalid = errors.New("invalid or missing input")

// parse deliberately discards parser diagnostics, which can contain input values.
func parse(data []byte) (Config, error) {
	const defaultWaitSeconds = 180
	c := Config{WaitSeconds: defaultWaitSeconds}
	if err := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields().Decode(&c); err != nil {
		return c, fmt.Errorf("nastools.toml: %w", ErrInvalid)
	}
	return c, nil
}

func (c Config) validate() error {
	if c.Project == "" {
		return fmt.Errorf("project: %w", ErrInvalid)
	}
	if len(c.Compose) == 0 {
		return fmt.Errorf("compose: %w", ErrInvalid)
	}
	if slices.Contains(c.Compose, "") {
		return fmt.Errorf("compose: %w", ErrInvalid)
	}
	if c.WaitSeconds <= 0 {
		return fmt.Errorf("wait_seconds: %w", ErrInvalid)
	}
	if slices.Contains([]string{c.SSH.AddressEnv, c.SSH.UserEnv, c.SSH.KnownHostsEnv}, "") {
		return fmt.Errorf("ssh: %w", ErrInvalid)
	}
	if slices.Contains(c.RequiredEnv, "") {
		return fmt.Errorf("required_env: %w", ErrInvalid)
	}
	type endpointKey struct {
		service string
		port    int
	}
	seen := make(map[endpointKey]bool)
	for i, endpoint := range c.Serve {
		key := endpointKey{endpoint.Service, endpoint.HTTPS + endpoint.TCP}
		if !endpoint.valid() || seen[key] {
			return fmt.Errorf("serve[%d]: %w", i, ErrInvalid)
		}
		seen[key] = true
	}
	return nil
}

func (e Endpoint) valid() bool {
	if !strings.HasPrefix(e.Service, "svc:") || (e.HTTPS > 0) == (e.TCP > 0) || e.HTTPS < 0 || e.TCP < 0 {
		return false
	}
	u, err := url.Parse(e.Target)
	if err != nil || u.Host == "" {
		return false
	}
	if e.TCP > 0 {
		return u.Scheme == "tcp" && u.Port() != ""
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

func resolve(dir, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(dir, path)
}

// Operations supplies effects used by the policy.
type Operations interface {
	ReadFile(path string) ([]byte, error)
	GitState(ctx context.Context, dir string) (GitState, error)
	Compose(ctx context.Context, args, env []string) error
}

type GitState struct {
	Commit string
	Dirty  bool // Includes new deployment inputs.
}

func (g GitState) Label() string {
	if g.Dirty {
		return g.Commit + "-dirty"
	}
	return g.Commit
}

type Inputs struct {
	Config Config
	Dir    string
	Env    []string
	Commit string
}

func (c Config) inputNames() []string {
	return append(slices.Clone(c.RequiredEnv), c.SSH.AddressEnv, c.SSH.UserEnv, c.SSH.KnownHostsEnv)
}

// PrivateValues is deterministic and never changes the supplied environment.
func PrivateValues(c Config, env map[string]string) []string {
	values := make(map[string]bool)
	for _, name := range c.inputNames() {
		values[env[name]] = true
	}
	for value := range strings.SplitSeq(env["NASTOOLS_PRIVATE_STRINGS"], ",") {
		values[strings.TrimSpace(value)] = true
	}
	delete(values, "")
	return slices.Sorted(maps.Keys(values))
}

// MaskCommands escapes workflow-command data, including embedded newlines.
func MaskCommands(values []string, githubActions bool) string {
	if !githubActions {
		return ""
	}
	var out strings.Builder
	escape := strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A")
	for _, value := range values {
		fmt.Fprintf(&out, "::add-mask::%s\n", escape.Replace(value))
	}
	return out.String()
}

func Prepare(ctx context.Context, ops Operations, path string, c Config, env map[string]string) (Inputs, error) {
	if err := c.validate(); err != nil {
		return Inputs{}, err
	}
	for _, name := range c.inputNames() {
		if env[name] == "" {
			return Inputs{}, fmt.Errorf("%s: %w", name, ErrInvalid)
		}
	}
	dir := filepath.Dir(path)
	child, err := c.fileEnvironment(ops, dir, env)
	if err != nil {
		return Inputs{}, err
	}
	state, err := ops.GitState(ctx, dir)
	if err != nil {
		return Inputs{}, fmt.Errorf("git: %w", ErrInvalid)
	}
	return Inputs{Config: c, Dir: dir, Env: childEnvironment(child), Commit: state.Label()}, nil
}

func (c Config) fileEnvironment(ops Operations, dir string, env map[string]string) (map[string]string, error) {
	paths := make(map[string]string)
	maps.Copy(paths, c.Files)
	for _, name := range slices.Sorted(maps.Keys(c.ExpandFiles)) {
		if _, duplicate := c.Files[name]; duplicate {
			return nil, fmt.Errorf("files.%s: duplicate output: %w", name, ErrInvalid)
		}
		paths[name] = c.ExpandFiles[name]
	}
	child := maps.Clone(env)
	for _, name := range slices.Sorted(maps.Keys(paths)) {
		if name == "" || paths[name] == "" {
			return nil, fmt.Errorf("files: %w", ErrInvalid)
		}
		data, err := ops.ReadFile(resolve(dir, paths[name]))
		if err != nil {
			return nil, fmt.Errorf("files.%s: unreadable input: %w", name, ErrInvalid)
		}
		child[name] = string(data)
		if _, expand := c.ExpandFiles[name]; !expand {
			continue
		}
		var missing bool
		child[name] = os.Expand(child[name], func(key string) string {
			missing = missing || env[key] == ""
			return env[key]
		})
		if missing {
			return nil, fmt.Errorf("expand_files.%s: %w", name, ErrInvalid)
		}
	}
	return child, nil
}

func childEnvironment(env map[string]string) []string {
	result := make([]string, 0, len(env))
	for _, name := range slices.Sorted(maps.Keys(env)) {
		result = append(result, name+"="+env[name])
	}
	return result
}

func Check(ctx context.Context, ops Operations, path string, env map[string]string, out io.Writer) error {
	data, readErr := ops.ReadFile(path)
	c, err := parse(data)
	// Mask even on read or parse failure, retaining any partially decoded input names.
	if _, writeErr := io.WriteString(
		out,
		MaskCommands(PrivateValues(c, env), env["GITHUB_ACTIONS"] != ""),
	); writeErr != nil {
		return fmt.Errorf("mask output: %w", ErrInvalid)
	}
	if readErr != nil {
		return fmt.Errorf("nastools.toml: unreadable input: %w", ErrInvalid)
	}
	if err != nil {
		return err
	}
	inputs, err := Prepare(ctx, ops, path, c, env)
	if err != nil {
		return err
	}
	args := []string{"--project-directory", inputs.Dir, "--project-name", inputs.Config.Project}
	for _, path := range inputs.Config.Compose {
		args = append(args, "--file", resolve(inputs.Dir, path))
	}
	args = append(args, "config", "-q")
	if composeErr := ops.Compose(ctx, args, inputs.Env); composeErr != nil {
		return fmt.Errorf("compose config: %w", ErrInvalid)
	}
	return nil
}
