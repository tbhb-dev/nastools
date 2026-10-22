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
	g.Register(redact.Set{Values: []string{secret}})
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
	g.Register(redact.Set{
		Values: []string{secret, "", "  ", secret, "%\r%"},
		Hosts:  []string{"secret-2.example.invalid:22", knownHosts},
	})
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
		maskPrefix+"%25%0D%25\n"+
		maskPrefix+"secret-2.example.invalid:22\n"+
		maskPrefix+"secret-2.example.invalid\n"+
		maskPrefix+strings.ReplaceAll(strings.TrimSuffix(knownHosts, "\n"), "\n", "%0A")+"\n"+
		maskPrefix+line(0)+"\n"+
		maskPrefix+"secret-4.example.invalid\n"+
		maskPrefix+line(1)+"\n"+
		maskPrefix+"secret-5.example.invalid\n"+
		"c ***\npartial", text(*log, "out"))
	assert.Equal(t, "a *** b\n*** ***", text(*log, "err"))
}

// TestRegisterOrderIndependent checks that the masks and the redacted
// output depend only on which values arrive in which group. The result
// stays the same for each order of the inputs, for each split of that order
// into calls, and when the calls run twice. The address and a known-hosts
// line also arrive as plain values, as when a config requires them.
func TestRegisterOrderIndependent(t *testing.T) {
	t.Parallel()
	inputs := []input{
		{secret, false},
		{"secret-2.example.invalid:22", false},
		{"secret-2.example.invalid:22", true},
		{knownHosts, true},
		{strings.Split(knownHosts, "\n")[0], false},
	}
	wantMasks, wantRedacted := registerCalls(t, [][]input{inputs})
	assert.Equal(t, "a *** *** ***\n*** *** ***\n", wantRedacted)
	for _, order := range permutations(inputs) {
		for _, calls := range splits(order) {
			for _, repeated := range [][][]input{calls, slices.Concat(calls, calls)} {
				masks, redacted := registerCalls(t, repeated)
				if !assert.Equal(t, wantMasks, masks, "%v", repeated) ||
					!assert.Equal(t, wantRedacted, redacted, "%v", repeated) {
					return
				}
			}
		}
	}
}

// input is one value to register, as an address or known-hosts value when
// host is set.
type input struct {
	value string
	host  bool
}

// registerCalls registers each call's inputs together, then redacts a line
// of every form. It returns the sorted masks and the redacted text.
func registerCalls(t *testing.T, calls [][]input) ([]string, string) {
	t.Helper()
	g, log := gate(0)
	for _, call := range calls {
		var set redact.Set
		for _, in := range call {
			if in.host {
				set.Hosts = append(set.Hosts, in.value)
			} else {
				set.Values = append(set.Values, in.value)
			}
		}
		g.Register(set)
	}
	_, err := io.WriteString(g.Stderr(), "a "+secret+" secret-2.example.invalid:22 secret-2.example.invalid\n"+
		"secret-4.example.invalid secret-5.example.invalid "+tailnet+"\n")
	require.NoError(t, err)
	require.NoError(t, g.Close())
	masks := strings.Split(text(*log, "out"), "\n")
	slices.Sort(masks)
	return masks, text(*log, "err")
}

// splits lists every way to cut items into consecutive calls.
func splits[T any](items []T) [][][]T {
	result := make([][][]T, 0, 1<<(len(items)-1))
	for cuts := range cap(result) {
		var calls [][]T
		first := 0
		for i := 1; i <= len(items); i++ {
			if i == len(items) || cuts&(1<<(i-1)) != 0 {
				calls = append(calls, items[first:i])
				first = i
			}
		}
		result = append(result, calls)
	}
	return result
}

// permutations lists every order of items.
func permutations[T any](items []T) [][]T {
	if len(items) <= 1 {
		return [][]T{items}
	}
	var result [][]T
	for i := range items {
		for _, rest := range permutations(slices.Concat(items[:i], items[i+1:])) {
			result = append(result, slices.Concat([]T{items[i]}, rest))
		}
	}
	return result
}

func TestTailnetNameMaskedBeforeItsLine(t *testing.T) {
	t.Parallel()
	for _, name := range []string{tailnet, "NODE.EXAMPLE.TS.NET", "Node.Example.Ts.Net"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g, log := gate(0)
			_, err := io.WriteString(g.Stderr(), "serving https://"+name+":443\n")
			require.NoError(t, err)
			_, err = io.WriteString(g.Stdout(), name+"\n")
			require.NoError(t, err)
			require.NoError(t, g.Close())
			assert.Equal(t, []entry{
				{"out", maskPrefix + name + "\n"},
				{"err", "serving https://***:443\n"},
				{"out", "***\n"},
			}, *log)
		})
	}
}

