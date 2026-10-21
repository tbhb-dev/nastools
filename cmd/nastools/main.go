// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

// Command nastools is the nastools command-line tool.
//
// See Run in run.go.
package main

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	process "os/exec"

	"github.com/tbhb-dev/nastools/internal/nastools"
)

func main() {
	env := make(map[string]string)
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		env[name] = value
	}
	signalled, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// Lock loss cancels this context. Calling stop for it would drop the signal handlers
	// and let a later interrupt end the process before cleanup.
	ctx, abort := context.WithCancel(signalled)
	code := Run(
		ctx,
		os.Args[1:],
		streams{in: os.Stdin, out: os.Stdout, err: os.Stderr},
		&operations{cancel: abort},
		env,
	)
	abort()
	stop()
	os.Exit(code)
}

const (
	sshDialTimeout      = 30 * time.Second
	sshHandshakeTimeout = 30 * time.Second
	forwardSessionLimit = 8
)

type operations struct {
	client  *ssh.Client // First connection, used for the lock session.
	dial    func(context.Context) (*ssh.Client, error)
	dir     string
	cancel  context.CancelFunc
	closers []io.Closer
	workers sync.WaitGroup
	mu      sync.Mutex
	pool    []*pooledClient
}

// pooledClient counts the forwarding sessions busy on one SSH connection.
type pooledClient struct {
	client *ssh.Client
	busy   int
}

func (*operations) ReadFile(path string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	return data, sanitized(err)
}

func (*operations) GitState(ctx context.Context, dir string) (nastools.GitState, error) {
	commit, err := process.CommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return nastools.GitState{}, sanitized(err)
	}
	status, err := process.CommandContext(ctx, "git", "-C", dir, "status", "--porcelain", "--untracked-files=all").
		Output()
	return nastools.GitState{Commit: strings.TrimSpace(string(commit)), Dirty: len(status) != 0}, sanitized(err)
}

func (*operations) Compose(ctx context.Context, args, env []string, out, errOut io.Writer) error {
	cmd := process.CommandContext(ctx, "docker", "compose")
	cmd.Args = append(cmd.Args, args...)
	// All paths in the policy request are relative to the caller or absolute.
	// Changing cmd.Dir here would resolve a relative config directory twice.
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = out, errOut
	return sanitized(cmd.Run())
}

// Remote runs one command on the remote host in its own session on a pooled connection.
func (o *operations) Remote(ctx context.Context, argv []string, out, errOut io.Writer) error {
	session, pooled, err := o.newSession(ctx)
	if err != nil {
		return nastools.ErrInvalid
	}
	defer o.release(pooled)
	defer session.Close()
	stop := context.AfterFunc(ctx, func() { _ = session.Close() })
	defer stop()
	session.Stdout, session.Stderr = out, errOut
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = quote(arg)
	}
	return sanitized(session.Run(strings.Join(quoted, " ")))
}

func quote(arg string) string { return "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'" }

func (o *operations) Open(ctx context.Context, config nastools.SSH, env map[string]string) (string, error) {
	var err error
	o.dir, err = os.MkdirTemp("", "nt-") // Mode 0700. A short name fits macOS Unix sockets.
	if err != nil {
		return "", nastools.ErrInvalid
	}
	path := filepath.Join(o.dir, "known_hosts")
	if err = os.WriteFile(path, []byte(env[config.KnownHostsEnv]), 0o600); err != nil {
		return "", nastools.ErrInvalid
	}
	verify, err := knownhosts.New(path)
	if err != nil {
		return "", nastools.ErrInvalid
	}
	o.dial = func(ctx context.Context) (*ssh.Client, error) { return connect(ctx, config, env, verify) }
	if o.client, err = o.dial(ctx); err != nil {
		return "", nastools.ErrInvalid
	}
	o.pool = []*pooledClient{{client: o.client}}
	socket := filepath.Join(o.dir, "d")
	listener, err := (&net.ListenConfig{}).Listen(ctx, "unix", socket)
	if err != nil {
		return "", nastools.ErrInvalid
	}
	o.closers = append(o.closers, listener)
	context.AfterFunc(ctx, func() { _ = listener.Close() })
	o.workers.Go(func() { o.forward(ctx, listener) })
	return socket, nil
}

