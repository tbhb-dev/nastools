// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package redact_test

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tbhb-dev/nastools/internal/redact"
)

const (
	placeholder = "***"
	secret      = "secret-1.example.invalid"
	tailnet     = "node.example.ts.net"
	maskPrefix  = "::add-mask::"
	knownHosts  = "secret-4.example.invalid ssh-ed25519 " +
		"AAAAC3NzaC1lZDI1NTE5AAAAICZ4niA2YR6ErsZQRahQYkioleR/p9/qUOStEMhVy/K5\n" +
		"[secret-5.example.invalid]:2222 ssh-ed25519 " +
		"AAAAC3NzaC1lZDI1NTE5AAAAICZ4niA2YR6ErsZQRahQYkioleR/p9/qUOStEMhVy/K5\n"
)

var errBroken = errors.New("broken pipe")

type entry struct {
	stream string
	text   string
}

// recorder logs both streams in one order and fails every write from the
// numbered one on.
type recorder struct {
	log    *[]entry
	stream string
	failAt int
	writes int
}

func (r *recorder) Write(p []byte) (int, error) {
	r.writes++
	if r.failAt > 0 && r.writes >= r.failAt {
		return 0, errBroken
	}
	*r.log = append(*r.log, entry{r.stream, string(p)})
	return len(p), nil
}

func gate(failAt int) (*redact.Gate, *[]entry) {
	log := new([]entry)
	g := redact.New(
		&recorder{log: log, stream: "out", failAt: failAt},
		&recorder{log: log, stream: "err"},
		true,
		redact.TailnetName,
	)
	return g, log
}

func text(log []entry, stream string) string {
	var b strings.Builder
	for _, e := range log {
		if e.stream == stream {
			b.WriteString(e.text)
		}
	}
	return b.String()
}

func TestInactivePassesThrough(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	g := redact.New(&out, &errOut, false, redact.TailnetName)
	g.Add(secret)
	assert.Same(t, &out, g.Stdout())
	assert.Same(t, &errOut, g.Stderr())
	_, err := io.WriteString(g.Stdout(), secret+" "+tailnet)
	require.NoError(t, err)
	require.NoError(t, g.Close())
	require.NoError(t, g.Err())
	assert.Equal(t, secret+" "+tailnet, out.String())
}

func TestMasksAndRedactsDerivedForms(t *testing.T) {
	t.Parallel()
	g, log := gate(0)
	g.Add(secret, "secret-2.example.invalid:22", knownHosts, "", "  ", secret, "%\r%")
	_, err := io.WriteString(
		g.Stderr(),
		"a secret-1.example.invalid b\nsecret-2.example.invalid secret-4.example.invalid",
	)
	require.NoError(t, err)
	_, err = io.WriteString(g.Stdout(), "c secret-5.example.invalid\npartial")
	require.NoError(t, err)
	require.NoError(t, g.Close())
	line := func(i int) string { return strings.TrimSpace(strings.Split(knownHosts, "\n")[i]) }
	assert.Equal(t, maskPrefix+secret+"\n"+
		maskPrefix+"secret-2.example.invalid:22\n"+
		maskPrefix+"secret-2.example.invalid\n"+
		maskPrefix+strings.ReplaceAll(strings.TrimSuffix(knownHosts, "\n"), "\n", "%0A")+"\n"+
		maskPrefix+line(0)+"\n"+
		maskPrefix+"secret-4.example.invalid\n"+
		maskPrefix+line(1)+"\n"+
		maskPrefix+"secret-5.example.invalid\n"+
		maskPrefix+"%25%0D%25\n"+
		"c ***\npartial", text(*log, "out"))
	assert.Equal(t, "a *** b\n*** ***", text(*log, "err"))
}

func TestTailnetNameMaskedBeforeItsLine(t *testing.T) {
	t.Parallel()
	g, log := gate(0)
	_, err := io.WriteString(g.Stderr(), "serving https://"+tailnet+":443\n")
	require.NoError(t, err)
	_, err = io.WriteString(g.Stdout(), tailnet+"\n")
	require.NoError(t, err)
	require.NoError(t, g.Close())
	assert.Equal(t, []entry{
		{"out", maskPrefix + tailnet + "\n"},
		{"err", "serving https://***:443\n"},
		{"out", "***\n"},
	}, *log)
}

func TestReplacementCannotExposeAName(t *testing.T) {
	t.Parallel()
	g, log := gate(0)
	// The value and the name overlap, so neither may leave part of the other.
	g.Add("long-secret-token=abc-node")
	_, err := io.WriteString(g.Stderr(), "long-secret-token=abc-node-2.example.ts.net\n")
	require.NoError(t, err)
	require.NoError(t, g.Close())
	assert.Equal(t, "***\n", text(*log, "err"))
}

