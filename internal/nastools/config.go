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
	LockDir       string `toml:"lock_dir"`
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
	if !filepath.IsAbs(c.SSH.LockDir) {
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

// Operations supplies effects used by the policy.
type Operations interface {
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
	// Actions reports a non-empty GITHUB_ACTIONS value.
	Actions bool
	// Referenced holds the non-empty values that expanded file templates read.
	Referenced []string
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

func maskOutput(out io.Writer, values []string, env map[string]string) error {
	if _, err := io.WriteString(out, MaskCommands(values, env["GITHUB_ACTIONS"] != "")); err != nil {
		return fmt.Errorf("mask output: %w", ErrInvalid)
	}
	return nil
}

func Prepare(ctx context.Context, ops Operations, path string, c Config, env map[string]string) (Inputs, error) {
	dir := filepath.Dir(path)
	referenced := make(map[string]bool)
	// Templates are read first so the caller can mask their variables on every failure below.
	child, fileErr := c.fileEnvironment(ops, dir, env, referenced)
	delete(referenced, "")
	inputs := Inputs{Actions: env["GITHUB_ACTIONS"] != "", Referenced: slices.Sorted(maps.Keys(referenced))}
	if err := c.validate(); err != nil {
		return inputs, err
	}
	for _, name := range c.inputNames() {
		if env[name] == "" {
			return inputs, fmt.Errorf("%s: %w", name, ErrInvalid)
		}
	}
	if fileErr != nil {
		return inputs, fileErr
	}
	state, err := ops.GitState(ctx, dir)
	if err != nil {
		return inputs, fmt.Errorf("git: %w", ErrInvalid)
	}
	// Provenance travels in the Compose environment: a compose file references
	// ${NASTOOLS_COMMIT} in a service label, so Compose owns applying it.
	child["NASTOOLS_COMMIT"] = state.Label()
	inputs.Config, inputs.Dir, inputs.Env, inputs.Commit = c, dir, childEnvironment(child), state.Label()
	return inputs, nil
}

// fileEnvironment records in referenced every value an expanded file reads.
func (c Config) fileEnvironment(
	ops Operations, dir string, env map[string]string, referenced map[string]bool,
) (map[string]string, error) {
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
			referenced[env[key]] = true
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

func Load(ops Operations, path string, env map[string]string, out io.Writer) (Config, error) {
	data, readErr := ops.ReadFile(path)
	c, err := parse(data)
	// Mask even on read or parse failure, retaining any partially decoded input names.
	if maskErr := maskOutput(out, PrivateValues(c, env), env); maskErr != nil {
		return c, maskErr
	}
	if readErr != nil {
		return c, fmt.Errorf("nastools.toml: unreadable input: %w", ErrInvalid)
	}
	return c, err
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

// prepared loads and prepares the inputs, masking every private value before any output.
func prepared(ctx context.Context, ops Operations, path string, env map[string]string, out io.Writer) (Inputs, error) {
	config, err := Load(ops, path, env, out)
	if err != nil {
		return Inputs{}, err
	}
	inputs, err := Prepare(ctx, ops, path, config, env)
	// Templates can read variables beyond the declared inputs, even on a failing path.
	private := PrivateValues(config, env)
	if maskErr := maskOutput(out, slices.DeleteFunc(slices.Clone(inputs.Referenced), func(value string) bool {
		return slices.Contains(private, value)
	}), env); maskErr != nil {
		return Inputs{}, maskErr
	}
	return inputs, err
}

func Execute(ctx context.Context, ops Operations, path string, env map[string]string,
	out, errOut io.Writer, command Command,
) error {
	inputs, err := prepared(ctx, ops, path, env, out)
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
		lock := filepath.Join(inputs.Config.SSH.LockDir, inputs.Config.Project+".lock")
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
		if err := publish(ctx, ops, inputs, out, errOut); err != nil {
			return err
		}
	}
	if command.Name != Deploy && command.Name != Status {
		return nil
	}
	if err := compose(ctx, ops, inputs, []string{"ps"}, out, errOut); err != nil {
		return err
	}
	return serveStatus(ctx, ops, inputs, out, errOut)
}

// publish applies each declared endpoint. Re-running a Serve command with the
// same flags is idempotent, and the flags scope each command to one Service ID.
func publish(ctx context.Context, ops Operations, inputs Inputs, out, errOut io.Writer) error {
	for i, e := range inputs.Config.Serve {
		port := "--https=" + strconv.Itoa(e.HTTPS)
		if e.TCP > 0 {
			port = "--tcp=" + strconv.Itoa(e.TCP)
		}
		var stdout, stderr bytes.Buffer
		argv := []string{"tailscale", "serve", "--service=" + e.Service, port, e.Target}
		runErr := ops.Remote(ctx, argv, &stdout, &stderr)
		if err := relay(inputs.Actions, out, errOut, stdout.Bytes(), stderr.Bytes()); err != nil {
			return err
		}
		if runErr != nil {
			return fmt.Errorf("serve[%d]: %w", i, ErrInvalid)
		}
	}
	return nil
}

// serveStatus prints the Serve status of the declared Service IDs only.
func serveStatus(ctx context.Context, ops Operations, inputs Inputs, out, errOut io.Writer) error {
	if len(inputs.Config.Serve) == 0 {
		return nil
	}
	var raw, stderr bytes.Buffer
	var status struct {
		Services map[string]json.RawMessage `json:"Services"`
	}
	if ops.Remote(ctx, []string{"tailscale", "serve", "status", "--json"}, &raw, &stderr) != nil ||
		json.Unmarshal(raw.Bytes(), &status) != nil {
		return fmt.Errorf("serve status: %w", ErrInvalid)
	}
	scoped := make(map[string]json.RawMessage)
	for _, e := range inputs.Config.Serve {
		if service, ok := status.Services[e.Service]; ok {
			scoped[e.Service] = service
		}
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(scoped); err != nil {
		return fmt.Errorf("serve status: %w", ErrInvalid)
	}
	return relay(inputs.Actions, out, errOut, encoded.Bytes(), stderr.Bytes())
}

var serveHostname = regexp.MustCompile(`[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.ts\.net`)

// relay prints held Serve output. Holding the text back masks every *.ts.net
// hostname before a byte prints, so nothing needs suppressing under GitHub Actions.
func relay(actions bool, out, errOut io.Writer, stdout, stderr []byte) error {
	var names []string
	for _, text := range [][]byte{stdout, stderr} {
		for _, name := range serveHostname.FindAll(text, -1) {
			names = append(names, string(name))
		}
	}
	slices.Sort(names)
	if _, err := io.WriteString(out, MaskCommands(slices.Compact(names), actions)); err != nil {
		return fmt.Errorf("serve output: %w", ErrInvalid)
	}
	_, outErr := out.Write(stdout)
	_, errErr := errOut.Write(stderr)
	if errors.Join(outErr, errErr) != nil {
		return fmt.Errorf("serve output: %w", ErrInvalid)
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
