// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

//go:build e2e

package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	process "os/exec"

	"github.com/tbhb-dev/nastools/internal/nastools"
)

const (
	fixtureUser  = "nastools"
	serveLog     = "/var/lib/nastools/serve.log"
	lockLog      = "/var/lib/nastools/flock.log"
	fixtureLocks = "/var/lib/nastools/locks"
	commitLabel  = "example.invalid/commit"
	// sampleName is the name in testdata/fixture/project/nastools.toml that
	// each scenario replaces with its own.
	sampleName = "nastools-e2e"
	// holdDelay is how long, in seconds, the sample services report starting.
	// A deploy waits that long for them and keeps its lock meanwhile.
	holdDelay = "15"
	// lockBusy is the exit status the lock probe gives when the lock is held.
	lockBusy     = 75
	commandLimit = 3 * time.Minute
	stateLimit   = time.Minute
	pollInterval = 100 * time.Millisecond
)

// sshFixture is the SSH engine fixture that every scenario deploys through.
type sshFixture struct {
	name       string // Every engine resource the test creates starts with it.
	dir        string
	binary     string
	container  string
	sample     string // The image the sample services run.
	agent      string // The socket of the private agent holding the client key.
	address    string
	knownHosts string
}

// TestE2E builds nastools and the SSH engine fixture, then runs each scenario
// against the fixture with its own project, lock, and Service IDs.
func TestE2E(t *testing.T) {
	t.Parallel()
	f := startFixture(t)
	t.Run("deploy", func(t *testing.T) {
		t.Parallel()
		testDeploy(t, f)
	})
	t.Run("contention", func(t *testing.T) {
		t.Parallel()
		testLockContention(t, f)
	})
	t.Run("release", func(t *testing.T) {
		t.Parallel()
		testLockRelease(t, f)
	})
	t.Run("mismatch", func(t *testing.T) {
		t.Parallel()
		testHostKeyMismatch(t, f)
	})
}

// testDeploy runs every remote command against the sample project and checks
// the engine and the Serve log afterwards.
func testDeploy(t *testing.T, f *sshFixture) {
	t.Helper()
	s := newScenario(t, f, "deploy")
	for _, args := range [][]string{{"check"}, {"deploy"}} {
		r := s.run(t, options{}, args...)
		require.Equal(t, exitOK, r.code, "%s", r)
	}
	s.requireHealthy(t)
	assert.Equal(t, []string{
		"svc:" + s.project + "-web\thttps=443\thttp://127.0.0.1:8080",
		"svc:" + s.project + "-worker\ttcp=5432\ttcp://127.0.0.1:5432",
	}, s.serveEntries(t))
	status := s.run(t, options{}, "status")
	require.Equal(t, exitOK, status.code, "%s", status)
	assert.Contains(t, status.stdout, `"svc:`+s.project+`-web"`)
	assert.Contains(t, status.stdout, `"svc:`+s.project+`-worker"`)
	logs := s.run(t, options{}, "logs", "--tail", "5")
	require.Equal(t, exitOK, logs.code, "%s", logs)
	assert.Contains(t, logs.stdout, "ready")
}

// testLockContention holds the lock with one deploy and starts the others
// only once the lock probe reports it busy. The fixture's lock log then shows
// the waiting deploy refused while the first held the lock, and taking it
// only after the first released it.
func testLockContention(t *testing.T, f *sshFixture) {
	t.Helper()
	s := newScenario(t, f, "contention")
	// The probe takes a free lock for a moment. The first deploy waits for
	// the lock in case the two meet.
	first := s.start(t, options{delay: holdDelay}, "deploy", "--lock-wait", "1m")
	waitFor(t, "the first deploy to hold the lock", func() bool { return s.locked(t) })
	busy := s.run(t, options{delay: holdDelay}, "deploy")
	assert.Equal(t, exitError, busy.code, "%s", busy)
	assert.Contains(t, busy.stderr, nastools.ErrBusy.Error())
	require.True(t, s.locked(t), "the first deploy finished before the waiting deploy started")
	// The first deploy holds the lock and the refused deploy has exited, so
	// every attempt logged from here on comes from the waiting deploy. A refusal
	// is logged after the fact, so one may also follow the release.
	before := len(s.lockLog(t))
	waiting := s.start(t, options{delay: holdDelay}, "deploy", "--lock-wait", "2m")
	held, waited := first.wait(t), waiting.wait(t)
	require.Equal(t, exitOK, held.code, "%s", held)
	assert.Regexp(t, `^(busy )+released (busy )*acquired released$`, strings.Join(s.lockLog(t)[before:], " "),
		"the waiting deploy didn't find the lock busy and take it after the first deploy released it")
	require.Equal(t, exitOK, waited.code, "%s", waited)
}

