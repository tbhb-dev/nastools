// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

// Package nastools owns deployment input and command policy.
package nastools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

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
	// The lock directory keys are pointers so that a key set to "" still
	// counts as present.
	LockDir    *string `toml:"lock_dir"`
	LockDirEnv *string `toml:"lock_dir_env"`
}

// lockDir returns the lock directory from whichever key the config sets. It
// returns "" when both keys are present, so validation rejects both as it
// rejects neither.
func (s SSH) lockDir(env map[string]string) string {
	switch {
	case s.LockDirEnv == nil && s.LockDir != nil:
		return *s.LockDir
	case s.LockDir == nil && s.LockDirEnv != nil:
		return env[*s.LockDirEnv]
	}
	return ""
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
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`).MatchString(c.Project) {
		return fmt.Errorf("project: %w", ErrInvalid)
	}
	if !filepath.IsAbs(*c.SSH.LockDir) {
		return fmt.Errorf("ssh.lock_dir: %w", ErrInvalid)
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

const maxPort = 65535

var serviceID = regexp.MustCompile(`^svc:[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func validPort(port string) bool {
	number, err := strconv.Atoi(port)
	return err == nil && number >= 1 && number <= maxPort
}

func (e Endpoint) valid() bool {
	if !serviceID.MatchString(e.Service) ||
		(e.HTTPS > 0) == (e.TCP > 0) || e.HTTPS < 0 || e.TCP < 0 || e.HTTPS > maxPort || e.TCP > maxPort {
		return false
	}
	u, err := url.Parse(e.Target)
	if err != nil || u.Host == "" {
		return false
	}
	if e.TCP > 0 {
		return u.Scheme == "tcp" && validPort(u.Port())
	}
	return (u.Scheme == "http" || u.Scheme == "https") && (u.Port() == "" || validPort(u.Port()))
}

func resolve(dir, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(dir, path)
}

// Files opens deployment inputs. ReadInput reads through it.
type Files interface {
	Stat(path string) (fs.FileInfo, error)
	OpenFile(path string) (io.ReadCloser, error)
}

// Operations supplies effects used by the policy.
type Operations interface {
	Files
	ReadFile(path string) ([]byte, error)
	GitState(ctx context.Context, dir string) (GitState, error)
	Compose(ctx context.Context, args, env []string, out, errOut io.Writer) error
	Remote(ctx context.Context, argv []string, out, errOut io.Writer) error
	Open(ctx context.Context, ssh SSH, env map[string]string) (string, error)
	Lock(ctx context.Context, path string) error
	Pause(ctx context.Context) error
	Close()
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

func Prepare(ctx context.Context, ops Operations, path string, c Config, env map[string]string) (Inputs, error) {
	lockDir := c.SSH.lockDir(env)
	c.SSH.LockDir = &lockDir
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
	// Provenance travels in the Compose environment: a compose file references
	// ${NASTOOLS_COMMIT} in a service label, so Compose owns applying it.
	child["NASTOOLS_COMMIT"] = state.Label()
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

func load(ops Operations, path string) (Config, error) {
	data, err := ops.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("nastools.toml: unreadable input: %w", ErrInvalid)
	}
	return parse(data)
}

func compose(ctx context.Context, ops Operations, inputs Inputs, command []string, out, errOut io.Writer) error {
	const fixedArgs = 4
	args := make([]string, 0, fixedArgs+2*len(inputs.Config.Compose)+len(command))
	args = append(args, "--project-directory", inputs.Dir, "--project-name", inputs.Config.Project)
	for _, path := range inputs.Config.Compose {
		args = append(args, "--file", resolve(inputs.Dir, path))
	}
	if err := ops.Compose(ctx, append(args, command...), inputs.Env, out, errOut); err != nil {
		return fmt.Errorf("compose %s: %w", command[0], ErrInvalid)
	}
	return nil
}

const (
	CheckName = "check"
	Deploy    = "deploy"
	Logs      = "logs"
	Status    = "status"
)

var ErrBusy = errors.New("deploy in progress")

type Command struct {
	Name     string
	Service  string
	Tail     int
	Follow   bool
	LockWait time.Duration
}

func Execute(ctx context.Context, ops Operations, path string, env map[string]string,
	out, errOut io.Writer, command Command,
) error {
	config, err := load(ops, path)
	if err != nil {
		return err
	}
	inputs, err := Prepare(ctx, ops, path, config, env)
	if err != nil {
		return err
	}
	if command.LockWait < 0 || command.Tail < 0 {
		return ErrInvalid
	}
	if command.Name == Deploy || command.Name == CheckName {
		err = compose(ctx, ops, inputs, []string{"config", "-q"}, nil, nil)
	}
	if err != nil || command.Name == CheckName {
		return err
	}

	defer ops.Close() // Also cleans partially opened transports on error.
	socket, err := ops.Open(ctx, inputs.Config.SSH, env)
	if err != nil {
		return fmt.Errorf("SSH transport: %w", ErrInvalid)
	}
	if command.Name == Deploy {
		lock := filepath.Join(*inputs.Config.SSH.LockDir, inputs.Config.Project+".lock")
		if err = acquire(ctx, ops, lock, command.LockWait); err != nil {
			return err
		}
	}
	inputs.Env = append(slices.DeleteFunc(inputs.Env, func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		return slices.Contains(
			[]string{"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_TLS", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"},
			name,
		)
	}), "DOCKER_HOST=unix://"+socket)
	for _, args := range command.arguments(inputs.Config.WaitSeconds) {
		if err = compose(ctx, ops, inputs, args, out, errOut); err != nil {
			return err
		}
	}
	return command.report(ctx, ops, inputs, out, errOut)
}

// report runs after Compose has succeeded. A Serve failure leaves the stack up.
func (command Command) report(ctx context.Context, ops Operations, inputs Inputs, out, errOut io.Writer) error {
	if command.Name == Deploy {
		if err := publish(ctx, ops, inputs.Config.Serve, out, errOut); err != nil {
			return err
		}
	}
	if command.Name != Deploy && command.Name != Status {
		return nil
	}
	if err := compose(ctx, ops, inputs, []string{"ps"}, out, errOut); err != nil {
		return err
	}
	return serveStatus(ctx, ops, inputs.Config.Serve, out, errOut)
}

// publish applies each declared endpoint. Re-running a Serve command with the
// same flags is idempotent, and the flags scope each command to one Service ID.
func publish(ctx context.Context, ops Operations, endpoints []Endpoint, out, errOut io.Writer) error {
	for i, e := range endpoints {
		port := "--https=" + strconv.Itoa(e.HTTPS)
		if e.TCP > 0 {
			port = "--tcp=" + strconv.Itoa(e.TCP)
		}
		argv := []string{"tailscale", "serve", "--service=" + e.Service, port, e.Target}
		if ops.Remote(ctx, argv, out, errOut) != nil {
			return fmt.Errorf("serve[%d]: %w", i, ErrInvalid)
		}
	}
	return nil
}

// serveStatus prints the Serve status of the declared Service IDs only.
func serveStatus(ctx context.Context, ops Operations, endpoints []Endpoint, out, errOut io.Writer) error {
	if len(endpoints) == 0 {
		return nil
	}
	var raw bytes.Buffer
	var status struct {
		Services map[string]json.RawMessage `json:"Services"`
	}
	if ops.Remote(ctx, []string{"tailscale", "serve", "status", "--json"}, &raw, errOut) != nil ||
		json.Unmarshal(raw.Bytes(), &status) != nil {
		return fmt.Errorf("serve status: %w", ErrInvalid)
	}
	scoped := make(map[string]json.RawMessage)
	for _, e := range endpoints {
		if service, ok := status.Services[e.Service]; ok {
			scoped[e.Service] = service
		}
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(scoped); err != nil {
		return fmt.Errorf("serve status: %w", ErrInvalid)
	}
	return nil
}

func (command Command) arguments(wait int) [][]string {
	switch command.Name {
	case Deploy:
		return [][]string{
			{"pull", "--ignore-buildable"},
			{"up", "--wait", "--wait-timeout", strconv.Itoa(wait), "--remove-orphans"},
		}
	case Logs:
		args := []string{"logs", "--tail", strconv.Itoa(command.Tail)}
		if command.Follow {
			args = append(args, "--follow")
		}
		if command.Service != "" {
			args = append(args, "--", command.Service)
		}
		return [][]string{args}
	}
	return nil
}

// busy reports contention, unless the parent context ended the wait.
func busy(parent context.Context) error {
	if parent.Err() != nil {
		return fmt.Errorf("deploy lock: %w", ErrInvalid)
	}
	return fmt.Errorf("%w: %w", ErrBusy, ErrInvalid)
}

func acquire(ctx context.Context, ops Operations, path string, wait time.Duration) error {
	parent := ctx
	if wait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, wait)
		defer cancel()
	}
	for {
		err := ops.Lock(ctx, path)
		if err == nil {
			return nil
		}
		// A wait that expires mid-attempt may fail the transport, yet the lock was still busy.
		expired := parent.Err() == nil && errors.Is(ctx.Err(), context.DeadlineExceeded)
		if !expired && !errors.Is(err, ErrBusy) {
			return fmt.Errorf("deploy lock: %w", ErrInvalid)
		}
		if expired || wait == 0 {
			return fmt.Errorf("%w: %w", ErrBusy, ErrInvalid)
		}
		if ops.Pause(ctx) != nil {
			return busy(parent)
		}
	}
}
