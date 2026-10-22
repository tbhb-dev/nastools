// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

// Package nastoolstest supplies an in-memory nastools.Operations for tests.
package nastoolstest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"strings"

	"github.com/tbhb-dev/nastools/internal/nastools"
)

// ErrPrivate is a fake adapter diagnostic with a private value in its text.
var ErrPrivate = errors.New("private.example.invalid")

// errBlocked replaces an open that would block on a named pipe.
var errBlocked = errors.New("open blocks on a named pipe")

// Fake records every call. With Canaries set, Compose and Remote write each
// canary to both writers in odd chunk sizes, and every injected error's text
// carries the canaries.
type Fake struct {
	Files map[string]string
	// Streams maps a path to its mode, for a file whose content never ends,
	// such as /dev/zero. Opening a named pipe fails, where a real one blocks.
	Streams      map[string]fs.FileMode
	State        nastools.GitState
	GitErr       error
	ComposeErr   error
	OpenErr      error
	LockErrors   []error
	PauseErr     error
	FailCommand  string
	RemoteStatus string
	RemoteFail   string
	LockBlock    bool
	OnPause      func()
	Canaries     []string

	Calls        []string
	Opened       []string
	Args         []string
	Env          []string
	Out          bytes.Buffer
	KnownHosts   string
	Commands     [][]string
	Environments [][]string
	LockPaths    []string
	Remotes      [][]string
	Closed       int
}

// fail returns err with the canaries in its text.
func (f *Fake) fail(err error) error {
	if err == nil || len(f.Canaries) == 0 {
		return err
	}
	return fmt.Errorf("%w: %s", err, strings.Join(f.Canaries, " "))
}

// leak writes every canary to each writer, split across 1, 3 and 7 byte writes.
func (f *Fake) leak(writers ...io.Writer) error {
	text := strings.Join(f.Canaries, " ")
	if text == "" {
		return nil
	}
	text = "output " + text + "\n"
	for _, w := range writers {
		for i, n := 0, 0; i < len(text); n++ {
			size := min([]int{1, 3, 7}[n%3], len(text)-i)
			if _, err := io.WriteString(w, text[i:i+size]); err != nil {
				return f.fail(ErrPrivate)
			}
			i += size
		}
	}
	return nil
}

func (f *Fake) ReadFile(path string) ([]byte, error) {
	data, err := nastools.ReadInput(f, path)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	return data, nil
}

// Stat records the start of a read as ReadFile does.
func (f *Fake) Stat(path string) (fs.FileInfo, error) {
	f.Calls = append(f.Calls, "read:"+path)
	if mode, ok := f.Streams[path]; ok {
		return fileInfo{mode: mode}, nil
	}
	if _, ok := f.Files[path]; !ok {
		return nil, f.fail(ErrPrivate)
	}
	return fileInfo{}, nil
}

func (f *Fake) OpenFile(path string) (io.ReadCloser, error) {
	f.Opened = append(f.Opened, path)
	if mode, ok := f.Streams[path]; ok {
		if mode&fs.ModeNamedPipe != 0 {
			return nil, errBlocked
		}
		return io.NopCloser(zeros{}), nil
	}
	data, ok := f.Files[path]
	if !ok {
		return nil, f.fail(ErrPrivate)
	}
	return io.NopCloser(strings.NewReader(data)), nil
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// fileInfo reports only the mode, which ReadInput checks.
type fileInfo struct {
	fs.FileInfo

	mode fs.FileMode
}

func (i fileInfo) Mode() fs.FileMode { return i.mode }

func (f *Fake) GitState(context.Context, string) (nastools.GitState, error) {
	f.Calls = append(f.Calls, "git")
	return f.State, f.fail(f.GitErr)
}

func (f *Fake) Compose(_ context.Context, args, env []string, out, errOut io.Writer) error {
	f.Calls = append(f.Calls, "compose")
	f.Args, f.Env = slices.Clone(args), slices.Clone(env)
	f.Commands = append(f.Commands, slices.Clone(args))
	f.Environments = append(f.Environments, slices.Clone(env))
	if out != nil {
		if _, err := io.WriteString(out, "compose output\n"); err != nil {
			return f.fail(ErrPrivate)
		}
		if _, err := io.WriteString(errOut, "compose diagnostic\n"); err != nil {
			return f.fail(ErrPrivate)
		}
		if err := f.leak(out, errOut); err != nil {
			return err
		}
	}
	if slices.Contains(args, f.FailCommand) {
		return f.fail(ErrPrivate)
	}
	return f.fail(f.ComposeErr)
}

// Remote records one remote command, writing the canned Serve status for a
// status query.
func (f *Fake) Remote(_ context.Context, argv []string, out, errOut io.Writer) error {
	f.Calls = append(f.Calls, "remote")
	f.Remotes = append(f.Remotes, slices.Clone(argv))
	status := slices.Contains(argv, "status")
	if status {
		if _, err := io.WriteString(out, f.RemoteStatus); err != nil {
			return f.fail(ErrPrivate)
		}
	}
	writers := []io.Writer{errOut}
	if !status {
		writers = append(writers, out)
	}
	if err := f.leak(writers...); err != nil {
		return err
	}
	if f.RemoteFail != "" && slices.Contains(argv, f.RemoteFail) {
		return f.fail(ErrPrivate)
	}
	return nil
}

func (f *Fake) Open(_ context.Context, config nastools.SSH, env map[string]string) (string, error) {
	f.Calls = append(f.Calls, "open")
	f.KnownHosts = env[config.KnownHostsEnv]
	return "/example.invalid/socket", f.fail(f.OpenErr)
}

func (f *Fake) Lock(ctx context.Context, path string) error {
	f.Calls = append(f.Calls, "lock")
	f.LockPaths = append(f.LockPaths, path)
	if f.LockBlock {
		<-ctx.Done()
		return f.fail(ErrPrivate)
	}
	if len(f.LockErrors) == 0 {
		return nil
	}
	err := f.LockErrors[0]
	f.LockErrors = f.LockErrors[1:]
	return f.fail(err)
}

func (f *Fake) Pause(context.Context) error {
	f.Calls = append(f.Calls, "pause")
	if f.OnPause != nil {
		f.OnPause()
	}
	return f.fail(f.PauseErr)
}

func (f *Fake) Close() { f.Closed++; f.Calls = append(f.Calls, "close") }