// testLockRelease kills a deploy that holds the lock. The lock then comes
// free without any cleanup, and the next deploy gets it without waiting.
func testLockRelease(t *testing.T, f *sshFixture) {
	t.Helper()
	s := newScenario(t, f, "release")
	first := s.start(t, options{delay: holdDelay}, "deploy", "--lock-wait", "1m")
	waitFor(t, "the first deploy to hold the lock", func() bool { return s.locked(t) })
	first.kill(t)
	killed := first.wait(t)
	require.NotEqual(t, exitOK, killed.code, "%s", killed)
	waitFor(t, "the killed deploy's lock to come free", func() bool { return !s.locked(t) })
	next := s.run(t, options{}, "deploy")
	require.Equal(t, exitOK, next.code, "%s", next)
	s.requireHealthy(t)
}

// testHostKeyMismatch connects with a known-hosts entry for another key. The
// SSH server logs the user of every authentication attempt, and the user
// name of this run never appears in its log, which shows the run stopped
// before authentication. A second run with the right key and an unknown
// user follows, and its log line proves the log already covers the first.
func testHostKeyMismatch(t *testing.T, f *sshFixture) {
	t.Helper()
	s := newScenario(t, f, "mismatch")
	mismatch, control := "mismatch-"+randomSuffix(), "control-"+randomSuffix()
	wrong := knownhosts.Line([]string{s.address}, publicKey(t, newKey(t)))
	r := s.run(t, options{user: mismatch, knownHosts: wrong}, "status")
	assert.Equal(t, exitError, r.code, "%s", r)
	assert.Contains(t, r.stderr, "SSH transport")
	r = s.run(t, options{user: control}, "status")
	assert.Equal(t, exitError, r.code, "%s", r)
	var log string
	waitFor(t, "sshd to log the control user", func() bool {
		log = f.log(t)
		return strings.Contains(log, "Invalid user "+control)
	})
	assert.NotContains(t, log, mismatch)
}

func startFixture(t *testing.T) *sshFixture {
	t.Helper()
	dockerContext := requireLocalEngine(t)
	version, err := output(t.Context(), "docker", "version", "--format", "{{.Server.Version}}")
	if err != nil {
		noEngine(t, err)
	}
	compose := mustOutput(t, "docker", "compose", "version", "--short")
	t.Logf("Docker Engine %s, Compose %s", strings.TrimSpace(version), strings.TrimSpace(compose))
	build := requireLocalBuilder(t, dockerContext)

	name := cmp.Or(os.Getenv("NASTOOLS_E2E_PREFIX"), "nastools-e2e") + "-" + randomSuffix()
	f := &sshFixture{name: name, dir: t.TempDir(), container: name + "-fixture", sample: name + "-sample"}
	f.binary = filepath.Join(f.dir, "nastools")
	mustOutput(t, "go", "build", "-o", f.binary, ".")
	fixtureDir := filepath.Join("..", "..", "testdata", "fixture")
	build(f.container, fixtureDir)
	removeLater(t, "image", "rm", f.container)
	build(f.sample, filepath.Join(fixtureDir, "sample"))
	removeLater(t, "image", "rm", f.sample)

	clientKey, hostKey := newKey(t), newKey(t)
	f.agent = serveAgent(t, f.dir, clientKey)
	mustOutput(t, "docker", "create", "--name", f.container, "--publish", "127.0.0.1::22",
		"--volume", "/var/run/docker.sock:/var/run/docker.sock", f.container)
	removeLater(t, "rm", "--force", f.container)
	hostKeyFile, clientKeyFile := filepath.Join(f.dir, "host_key"), filepath.Join(f.dir, "client_key.pub")
	require.NoError(t, os.WriteFile(hostKeyFile, pem.EncodeToMemory(privateKey(t, hostKey)), 0o600))
	require.NoError(t, os.WriteFile(clientKeyFile, ssh.MarshalAuthorizedKey(publicKey(t, clientKey)), 0o600))
	mustOutput(t, "docker", "cp", hostKeyFile, f.container+":/etc/ssh/ssh_host_ed25519_key")
	mustOutput(t, "docker", "cp", clientKeyFile, f.container+":/home/nastools/.ssh/authorized_keys")
	mustOutput(t, "docker", "start", f.container)
	port := mustOutput(t, "docker", "port", f.container, "22/tcp")
	f.address, _, _ = strings.Cut(strings.TrimSpace(port), "\n")
	f.knownHosts = knownhosts.Line([]string{f.address}, publicKey(t, hostKey))
	waitFor(t, "sshd to answer", func() bool { return sshAnswers(t.Context(), f.address) })
	return f
}