// TestHostFormsOnlyForHosts checks that only an address or known-hosts
// value yields host names, so a static file line keeps its words.
func TestHostFormsOnlyForHosts(t *testing.T) {
	t.Parallel()
	g, log := gate(0)
	g.Register(redact.Set{
		Values: []string{"services: web", "hostname:8080", "secret-4.example.invalid ssh-ed25519 AAAA"},
		Hosts:  []string{"secret-2.example.invalid:22"},
	})
	_, err := io.WriteString(g.Stderr(), "services hostname secret-4.example.invalid secret-2.example.invalid\n")
	require.NoError(t, err)
	require.NoError(t, g.Close())
	assert.Equal(t, "services hostname secret-4.example.invalid ***\n", text(*log, "err"))
}

func TestReplacementCannotExposeAName(t *testing.T) {
	t.Parallel()
	g, log := gate(0)
	// The value and the name overlap, so neither may leave part of the other.
	g.Register(redact.Set{Values: []string{"long-secret-token=abc-node"}})
	_, err := io.WriteString(g.Stderr(), "long-secret-token=abc-node-2.example.ts.net\n")
	require.NoError(t, err)
	require.NoError(t, g.Close())
	assert.Equal(t, "***\n", text(*log, "err"))
}

func TestOverlappingValues(t *testing.T) {
	t.Parallel()
	g, log := gate(0)
	g.Register(redact.Set{Values: []string{"overlap", "lapping", "ping-pong"}})
	_, err := io.WriteString(g.Stderr(), "an overlapping-pong one, overlap lapping\n")
	require.NoError(t, err)
	require.NoError(t, g.Close())
	assert.Equal(t, "an *** one, *** ***\n", text(*log, "err"))
}

