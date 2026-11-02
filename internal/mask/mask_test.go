package mask

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, w *Writer, s string) {
	t.Helper()
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
}

func flush(t *testing.T, w *Writer) {
	t.Helper()
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
}

func run(t *testing.T, secrets []string, writes ...string) string {
	t.Helper()
	var out bytes.Buffer
	w := NewWriter(&out, secrets)
	for _, s := range writes {
		write(t, w, s)
	}
	flush(t, w)
	return out.String()
}

func TestMasksInsideALine(t *testing.T) {
	if got := run(t, []string{"s3cretvalue"}, "token=s3cretvalue done\n"); got != "token=*** done\n" {
		t.Fatalf("%q", got)
	}
}

func TestMasksASecretSplitAcrossWrites(t *testing.T) {
	if got := run(t, []string{"s3cretvalue"}, "a s3cre", "tvalue b\n"); got != "a *** b\n" {
		t.Fatalf("%q", got)
	}
}

func TestASecretSlidAcrossEverySplitPoint(t *testing.T) {
	const in = "pre s3cretvalue post\n"
	for i := 0; i <= len(in); i++ {
		if got := run(t, []string{"s3cretvalue"}, in[:i], in[i:]); got != "pre *** post\n" {
			t.Fatalf("split %d: %q", i, got)
		}
	}
}

func TestMasksASecretSplitAcrossThreeWrites(t *testing.T) {
	if got := run(t, []string{"s3cretvalue"}, "a s3c", "retva", "lue b\n"); got != "a *** b\n" {
		t.Fatalf("%q", got)
	}
}

func TestTheTailIsMaskedOnFlushAndOrdinaryOutputIsNotHeld(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out, []string{"s3cretvalue"})
	write(t, w, "hello world")
	if out.String() != "hello world" {
		t.Fatalf("ordinary output was held: %q", out.String())
	}
	write(t, w, " x=s3cretvalue")
	flush(t, w)
	if out.String() != "hello world x=***" {
		t.Fatalf("%q", out.String())
	}
}

func TestAPartialPrefixIsHeldUntilItResolves(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out, []string{"s3cretvalue"})
	write(t, w, "x s3cre")
	if out.String() != "x " {
		t.Fatalf("%q", out.String())
	}
	write(t, w, "zzz")
	if out.String() != "x s3crezzz" {
		t.Fatalf("%q", out.String())
	}
}

func TestLongestSecretFirstAndEmptyIgnored(t *testing.T) {
	if got := run(t, []string{"", "abc", "abcdef"}, "abcdef abc\n"); got != "*** ***\n" {
		t.Fatalf("%q", got)
	}
}

func TestAPrefixOfAnotherSecretSplit(t *testing.T) {
	for i := 0; i <= len("abcdef x"); i++ {
		in := "abcdef x"
		if got := run(t, []string{"abc", "abcdef"}, in[:i], in[i:]); got != "*** x" {
			t.Fatalf("split %d: %q", i, got)
		}
	}
}

func TestNoSecretsPassesAHugeLineThrough(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out, nil)
	write(t, w, strings.Repeat("x", 70000))
	if out.Len() != 70000 {
		t.Fatalf("wrote %d", out.Len())
	}
}

func TestAHugeLineWithASecretAtAnyBoundary(t *testing.T) {
	const secret = "s3cretvalue"
	for _, tail := range []string{"", "yyyyy"} {
		pad := strings.Repeat("x", 64*1024)
		got := run(t, []string{secret, "tok123"}, pad+secret+tail, "\n")
		if got != pad+"***"+tail+"\n" {
			t.Fatalf("tail %q leaked or changed", tail)
		}
	}
}

func TestASecretWithANewlineSplitAcrossWrites(t *testing.T) {
	if got := run(t, []string{"LINE1\nLINE2"}, "k=LINE1\n", "LINE2\n"); got != "k=***\n" {
		t.Fatalf("%q", got)
	}
}

func TestOverlappingSecretsLeaveNoFragment(t *testing.T) {
	secrets := []string{"abcd", "cdef"}
	if got := run(t, secrets, "abcdef"); got != "***" {
		t.Fatalf("%q", got)
	}
	if got := run(t, secrets, "abcd", "xx"); got != "***xx" {
		t.Fatalf("%q", got)
	}
	if got := run(t, secrets, "ab", "cd", "ef"); got != "***" {
		t.Fatalf("%q", got)
	}
}

type failing struct{ calls, bytes int }

func (f *failing) Write(p []byte) (int, error) {
	f.calls++
	f.bytes += len(p)
	return 0, errors.New("disk full")
}

func TestAFailingWriterDoesNotGrowOrReplay(t *testing.T) {
	f := &failing{}
	w := NewWriter(f, []string{"s3cretvalue"})
	if _, err := w.Write([]byte("one\n")); err == nil {
		t.Fatal("error swallowed")
	}
	if _, err := w.Write([]byte("two\n")); err == nil {
		t.Fatal("error swallowed")
	}
	if f.bytes != 8 || len(w.buf) != 0 {
		t.Fatalf("replayed %d bytes, held %d", f.bytes, len(w.buf))
	}
}

func TestASelfOverlappingSecretDoesNotAccumulate(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out, []string{"aa"})
	for range 10000 {
		write(t, w, "a")
		if len(w.buf) > 1 {
			t.Fatalf("held %d bytes", len(w.buf))
		}
	}
	flush(t, w)
	if strings.Trim(out.String(), "*") != "" || out.Len() == 0 {
		t.Fatalf("output is not only stars: %.40q", out.String())
	}
}