// requireLocalEngine ends the test unless the Docker command-line tool
// reaches an engine on this machine through a Unix socket, and returns the
// name of the active context. The fixture gets full control of the engine it
// runs on, so it never runs on a remote one. It reads only the environment
// and the Docker configuration, and doesn't contact an engine.
func requireLocalEngine(t *testing.T) string {
	t.Helper()
	if host := os.Getenv("DOCKER_HOST"); host != "" && !strings.HasPrefix(host, "unix://") {
		notLocal(t, "DOCKER_HOST isn't a unix:// socket")
	}
	active, err := output(t.Context(), "docker", "context", "inspect", "--format",
		"{{.Name}} {{.Endpoints.docker.Host}}")
	if err != nil {
		noEngine(t, err)
	}
	name, endpoint, _ := strings.Cut(strings.TrimSpace(active), " ")
	if !strings.HasPrefix(endpoint, "unix://") {
		notLocal(t, "the active Docker context isn't a unix:// socket")
	}
	return name
}

// requireLocalBuilder ends the test unless the builder named after the active
// Docker context uses the docker driver, which builds on that context's
// engine. It returns a function that builds an image there. The builds drop
// BUILDX_BUILDER and select that builder explicitly, so no other builder
// selection reaches them.
func requireLocalBuilder(t *testing.T, dockerContext string) func(tag, dir string) {
	t.Helper()
	env := slices.DeleteFunc(os.Environ(), func(entry string) bool {
		return strings.HasPrefix(entry, "BUILDX_BUILDER=")
	})
	out, err := combined(t.Context(), env, "docker", "buildx", "inspect", dockerContext)
	if err != nil {
		notLocal(t, fmt.Sprintf("Docker Buildx can't inspect the builder %s: %v", dockerContext, err))
	}
	var name, driver string
	var endpoints []string
	for line := range strings.Lines(out) {
		key, value, _ := strings.Cut(line, ":")
		value = strings.TrimSpace(value)
		switch key {
		case "Name":
			name = cmp.Or(name, value)
		case "Driver":
			driver = value
		case "Endpoint":
			endpoints = append(endpoints, value)
		}
	}
	if name != dockerContext || driver != "docker" || !slices.Equal(endpoints, []string{dockerContext}) {
		notLocal(t, fmt.Sprintf("the builder %s doesn't use the docker driver on the active context", dockerContext))
	}
	return func(tag, dir string) {
		t.Helper()
		_, buildErr := combined(t.Context(), env, "docker", "buildx", "build", "--builder", dockerContext, "--load",
			"--tag", tag, dir)
		require.NoError(t, buildErr)
	}
}

// mustRun reports whether a test run has to reach the scenarios instead of
// skipping them. A CI run on Linux does, so the required check can't pass
// without exercising anything.
func mustRun(goos string, getenv func(string) string) bool {
	return goos == "linux" && (getenv("CI") != "" || getenv("GITHUB_ACTIONS") != "")
}

// notLocal ends a test whose engine or builder isn't local. It fails a CI
// run on Linux and skips anywhere else.
func notLocal(t *testing.T, reason string) {
	t.Helper()
	if mustRun(runtime.GOOS, os.Getenv) {
		t.Fatalf("e2e: %s, and the fixture runs only on a local engine", reason)
	}
	t.Skipf("e2e: skipped: %s, and the fixture runs only on a local engine", reason)
}

// noEngine ends a test when no Docker engine answers. It skips on macOS,
// because hosted macOS runners have none.
func noEngine(t *testing.T, err error) {
	t.Helper()
	if runtime.GOOS == "darwin" {
		t.Skip("e2e: skipped: no Docker engine answers here, and hosted macOS runners have none")
	}
	t.Fatalf("e2e: the SSH engine fixture needs a Docker engine: %v", err)
}

// log returns everything the SSH server has logged.
func (f *sshFixture) log(t *testing.T) string {
	t.Helper()
	out, err := combined(t.Context(), nil, "docker", "logs", f.container)
	require.NoError(t, err)
	return out
}

// scenario is one copy of the sample project in its own Git repository,
// under a project name of its own.
type scenario struct {
	*sshFixture

	project string
	dir     string
	tmp     string // TMPDIR for nastools, short enough for its socket path.
	commit  string
}