func TestOverlappingValues(t *testing.T) {
	t.Parallel()
	g, log := gate(0)
	g.Add("overlap", "lapping", "ping-pong")
	_, err := io.WriteString(g.Stderr(), "an overlapping-pong one, overlap lapping\n")
	require.NoError(t, err)
	require.NoError(t, g.Close())
	assert.Equal(t, "an *** one, *** ***\n", text(*log, "err"))
}

func TestTrailingNewlineValue(t *testing.T) {
	t.Parallel()
	g, log := gate(0)
	g.Add("abc1234\n", "def5678\r\n", "first-line\nx\n")
	_, err := io.WriteString(g.Stderr(), "abc1234 def5678 first-line x\n")
	require.NoError(t, err)
	require.NoError(t, g.Close())
	assert.Equal(t, maskPrefix+"abc1234\n"+maskPrefix+"def5678\n"+
		maskPrefix+"first-line%0Ax\n"+maskPrefix+"first-line\n", text(*log, "out"))
	assert.Equal(t, "*** *** *** x\n", text(*log, "err"))
}

func TestCloseReportsFlushMaskFailure(t *testing.T) {
	t.Parallel()
	g, log := gate(1)
	// The name is new and its line is partial, so only Close writes its mask.
	_, err := io.WriteString(g.Stderr(), "serving "+tailnet)
	require.NoError(t, err)
	require.NoError(t, g.Err())
	require.ErrorIs(t, g.Close(), errBroken)
	assert.Equal(t, "serving ***", text(*log, "err"))
}

func TestMaskFailureIsSticky(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		failAt int
		values []string
	}{
		{"discovery", 2, []string{secret, "secret-2.example.invalid"}},
		{"dynamic", 2, []string{secret}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g, log := gate(tc.failAt)
			g.Add(tc.values...)
			_, err := io.WriteString(g.Stderr(), "x "+tailnet+" secret-2.example.invalid "+secret+"\n")
			require.NoError(t, err)
			require.ErrorIs(t, g.Err(), errBroken)
			_, err = io.WriteString(g.Stdout(), "later\n")
			require.ErrorIs(t, err, errBroken)
			_, err = io.WriteString(g.Stderr(), "still "+secret+"\n")
			require.NoError(t, err)
			require.ErrorIs(t, g.Close(), errBroken)
			assert.Equal(t, maskPrefix+secret+"\n", text(*log, "out"))
			stderr := text(*log, "err")
			assert.NotContains(t, stderr, secret)
			assert.NotContains(t, stderr, tailnet)
			assert.Contains(t, stderr, "still ***\n")
		})
	}
}

func TestLongLineCut(t *testing.T) {
	t.Parallel()
	for _, value := range []string{secret, tailnet} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			g, log := gate(0)
			g.Add(secret)
			// The default cut point falls inside value.
			prefix, suffix := strings.Repeat("_", 64<<10), strings.Repeat("_", 243)
			_, err := io.WriteString(g.Stderr(), prefix+value+suffix)
			require.NoError(t, err)
			assert.Equal(t, prefix+"***", text(*log, "err"))
			_, err = io.WriteString(g.Stdout(), prefix+value+suffix+"\n")
			require.NoError(t, err)
			require.NoError(t, g.Close())
			want := prefix + "***" + suffix
			assert.Equal(t, want, text(*log, "err"))
			assert.True(t, strings.HasSuffix(text(*log, "out"), want+"\n"))
		})
	}
}

func TestMaskAfterCutStartsALine(t *testing.T) {
	t.Parallel()
	g, log := gate(0)
	long := strings.Repeat("x", 70<<10)
	_, err := io.WriteString(g.Stdout(), long)
	require.NoError(t, err)
	_, err = io.WriteString(g.Stderr(), tailnet+"\n")
	require.NoError(t, err)
	require.NoError(t, g.Close())
	cut := len(long) - 253
	assert.Equal(t, long[:cut]+"\n"+maskPrefix+tailnet+"\n"+long[cut:], text(*log, "out"))
}

