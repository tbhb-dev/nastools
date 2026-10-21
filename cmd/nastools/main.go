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

type operations struct {
	client  *ssh.Client
	dir     string
	cancel  context.CancelFunc
	closers []io.Closer
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
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", env[config.AddressEnv])
	if err != nil {
		return "", nastools.ErrInvalid
	}
	o.closers = append(o.closers, raw)
	context.AfterFunc(ctx, func() { _ = raw.Close() })
	conn, channels, requests, err := ssh.NewClientConn(raw, env[config.AddressEnv], &ssh.ClientConfig{
		User:            env[config.UserEnv],
		Auth:            []ssh.AuthMethod{ssh.PublicKeysCallback(agent.NewClient(agentConn).Signers)},
		HostKeyCallback: verify,
	})
	if err != nil {
		return "", nastools.ErrInvalid
	}
	o.client = ssh.NewClient(conn, channels, requests)
	socket := filepath.Join(o.dir, "d")
	listener, err := (&net.ListenConfig{}).Listen(ctx, "unix", socket)
	if err != nil {
		return "", nastools.ErrInvalid
	}
	o.closers = append(o.closers, listener)
	go o.forward(listener)
	return socket, nil
}

func (o *operations) forward(listener net.Listener) {
	for {
		local, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer local.Close()
			session, sessionErr := o.client.NewSession()
			if sessionErr != nil {
				return
			}
			defer session.Close()
			session.Stdin, session.Stdout = local, local
			if session.Run("sudo -n /usr/local/bin/docker system dial-stdio") != nil {
				return
			}
		}()
	}
}

func (o *operations) Lock(ctx context.Context, path string) error {
	session, err := o.client.NewSession()
	if err != nil {
		return nastools.ErrInvalid
	}
	stop := context.AfterFunc(ctx, func() { _ = session.Close() })
	defer stop()
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
	go func() {
		if session.Wait() != nil {
			o.cancel()
		}
	}()
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
	_ = os.RemoveAll(o.dir)
}

// Policy adds operation names. Adapter diagnostics can contain private values.
func sanitized(err error) error {
	if err != nil {
		return nastools.ErrInvalid
	}
	return nil
}