func newScenario(t *testing.T, f *sshFixture, name string) *scenario {
	t.Helper()
	dir := t.TempDir()
	s := &scenario{sshFixture: f, project: f.name + "-" + name, dir: dir, tmp: filepath.Join(f.dir, name)}
	require.NoError(t, os.Mkdir(s.tmp, 0o700))
	removeLater(t, "compose", "--project-name", s.project, "down", "--volumes", "--remove-orphans", "--timeout", "1")
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()
	for _, file := range []string{"compose.yaml", "nastools.toml"} {
		data, readErr := os.ReadFile(filepath.Join("..", "..", "testdata", "fixture", "project", file))
		require.NoError(t, readErr)
		data = bytes.ReplaceAll(data, []byte(sampleName), []byte(s.project))
		require.NoError(t, root.WriteFile(file, data, 0o600))
	}
	// The repository ignores the user's Git configuration, signing included.
	env := append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_AUTHOR_NAME=nastools", "GIT_AUTHOR_EMAIL=e2e@example.invalid",
		"GIT_COMMITTER_NAME=nastools", "GIT_COMMITTER_EMAIL=e2e@example.invalid")
	git := func(args ...string) string {
		out, gitErr := combined(t.Context(), env, "git", append([]string{"-C", dir}, args...)...)
		require.NoError(t, gitErr)
		return strings.TrimSpace(out)
	}
	git("init", "--quiet", "--initial-branch=main")
	git("add", "compose.yaml", "nastools.toml")
	git("commit", "--quiet", "--message=sample")
	s.commit = git("rev-parse", "HEAD")
	return s
}

// options vary one nastools run. Empty fields take the fixture's values.
type options struct {
	delay      string
	user       string
	knownHosts string
}

// environ hands nastools the fixture's inputs. It leaves GITHUB_ACTIONS out,
// so a CI run sees the same unmasked output as a local one.
func (s *scenario) environ(o options) []string {
	env := slices.DeleteFunc(os.Environ(), func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		return slices.Contains([]string{"SSH_AUTH_SOCK", "TMPDIR", "READY_DELAY", "GITHUB_ACTIONS"}, name) ||
			strings.HasPrefix(name, "NASTOOLS_E2E_")
	})
	return append(env,
		"SSH_AUTH_SOCK="+s.agent,
		"TMPDIR="+s.tmp,
		"READY_DELAY="+o.delay,
		"NASTOOLS_E2E_SAMPLE_IMAGE="+s.sample,
		"NASTOOLS_E2E_ADDRESS="+s.address,
		"NASTOOLS_E2E_USER="+cmp.Or(o.user, fixtureUser),
		"NASTOOLS_E2E_KNOWN_HOSTS="+cmp.Or(o.knownHosts, s.knownHosts),
	)
}

// started is a nastools run in its own process group, so that killing it
// also kills the Compose process it started.
type started struct {
	cmd    *process.Cmd
	stdout bytes.Buffer
	stderr bytes.Buffer
	done   chan struct{}
}

func (s *scenario) start(t *testing.T, o options, args ...string) *started {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), commandLimit)
	args = append(args, "--config", filepath.Join(s.dir, "nastools.toml"))
	p := &started{cmd: process.CommandContext(ctx, s.binary, args...), done: make(chan struct{})}
	p.cmd.Env = s.environ(o)
	p.cmd.Stdout, p.cmd.Stderr = &p.stdout, &p.stderr
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	p.cmd.Cancel = func() error { return syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL) }
	p.cmd.WaitDelay = 10 * time.Second
	require.NoError(t, p.cmd.Start())
	go func() {
		defer cancel()
		defer close(p.done)
		// The exit status is read from ProcessState after done closes.
		waitErr := p.cmd.Wait()
		_ = waitErr
	}()
	// A failed assertion can skip the wait for the run. Cancel kills its
	// process group if it's still running, and the cleanup waits for it to
	// finish before the scenario's earlier cleanups remove engine resources.
	t.Cleanup(func() {
		cancel()
		<-p.done
	})
	return p
}

func (p *started) kill(t *testing.T) {
	t.Helper()
	require.NoError(t, syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL))
}

// result is a finished nastools run.
type result struct {
	code   int
	stdout string
	stderr string
}

