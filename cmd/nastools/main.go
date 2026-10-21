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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := Run(ctx, os.Args[1:], streams{in: os.Stdin, out: os.Stdout, err: os.Stderr}, &operations{cancel: stop}, env)
	stop()
	os.Exit(code)
}

const (
	sshDialTimeout      = 30 * time.Second
	sshHandshakeTimeout = 30 * time.Second
	forwardSessionLimit = 8
)

type operations struct {
	client  *ssh.Client
	dir     string
	cancel  context.CancelFunc
	closers []io.Closer
	workers sync.WaitGroup
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
	agentConn, err := (&net.Dialer{}).DialContext(ctx, "unix", env["SSH_AUTH_SOCK"])
	if err != nil {
		return "", nastools.ErrInvalid
	}
	defer agentConn.Close()
	stopAgent := context.AfterFunc(ctx, func() { _ = agentConn.Close() })
	defer stopAgent()
	raw, err := (&net.Dialer{Timeout: sshDialTimeout}).DialContext(ctx, "tcp", env[config.AddressEnv])
	if err != nil {
		return "", nastools.ErrInvalid
	}
	o.closers = append(o.closers, raw)
	context.AfterFunc(ctx, func() { _ = raw.Close() })
	if raw.SetDeadline(time.Now().Add(sshHandshakeTimeout)) != nil {
		return "", nastools.ErrInvalid
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
	if err != nil {
		return "", nastools.ErrInvalid
	}
	if raw.SetDeadline(time.Time{}) != nil {
		return "", nastools.ErrInvalid
	}
	o.client = ssh.NewClient(conn, channels, requests)
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
	slots := make(chan struct{}, forwardSessionLimit)
	for {
		local, err := listener.Accept()
		if err != nil {
			return
		}
		// Listener shutdown closes every accepted connection before joining workers.
		defer local.Close()
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return
		}
		workers.Go(func() {
			defer func() { <-slots }()
			o.forwardConnection(local)
		})
	}
}

func (o *operations) forwardConnection(local net.Conn) {
	defer local.Close()
	session, sessionErr := o.client.NewSession()
	if sessionErr != nil {
		return
	}
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
	command := "flock -n '" + strings.ReplaceAll(path, "'", "'\\''") + "' cat"
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
	o.closers = append(o.closers, input)
	o.workers.Go(func() {
		if session.Wait() != nil {
			o.cancel()
		}
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
	o.cancel()
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
