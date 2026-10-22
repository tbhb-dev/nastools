// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

// Package redact filters every byte of process output under GitHub Actions.
// It masks private values for the runner and replaces them in each line.
package redact

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"slices"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

// TailnetName matches a MagicDNS host name.
var TailnetName = regexp.MustCompile(`[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)*\.ts\.net`)

const (
	// A derived form shorter than this would redact common text everywhere.
	minDerived = 8
	// A line without a newline is cut once it grows past this many bytes.
	maxLine = 64 << 10
	// The tail kept after a cut has room for a whole DNS name.
	maxHost     = 253
	placeholder = "***"
)

// Gate serializes and redacts both process streams.
type Gate struct {
	mu       sync.Mutex
	active   bool
	stdout   *stream
	stderr   *stream
	secrets  []string // Longest first.
	masked   map[string]bool
	patterns []*regexp.Regexp
	err      error // Sticky mask-write failure.
}

type stream struct {
	gate    *Gate
	w       io.Writer
	pending []byte
	open    bool // The last byte written wasn't a newline.
}

// New returns a gate over the process streams. An inactive gate passes the
// writers through unchanged.
func New(stdout, stderr io.Writer, active bool, patterns ...*regexp.Regexp) *Gate {
	g := &Gate{active: active, masked: make(map[string]bool), patterns: patterns}
	g.stdout = &stream{gate: g, w: stdout}
	g.stderr = &stream{gate: g, w: stderr}
	return g
}

// Add masks each value at once and redacts it from later output.
func (g *Gate) Add(values ...string) {
	if !g.active {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, value := range values {
		g.register(value, false)
	}
}

func (g *Gate) Stdout() io.Writer {
	if !g.active {
		return g.stdout.w
	}
	return g.stdout
}

func (g *Gate) Stderr() io.Writer {
	if !g.active {
		return g.stderr.w
	}
	return g.stderr
}

// Err reports the first mask-write failure.
func (g *Gate) Err() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.err
}

// Close flushes partial lines, redacted, and reports any mask-write failure.
func (g *Gate) Close() error {
	if !g.active {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	errs := []error{g.err}
	for _, s := range []*stream{g.stdout, g.stderr} {
		if len(s.pending) > 0 {
			errs = append(errs, s.emit(len(s.pending)))
		}
	}
	return errors.Join(errs...)
}

func (g *Gate) register(value string, derived bool) {
	if strings.TrimSpace(value) == "" || g.masked[value] || (derived && len(value) < minDerived) {
		return
	}
	g.masked[value] = true
	if !strings.Contains(value, "\n") {
		g.secrets = append(g.secrets, value)
		slices.SortFunc(g.secrets, func(a, b string) int {
			return cmp.Or(cmp.Compare(len(b), len(a)), strings.Compare(a, b))
		})
	}
	g.mask(value)
	for _, form := range derivedForms(value) {
		g.register(form, true)
	}
}

// derivedForms lists the lines of a multi-line value, the host part of an
// address, and the host fields of a known-hosts line.
func derivedForms(value string) []string {
	var forms []string
	if strings.Contains(value, "\n") {
		for line := range strings.Lines(value) {
			forms = append(forms, strings.TrimSpace(line))
		}
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		forms = append(forms, host)
	}
	if _, hosts, _, _, _, err := ssh.ParseKnownHosts([]byte(value)); err == nil {
		for _, host := range hosts {
			if bare, _, splitErr := net.SplitHostPort(host); splitErr == nil {
				host = bare
			}
			forms = append(forms, host)
		}
	}
	return forms
}

func (g *Gate) mask(value string) {
	if g.err != nil {
		return
	}
	escaped := strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(value)
	command := "::add-mask::" + escaped + "\n"
	if g.stdout.open {
		command = "\n" + command
	}
	if _, err := io.WriteString(g.stdout.w, command); err != nil {
		g.err = err
		return
	}
	g.stdout.open = false
}

// redact masks each pattern match before replacing every known value. It
// repeats until no match remains, because a replacement can cut a host name.
func (g *Gate) redact(line string) string {
	for {
		found := false
		for _, pattern := range g.patterns {
			for _, match := range pattern.FindAllString(line, -1) {
				g.register(match, false)
				found = true
			}
		}
		for _, secret := range g.secrets {
			line = strings.ReplaceAll(line, secret, placeholder)
		}
		if !found {
			return line
		}
	}
}

// cut returns where to split a long line: before the tail that could hold the
// start of a value, and past any value or match that crosses that point.
func (g *Gate) cut(data []byte) int {
	keep := maxHost
	if len(g.secrets) > 0 {
		keep = max(keep, len(g.secrets[0])-1)
	}
	end := len(data) - keep
	for end > 0 {
		next := g.past(data, end)
		if next == end {
			break
		}
		end = next
	}
	return end
}

// past returns the end of the first value or match that crosses end, or end.
func (g *Gate) past(data []byte, end int) int {
	for _, secret := range g.secrets {
		lo := max(0, end-len(secret)+1)
		i := bytes.Index(data[lo:min(len(data), end+len(secret)-1)], []byte(secret))
		if i >= 0 && lo+i < end {
			return lo + i + len(secret)
		}
	}
	for _, pattern := range g.patterns {
		for _, loc := range pattern.FindAllIndex(data, -1) {
			if loc[0] < end && end < loc[1] {
				return loc[1]
			}
		}
	}
	return end
}

// Write emits each complete line redacted. It reports len(p) for buffered bytes.
func (s *stream) Write(p []byte) (int, error) {
	g := s.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	if s == g.stdout && g.err != nil {
		return 0, g.err
	}
	s.pending = append(s.pending, p...)
	for {
		end := bytes.IndexByte(s.pending, '\n') + 1
		if end == 0 && len(s.pending) > maxLine {
			end = g.cut(s.pending)
		}
		if end <= 0 {
			return len(p), nil
		}
		if err := s.emit(end); err != nil {
			return 0, err
		}
	}
}

func (s *stream) emit(end int) error {
	g := s.gate
	line := g.redact(string(s.pending[:end]))
	s.pending = slices.Delete(s.pending, 0, end)
	if s == g.stdout && g.err != nil {
		return g.err
	}
	if _, err := io.WriteString(s.w, line); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	s.open = !strings.HasSuffix(line, "\n")
	return nil
}