func TestConcurrentStreams(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	g := redact.New(&out, &errOut, true, redact.TailnetName)
	g.Add(secret)
	var workers sync.WaitGroup
	for i := range 8 {
		workers.Go(func() {
			w := g.Stdout()
			if i%2 == 1 {
				w = g.Stderr()
			}
			for j := range 50 {
				g.Add("added-" + strconv.Itoa(i) + ".example.invalid")
				line := "line " + secret + " node-" + strconv.Itoa(j) + ".example.ts.net\n"
				_, err := io.WriteString(w, line)
				assert.NoError(t, err)
			}
		})
	}
	workers.Wait()
	require.NoError(t, g.Close())
	assert.Equal(t, strings.Repeat("line *** ***\n", 200), errOut.String())
	lines := 0
	for line := range strings.Lines(out.String()) {
		if !strings.HasPrefix(line, maskPrefix) {
			assert.Equal(t, "line *** ***\n", line)
			lines++
		}
	}
	assert.Equal(t, 200, lines)
}

// writeChunks writes data in chunks of 1 + splits[i] bytes, cycling through
// splits. Without splits it writes data whole.
func writeChunks(t *testing.T, w io.Writer, data string, splits []byte) {
	t.Helper()
	for i := 0; len(data) > 0; i++ {
		n := len(data)
		if len(splits) > 0 {
			n = min(n, 1+int(splits[i%len(splits)]))
		}
		_, err := io.WriteString(w, data[:n])
		require.NoError(t, err)
		data = data[n:]
	}
}

func run(t *testing.T, secrets []string, input string, splits []byte) []entry {
	t.Helper()
	g, log := gate(0)
	g.Add(secrets...)
	writeChunks(t, g.Stderr(), input, splits)
	writeChunks(t, g.Stdout(), input, splits)
	require.NoError(t, g.Close())
	return *log
}

// FuzzGate builds two secrets that overlap by overlap bytes, a+b[:overlap]
// and b, and checks a line holding a+b besides the fuzzed input.
func FuzzGate(f *testing.F) {
	f.Add(
		secret,
		"secret-2.example.invalid:22",
		"a "+secret+" b\n"+tailnet+"\nsecret-2.example.invalid",
		[]byte{0, 4, 2},
		uint8(0),
	)
	f.Add("over", "lapping", "an overlapping one\n", []byte{2}, uint8(3))
	f.Add("prefix-node", "x", "prefix-node.example.ts.net prefix-nodes.example.ts.net\n\n", []byte{6, 0}, uint8(1))
	f.Add("%", "\r", "100%\r\n", []byte{}, uint8(0))
	f.Fuzz(func(t *testing.T, a, b, input string, splits []byte, overlap uint8) {
		if len(input) > 4096 {
			t.Skip("long lines have their own test")
		}
		var secrets []string
		for _, value := range []string{a + b[:int(overlap)%(len(b)+1)], b} {
			// The gate drops a trailing carriage return, which the line check
			// below would then find uncovered.
			if !strings.ContainsAny(value, "*\n") && !strings.HasSuffix(value, "\r") && strings.TrimSpace(value) != "" {
				secrets = append(secrets, value)
			}
		}
		if len(secrets) == 2 {
			input = a + b + "\n" + input
		}
		whole := run(t, secrets, input, nil)
		chunked := run(t, secrets, input, append(splits, 0))
		assert.Equal(t, text(whole, "out"), text(chunked, "out"))
		assert.Equal(t, text(whole, "err"), text(chunked, "err"))
		assertNoSurvivors(t, whole, secrets)
		assertMasksFirst(t, whole, input)
		if len(secrets) == 2 {
			// The secrets cover the whole line between them.
			first, _, _ := strings.Cut(text(whole, "err"), "\n")
			assert.Equal(t, placeholder, first)
		}
	})
}

// assertNoSurvivors checks every output line outside a mask command.
func assertNoSurvivors(t *testing.T, log []entry, secrets []string) {
	t.Helper()
	lines := slices.Collect(strings.Lines(text(log, "err")))
	for line := range strings.Lines(text(log, "out")) {
		if !strings.HasPrefix(line, maskPrefix) {
			lines = append(lines, line)
		}
	}
	for _, line := range lines {
		for _, value := range secrets {
			assert.NotContains(t, line, value)
		}
		assert.Empty(t, redact.TailnetName.FindString(line))
	}
}

// assertMasksFirst checks that each name in a standard error line is masked
// before that line.
func assertMasksFirst(t *testing.T, log []entry, input string) {
	t.Helper()
	masked := make(map[string]bool)
	inputs := slices.Collect(strings.Lines(input))
	for _, e := range log {
		if value, ok := strings.CutPrefix(e.text, maskPrefix); ok && e.stream == "out" {
			masked[strings.TrimSuffix(value, "\n")] = true
			continue
		}
		if e.stream == "out" {
			continue
		}
		require.NotEmpty(t, inputs)
		for _, name := range redact.TailnetName.FindAllString(inputs[0], -1) {
			assert.True(t, masked[name], name)
		}
		inputs = inputs[1:]
	}
}