// connect opens one authenticated SSH connection. Every connection repeats the
// agent authentication and the strict host-key check.
func connect(
	ctx context.Context,
	config nastools.SSH,
	env map[string]string,
	verify ssh.HostKeyCallback,
) (*ssh.Client, error) {
	agentConn, err := (&net.Dialer{Timeout: sshDialTimeout}).DialContext(ctx, "unix", env["SSH_AUTH_SOCK"])
	if err != nil {
		return nil, nastools.ErrInvalid
	}
	defer agentConn.Close()
	stopAgent := context.AfterFunc(ctx, func() { _ = agentConn.Close() })
	defer stopAgent()
	raw, err := (&net.Dialer{Timeout: sshDialTimeout}).DialContext(ctx, "tcp", env[config.AddressEnv])
	if err != nil {
		return nil, nastools.ErrInvalid
	}
	connected := false
	defer func() {
		if !connected {
			_ = raw.Close()
		}
	}()
	context.AfterFunc(ctx, func() { _ = raw.Close() })
	// The authentication callback reads from the agent, which needs the same deadline.
	deadline := time.Now().Add(sshHandshakeTimeout)
	if errors.Join(raw.SetDeadline(deadline), agentConn.SetDeadline(deadline)) != nil {
		return nil, nastools.ErrInvalid
	}
	conn, channels, requests, err := ssh.NewClientConn(raw, env[config.AddressEnv], &ssh.ClientConfig{
		User:            env[config.UserEnv],
		Auth:            []ssh.AuthMethod{ssh.PublicKeysCallback(agent.NewClient(agentConn).Signers)},
		HostKeyCallback: verify,
		HostKeyAlgorithms: hostKeyAlgorithms(
			verify,
			env[config.AddressEnv],
			raw.RemoteAddr(),
			env[config.KnownHostsEnv],
		),
	})
	if err != nil || raw.SetDeadline(time.Time{}) != nil {
		return nil, nastools.ErrInvalid
	}
	connected = true
	return ssh.NewClient(conn, channels, requests), nil
}

func hostKeyAlgorithms(verify ssh.HostKeyCallback, address string, remote net.Addr, text string) []string {
	var algorithms []string
	for data := []byte(text); len(data) > 0; {
		_, _, key, _, rest, parseErr := ssh.ParseKnownHosts(data)
		if parseErr != nil {
			break
		}
		data = rest
		if verify(address, remote, key) == nil {
			if key.Type() == ssh.KeyAlgoRSA {
				algorithms = append(algorithms, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256)
			}
			algorithms = append(algorithms, key.Type())
		}
	}
	return algorithms
}

func (o *operations) forward(ctx context.Context, listener net.Listener) {
	var workers sync.WaitGroup
	defer workers.Wait()
	for {
		local, err := listener.Accept()
		if err != nil {
			return
		}
		// Listener shutdown closes every accepted connection before joining workers.
		defer local.Close()
		workers.Go(func() { o.forwardConnection(ctx, local) })
	}
}

// acquire never queues: when every connection has forwardSessionLimit sessions
// busy it opens another one. A queued long-lived stream would starve silently.
// Dialing under the lock lets a burst of requests share one new connection.
func (o *operations) acquire(ctx context.Context) (*pooledClient, bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, pooled := range o.pool {
		if pooled.busy < forwardSessionLimit {
			pooled.busy++
			return pooled, false, nil
		}
	}
	client, err := o.dial(ctx)
	if err != nil {
		return nil, false, err
	}
	pooled := &pooledClient{client: client, busy: 1}
	o.pool = append(o.pool, pooled)
	return pooled, true, nil
}