func TestSecretsThatOverlapEachOtherInAStream(t *testing.T) {
	for _, c := range []struct {
		secrets []string
		piece   string
		longest int
	}{{[]string{"ab", "ba"}, "ab", 2}, {[]string{"hunter2hunter2"}, "hunter2", 14}} {
		var out bytes.Buffer
		w := NewWriter(&out, c.secrets)
		for range 1000 {
			write(t, w, c.piece)
			if len(w.buf) >= c.longest {
				t.Fatalf("%v held %d bytes, as many as its longest secret", c.secrets, len(w.buf))
			}
		}
		flush(t, w)
		for _, s := range c.secrets {
			if strings.Contains(out.String(), s) {
				t.Fatalf("%v leaked", c.secrets)
			}
		}
	}
}

func TestAStarredSecretLeavesNoRemainder(t *testing.T) {
	got := run(t, []string{"abcd", "cdef"}, "ab", "cd", "ef")
	if strings.ContainsAny(got, "abcdef") || !strings.Contains(got, "***") {
		t.Fatalf("%q", got)
	}
}

func wholeInput(in string, secrets [][]byte) string {
	var sb strings.Builder
	at := 0
	for _, s := range spans([]byte(in), secrets, 0) {
		sb.WriteString(in[at:s[0]] + "***")
		at = s[1]
	}
	sb.WriteString(in[at:])
	return sb.String()
}

func collapse(s string) string {
	for strings.Contains(s, "**") {
		s = strings.ReplaceAll(s, "**", "*")
	}
	return s
}

func TestStreamingEqualsWholeInputMasking(t *testing.T) {
	// ChaCha8 from a fixed seed: the same cases on every run, from a
	// generator with no weakness to speak of.
	src := rand.NewChaCha8([32]byte{1, 2})
	intN := func(n int) int { // n is never above 40, far under a byte
		var b [1]byte
		_, _ = src.Read(b[:])
		return int(b[0]) % n
	}
	randStr := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = "ab"[intN(2)]
		}
		return string(b)
	}
	for range 20000 {
		var secrets []string
		var raw [][]byte
		for range 1 + intN(3) {
			s := randStr(1 + intN(5))
			secrets = append(secrets, s)
			raw = append(raw, []byte(s))
		}
		in := randStr(intN(40))
		var out bytes.Buffer
		w := NewWriter(&out, secrets)
		for rest := in; rest != ""; {
			n := 1 + intN(len(rest))
			write(t, w, rest[:n])
			rest = rest[n:]
		}
		flush(t, w)
		for _, s := range secrets {
			if strings.Contains(out.String(), s) {
				t.Fatalf("%v %q: leaked in %q", secrets, in, out.String())
			}
		}
		if want := collapse(wholeInput(in, raw)); collapse(out.String()) != want {
			t.Fatalf("%v %q: got %q want %q", secrets, in, collapse(out.String()), want)
		}
	}
}

// The automaton finds what the search for each secret finds, in the same
// spans and with the same bytes held back.
func TestTheAutomatonFindsWhatOneSearchPerSecretFinds(t *testing.T) {
	src := rand.NewChaCha8([32]byte{3, 4})
	intn := func(n int) int { // n is never above 40, far under a byte
		var b [1]byte
		_, _ = src.Read(b[:])
		return int(b[0]) % n
	}
	word := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = "ab"[intn(2)]
		}
		return b
	}
	for range 300 {
		var secrets [][]byte
		for range 1 + intn(8) {
			secrets = append(secrets, word(1+intn(6)))
		}
		a := newAutomaton(secrets)
		buf := word(intn(40))
		covered := intn(2) * min(len(buf), 2)
		if got, want := a.spans(buf, covered), spans(buf, secrets, covered); !slices.Equal(got, want) {
			t.Fatalf("%q in %q: spans %v, want %v", secrets, buf, got, want)
		}
		if got, want := a.heldFrom(buf), heldFrom(buf, secrets); got != want {
			t.Fatalf("%q in %q: held from %d, want %d", secrets, buf, got, want)
		}
	}
}

// A file secret of a megabyte gives thousands of lines to look for; the
// log of a stage that prints it whole is still masked in a moment.
func TestAThousandsOfLinesFileIsMaskedWholeAndFast(t *testing.T) {
	var lines []string
	for i := range 8000 {
		lines = append(lines, fmt.Sprintf("line-%06d-%s", i, strings.Repeat("k", 50)))
	}
	var out bytes.Buffer
	w := NewWriter(&out, lines)
	start := time.Now()
	for _, l := range lines {
		_, _ = w.Write([]byte(l + "\n"))
	}
	_ = w.Flush()
	if strings.Contains(out.String(), "line-") {
		t.Fatalf("a line leaked: %.80q", out.String())
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("masking took %v", took)
	}
}

func BenchmarkOneSecret(b *testing.B) { benchmarkMask(b, []string{"s3cr3t-value-0001"}) }

func BenchmarkAFileOfThousandsOfLines(b *testing.B) {
	var lines []string
	for i := range 8000 {
		lines = append(lines, fmt.Sprintf("line-%06d-%s", i, strings.Repeat("k", 50)))
	}
	benchmarkMask(b, lines)
}

// The throughput of masking, the automaton built once: the file of thousands
// of lines must stay within half of what one secret gives.
func benchmarkMask(b *testing.B, secrets []string) {
	chunk := []byte(strings.Repeat("an ordinary line of output, nothing to hide\n", 200))
	b.SetBytes(int64(len(chunk)))
	w := NewWriter(io.Discard, secrets)
	for b.Loop() {
		_, _ = w.Write(chunk)
		_ = w.Flush()
	}
}