func TestTrailingNewlineValue(t *testing.T) {
	t.Parallel()
	g, log := gate(0)
	// Only the first two values are multi-line once their blank lines go.
	g.Register(redact.Set{Values: []string{
		"abc1234\n", "def5678\r\n", "first-line\nx\n", "\npass123", "word456\n \n", " \r\n\tkey789\n\n", "\n \n",
	}})
	_, err := io.WriteString(g.Stderr(), "abc1234 def5678 first-line x pass123 word456 \tkey789\n")
	require.NoError(t, err)
	require.NoError(t, g.Close())
	assert.Equal(t, maskPrefix+"abc1234\n"+maskPrefix+"def5678\n"+
		maskPrefix+"first-line%0Ax\n"+maskPrefix+"first-line\n"+
		maskPrefix+"pass123\n"+maskPrefix+"word456\n"+maskPrefix+"\tkey789\n", text(*log, "out"))
	assert.Equal(t, "*** *** *** x *** *** ***\n", text(*log, "err"))
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
			g.Register(redact.Set{Values: tc.values})
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

// TestLongLineCut checks that a cut never splits a value or a host name,
// and holds back the start of a value that hasn't fully arrived.
func TestLongLineCut(t *testing.T) {
	t.Parallel()
	filler := strings.Repeat("_", 64<<10)
	a, b := strings.Repeat("a", 300), "a"+strings.Repeat("b", 299)
	for _, tc := range []struct {
		name    string
		secrets []string
		writes  []string
		first   string // Standard error after the first write.
		want    string
	}{
		// The default cut point falls inside the value or the name.
		{
			"value",
			[]string{secret},
			[]string{filler + secret + strings.Repeat("_", 243), ""},
			filler,
			filler + "***" + strings.Repeat("_", 243),
		},
		{
			"name", nil,
			[]string{filler + tailnet + strings.Repeat("_", 243), ""},
			filler,
			filler + "***" + strings.Repeat("_", 243),
		},
		// b overlaps the end of a and arrives only in part before the cut.
		{"overlap", []string{a, b}, []string{filler + a + b[1:101], b[101:] + "\n"}, filler, filler + "***\n"},
		{
			"overlap, one write",
			[]string{a, b},
			[]string{filler + a + b[1:] + "\n"},
			filler + "***\n",
			filler + "***\n",
		},
		{"repeated", []string{a}, []string{filler + strings.Repeat("a", 450), "x\n"}, filler, filler + "***x\n"},
		// A run of host bytes too long for a name is cut within it, but not
		// within a value.
		{
			"host run",
			[]string{secret},
			[]string{"x" + secret + strings.Repeat("y", 70<<10), "\n"},
			"x***" + strings.Repeat("y", 70<<10-253), "x***" + strings.Repeat("y", 70<<10) + "\n",
		},
		{
			"host run, value at the cut",
			[]string{secret},
			[]string{strings.Repeat("y", 64<<10) + secret + strings.Repeat("y", 240), "\n"},
			strings.Repeat("y", 64<<10), strings.Repeat("y", 64<<10) + "***" + strings.Repeat("y", 240) + "\n",
		},
		// Nothing outside the covered run can be cut. The line waits for its end.
		{"held", []string{strings.Repeat("ab", 4)}, []string{strings.Repeat("ab", 40<<10), "ab", "\n"}, "", "***\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g, log := gate(0)
			g.Register(redact.Set{Values: tc.secrets})
			for i, write := range tc.writes {
				_, err := io.WriteString(g.Stderr(), write)
				require.NoError(t, err)
				if i == 0 {
					assert.Equal(t, tc.first, text(*log, "err"))
				}
			}
			require.NoError(t, g.Close())
			assert.Equal(t, tc.want, text(*log, "err"))
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
	g.Register(redact.Set{Values: []string{secret}})
	var workers sync.WaitGroup
	for i := range 8 {
		workers.Go(func() {
			w := g.Stdout()
			if i%2 == 1 {
				w = g.Stderr()
			}
			for j := range 50 {
				g.Register(redact.Set{Values: []string{"added-" + strconv.Itoa(i) + ".example.invalid"}})
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
	g.Register(redact.Set{Values: secrets})
	writeChunks(t, g.Stderr(), input, splits)
	writeChunks(t, g.Stdout(), input, splits)
	require.NoError(t, g.Close())
	return *log
}

// FuzzGate builds two secrets that overlap by overlap bytes, a+b[:overlap]
// and b, and checks a line holding a+b besides the fuzzed input. With long
// set, the line starts with enough filler that a+b crosses the cut point.
func FuzzGate(f *testing.F) {
	f.Add(
		secret,
		"secret-2.example.invalid:22",
		"a "+secret+" b\n"+tailnet+"\nsecret-2.example.invalid",
		[]byte{0, 4, 2},
		uint8(0),
		false,
	)
	f.Add("over", "lapping", "an overlapping one\n", []byte{2}, uint8(3), false)
	f.Add(
		"prefix-node",
		"x",
		"prefix-node.example.ts.net prefix-nodes.example.ts.net\n\n",
		[]byte{6, 0},
		uint8(1),
		false,
	)
	f.Add("%", "\r", "100%\r\n", []byte{}, uint8(0), false)
	f.Add(strings.Repeat("a", 299), "a"+strings.Repeat("b", 299), "", []byte{99, 199}, uint8(1), true)
	f.Add(
		strings.Repeat("ab", 150),
		strings.Repeat("ab", 150),
		"NODE.EXAMPLE.TS.NET\n",
		[]byte{255, 7},
		uint8(200),
		true,
	)
	f.Add("x.example.ts.net", strings.Repeat("y", 280)+".example.ts.net", "", []byte{254, 3}, uint8(0), true)
	f.Fuzz(func(t *testing.T, a, b, input string, splits []byte, overlap uint8, long bool) {
		if len(input) > 4096 {
			t.Skip("long lines have their own test")
		}
		secrets := fuzzSecrets(a+b[:int(overlap)%(len(b)+1)], b)
		filler := ""
		if long {
			filler = strings.Repeat("_", 64<<10)
		}
		if len(secrets) == 2 || long {
			input = filler + a + b + "\n" + input
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
			if long {
				assert.True(t, strings.HasSuffix(first, placeholder))
			} else {
				assert.Equal(t, placeholder, first)
			}
		}
	})
}

// fuzzSecrets keeps the values that the gate registers unchanged as one line.
func fuzzSecrets(values ...string) []string {
	var secrets []string
	for _, value := range values {
		// The gate drops a trailing carriage return, which the line check
		// would then find uncovered.
		if !strings.ContainsAny(value, "*\n") && !strings.HasSuffix(value, "\r") && strings.TrimSpace(value) != "" {
			secrets = append(secrets, value)
		}
	}
	return secrets
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