// newSession opens a session on a pooled connection. A pooled connection that
// fails to open one is dead (an idle connection that a gateway dropped), so it
// leaves the pool and another one serves the request. A fresh connection that
// fails ends the attempt.
func (o *operations) newSession(ctx context.Context) (*ssh.Session, *pooledClient, error) {
	for {
		pooled, dialed, err := o.acquire(ctx)
		if err != nil {
			return nil, nil, err
		}
		session, err := pooled.client.NewSession()
		if err == nil {
			return session, pooled, nil
		}
		o.mu.Lock()
		o.pool = slices.DeleteFunc(o.pool, func(p *pooledClient) bool { return p == pooled })
		o.mu.Unlock()
		_ = pooled.client.Close()
		if dialed {
			return nil, nil, sanitized(err)
		}
	}
}

func (o *operations) release(pooled *pooledClient) {
	o.mu.Lock()
	defer o.mu.Unlock()
	pooled.busy--
}

func (o *operations) forwardConnection(ctx context.Context, local net.Conn) {
	defer local.Close()
	session, pooled, err := o.newSession(ctx)
	if err != nil {
		return
	}
	defer o.release(pooled)
	defer session.Close()
	input, inputErr := session.StdinPipe()
	output, outputErr := session.StdoutPipe()
	startErr := session.Start("sudo -n /usr/local/bin/docker system dial-stdio")
	if errors.Join(inputErr, outputErr, startErr) != nil {
		return
	}
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		defer input.Close()
		if _, copyErr := io.Copy(input, local); copyErr != nil {
			return
		}
	}()
	_, copyErr := io.Copy(local, output)
	_ = local.Close()   // Remote end of stream must also unblock the local reader.
	_ = session.Close() // Unblock a copier waiting for the remote window.
	<-copied
	if errors.Join(copyErr, session.Wait()) != nil {
		return
	}
}

func (o *operations) Lock(ctx context.Context, path string) error {
	stop := context.AfterFunc(ctx, func() { _ = o.client.Close() })
	defer stop()
	session, err := o.client.NewSession()
	if err != nil {
		return nastools.ErrInvalid
	}
	input, inputErr := session.StdinPipe()
	output, outputErr := session.StdoutPipe()
	command := "flock -n " + quote(path) + " cat"
	if err = errors.Join(inputErr, outputErr, session.Start(command)); err != nil {
		_ = session.Close()
		return nastools.ErrInvalid
	}
	// Only cat can echo this probe: success proves acquisition without a timing guess.
	_, writeErr := io.WriteString(input, "locked\n")
	probe := make([]byte, len("locked\n"))
	_, err = io.ReadFull(output, probe)
	if err != nil || writeErr != nil || string(probe) != "locked\n" {
		_ = input.Close()
		waitErr := session.Wait()
		_ = session.Close()
		var exit *ssh.ExitError
		if errors.As(waitErr, &exit) && exit.ExitStatus() == 1 {
			return nastools.ErrBusy
		}
		return nastools.ErrInvalid
	}
	// Close ends the session by closing its input first. Any exit before then,
	// clean or not, means the remote lock holder is gone.
	o.closers = append(o.closers, session, input)
	o.workers.Go(func() {
		exitErr := session.Wait()
		_ = exitErr
		o.cancel()
	})
	return nil
}

func (*operations) Pause(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return nastools.ErrInvalid
	case <-time.After(time.Second):
		return nil
	}
}

func (o *operations) Close() {
	for _, closer := range slices.Backward(o.closers) {
		_ = closer.Close()
	}
	o.cancel() // Before the pool lock, so a dial in progress ends and later dials fail.
	o.mu.Lock()
	for _, pooled := range o.pool {
		_ = pooled.client.Close()
	}
	o.mu.Unlock()
	o.workers.Wait()
	_ = os.RemoveAll(o.dir)
}

// Policy adds operation names. Adapter diagnostics can contain private values.
func sanitized(err error) error {
	if err != nil {
		return nastools.ErrInvalid
	}
	return nil
}
