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

// TailnetName matches a MagicDNS host name in any case, as DNS compares names.
var TailnetName = regexp.MustCompile(`(?i)[a-z0-9-]+(\.[a-z0-9-]+)*\.ts\.net`)

const (
	// A derived form shorter than this would redact common text everywhere.
	minDerived = 8
	// A line without a newline is cut once it grows past this many bytes.
	maxLine = 64 << 10
	// The tail kept after a cut has room for a whole DNS name.
	maxHost = 253
	// After a failed cut, the line must double before the next try.
	heldGrowth  = 2
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
	held    int  // Pending length at which the next cut runs, after one failed.
}

// New returns a gate over the process streams. An inactive gate passes the
// writers through unchanged. Each pattern matches host names, so only
// letters, digits, hyphens, and dots.
func New(stdout, stderr io.Writer, active bool, patterns ...*regexp.Regexp) *Gate {
	g := &Gate{active: active, masked: make(map[string]bool), patterns: patterns}
	g.stdout = &stream{gate: g, w: stdout}
	g.stderr = &stream{gate: g, w: stderr}
	return g
}

// Set holds the private values to register.
type Set struct {
	Values []string
	Hosts  []string // Address and known-hosts values, whose host names are private too.
}

// Register masks each value in set at once, with every form derived from
// it, and redacts them all from later output. A value's forms depend only
// on the value and its group, never on what the gate already contains, so
// the same values in any order, grouping, or repetition leave the same
// secrets.
func (g *Gate) Register(set Set) {
	if !g.active {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.register(set)
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

// Close flushes partial lines, redacted, and reports any mask-write failure,
// including one that a name in a flushed line causes.
func (g *Gate) Close() error {
	if !g.active {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var errs []error
	for _, s := range []*stream{g.stdout, g.stderr} {
		if len(s.pending) > 0 {
			errs = append(errs, s.emit(len(s.pending)))
		}
	}
	return errors.Join(append(errs, g.err)...)
}

// register derives every form of set first and only then skips the forms
// the gate already contains.
func (g *Gate) register(set Set) {
	for _, value := range set.forms() {
		if g.masked[value] {
			continue
		}
		g.masked[value] = true
		if !strings.Contains(value, "\n") {
			g.secrets = append(g.secrets, value)
		}
		g.mask(value)
	}
	slices.SortFunc(g.secrets, func(a, b string) int {
		return cmp.Or(cmp.Compare(len(b), len(a)), strings.Compare(a, b))
	})
}

// forms lists every value in set, each followed by the forms derived from it.
func (set Set) forms() []string {
	all := make([]string, 0, len(set.Values)+len(set.Hosts))
	for _, value := range set.Values {
		all = append(all, expand(value, false, false)...)
	}
	for _, value := range set.Hosts {
		all = append(all, expand(value, false, true)...)
	}
	return all
}

// expand returns value without its blank first and last lines, followed by
// the forms derived from it. A value then counts as multi-line only for a
// newline between two of its lines.
func expand(value string, derived, hosts bool) []string {
	value = trimBlankLines(value)
	if strings.TrimSpace(value) == "" || (derived && len(value) < minDerived) {
		return nil
	}
	forms := []string{value}
	for _, form := range derivedForms(value, hosts) {
		forms = append(forms, expand(form, true, hosts)...)
	}
	return forms
}

// trimBlankLines drops the whitespace-only lines at either end of value,
// and the carriage return that ends its last line.
func trimBlankLines(value string) string {
	for {
		line, rest, found := strings.Cut(value, "\n")
		if !found || strings.TrimSpace(line) != "" {
			break
		}
		value = rest
	}
	for {
		i := strings.LastIndexByte(value, '\n')
		if i < 0 || strings.TrimSpace(value[i+1:]) != "" {
			break
		}
		value = value[:i]
	}
	return strings.TrimRight(value, "\r")
}

// derivedForms lists the lines of a multi-line value, or a single-line value
// without its surrounding whitespace. For an address or known-hosts value, it
// adds the host part of an address and the host fields of a known-hosts line.
func derivedForms(value string, hosts bool) []string {
	var forms []string
	if strings.Contains(value, "\n") {
		for line := range strings.Lines(value) {
			forms = append(forms, strings.TrimSpace(line))
		}
	} else if trimmed := strings.TrimSpace(value); trimmed != value {
		forms = append(forms, trimmed)
	}
	if !hosts {
		return forms
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
			if matches := pattern.FindAllString(line, -1); len(matches) > 0 {
				g.register(Set{Values: matches})
				found = true
			}
		}
		line = g.replace(line)
		if !found {
			return line
		}
	}
}

// replace swaps each run of bytes that any known value covers for one
// placeholder. Values that overlap then don't leave part of either behind.
func (g *Gate) replace(line string) string {
	covered := g.cover(line)
	var b strings.Builder
	for i := range len(line) {
		switch {
		case !covered[i]:
			b.WriteByte(line[i])
		case i == 0 || !covered[i-1]:
			b.WriteString(placeholder)
		}
	}
	return b.String()
}

// cover marks each byte of line that a known value covers.
func (g *Gate) cover(line string) []bool {
	covered := make([]bool, len(line))
	for _, secret := range g.secrets {
		for start := 0; ; start++ {
			i := strings.Index(line[start:], secret)
			if i < 0 {
				break
			}
			start += i
			for j := range len(secret) {
				covered[start+j] = true
			}
		}
	}
	return covered
}

// cut returns where to split a long line, or 0 to buffer the whole line.
// The split keeps back room for all but one byte of the longest value and
// for a whole DNS name, so every value that starts before it has arrived.
// It then moves back past every byte that a value or a match covers, so
// none crosses it and a placeholder never splits in two. It prefers a byte
// that can't be part of a host name, because a match can't extend across
// one. A split within a longer run of host bytes can only cross a name too
// long for DNS. A line that one covered run fills waits until the run ends.
func (g *Gate) cut(data []byte) int {
	keep := maxHost
	if len(g.secrets) > 0 {
		keep = max(keep, len(g.secrets[0])-1)
	}
	line := string(data)
	covered := g.cover(line)
	for _, pattern := range g.patterns {
		for _, loc := range pattern.FindAllStringIndex(line, -1) {
			for i := loc[0]; i < loc[1]; i++ {
				covered[i] = true
			}
		}
	}
	fallback := 0
	for end := len(data) - keep; end > 0; end-- {
		switch {
		case covered[end-1]:
		case !hostByte(data[end-1]) || !hostByte(data[end]):
			return end
		case fallback == 0:
			fallback = end
		}
	}
	return fallback
}

func hostByte(c byte) bool {
	return c == '-' || c == '.' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
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
		if end == 0 && len(s.pending) > max(maxLine, s.held) {
			if end = g.cut(s.pending); end == 0 {
				s.held = heldGrowth * len(s.pending)
			}
		}
		if end <= 0 {
			return len(p), nil
		}
		s.held = 0
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