func (r result) String() string {
	return fmt.Sprintf("exit status %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
}

func (p *started) wait(t *testing.T) result {
	t.Helper()
	<-p.done
	return result{
		code:   p.cmd.ProcessState.ExitCode(),
		stdout: p.stdout.String(),
		stderr: p.stderr.String(),
	}
}

func (s *scenario) run(t *testing.T, o options, args ...string) result {
	t.Helper()
	return s.start(t, o, args...).wait(t)
}

// requireHealthy checks the health status and the commit label of both
// sample services.
func (s *scenario) requireHealthy(t *testing.T) {
	t.Helper()
	names := strings.Fields(mustOutput(t, "docker", "ps", "--all", "--format", "{{.Names}}",
		"--filter", "label=com.docker.compose.project="+s.project))
	require.Len(t, names, 2)
	format := `{{.State.Health.Status}} {{index .Config.Labels "` + commitLabel + `"}}`
	states := strings.Split(
		strings.TrimSpace(mustOutput(t, "docker", append([]string{"inspect", "--format", format}, names...)...)),
		"\n",
	)
	assert.Equal(t, []string{"healthy " + s.commit, "healthy " + s.commit}, states)
}

// serveEntries returns the Serve log lines for the scenario's Service IDs.
func (s *scenario) serveEntries(t *testing.T) []string {
	t.Helper()
	lines := strings.Split(mustOutput(t, "docker", "exec", s.container, "cat", serveLog), "\n")
	return slices.DeleteFunc(lines, func(line string) bool { return !strings.HasPrefix(line, "svc:"+s.project+"-") })
}

// lock returns the path of the scenario's lock inside the fixture.
func (s *scenario) lock() string {
	return path.Join(fixtureLocks, s.project+".lock")
}

// lockLog returns, in order, the events that the fixture's flock wrapper
// logged for the scenario's lock: busy, acquired, or released.
func (s *scenario) lockLog(t *testing.T) []string {
	t.Helper()
	var events []string
	for line := range strings.Lines(mustOutput(t, "docker", "exec", s.container, "cat", lockLog)) {
		event, lock, _ := strings.Cut(strings.TrimSpace(line), " ")
		if lock == s.lock() {
			events = append(events, event)
		}
	}
	return events
}

// locked reports whether a `flock -n` probe inside the fixture finds the
// scenario's lock held.
func (s *scenario) locked(t *testing.T) bool {
	t.Helper()
	_, err := output(t.Context(), "docker", "exec", "--user", fixtureUser, s.container,
		"flock", "-n", "-E", strconv.Itoa(lockBusy), s.lock(), "true")
	var exit *process.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == lockBusy {
		return true
	}
	require.NoError(t, err)
	return false
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(stateLimit)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("e2e: timed out waiting for %s", what)
		}
		time.Sleep(pollInterval)
	}
}

// sshAnswers reports whether an SSH server sends its banner at address. A
// published port can accept connections before the server inside listens.
func sshAnswers(ctx context.Context, address string) bool {
	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return false
	}
	defer conn.Close()
	if conn.SetReadDeadline(time.Now().Add(time.Second)) != nil {
		return false
	}
	banner, err := bufio.NewReader(conn).ReadString('\n')
	return err == nil && strings.HasPrefix(banner, "SSH-2.0-")
}

// serveAgent serves a private agent holding key on a socket in dir.
func serveAgent(t *testing.T, dir string, key ed25519.PrivateKey) string {
	t.Helper()
	keyring := agent.NewKeyring()
	require.NoError(t, keyring.Add(agent.AddedKey{PrivateKey: key}))
	socket := filepath.Join(dir, "agent")
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer conn.Close()
				if agent.ServeAgent(keyring, conn) != nil {
					return
				}
			}()
		}
	}()
	return socket
}

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	return key
}

func publicKey(t *testing.T, key ed25519.PrivateKey) ssh.PublicKey {
	t.Helper()
	public, err := ssh.NewPublicKey(key.Public())
	require.NoError(t, err)
	return public
}

func privateKey(t *testing.T, key ed25519.PrivateKey) *pem.Block {
	t.Helper()
	block, err := ssh.MarshalPrivateKey(key, "")
	require.NoError(t, err)
	return block
}

func randomSuffix() string {
	return strings.ToLower(rand.Text()[:6])
}

// removeLater removes an engine resource that the test created once the
// test and everything it started finish.
func removeLater(t *testing.T, args ...string) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := output(context.WithoutCancel(t.Context()), "docker", args...); err != nil {
			t.Errorf("e2e: cleanup: %v", err)
		}
	})
}

// mustOutput runs a command that has to succeed and returns its standard output.
func mustOutput(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := output(t.Context(), name, args...)
	require.NoError(t, err)
	return out
}

// combined returns a command's standard output and standard error together.
// A nil environment inherits the test's.
func combined(ctx context.Context, env []string, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandLimit)
	defer cancel()
	cmd := process.CommandContext(ctx, name, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

func output(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandLimit)
	defer cancel()
	cmd := process.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, stderr.Bytes())
	}
	return string(out), nil
}
