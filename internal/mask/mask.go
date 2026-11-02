// Package mask replaces secret values with *** on their way to a log, so a
// log file never holds one, even if the process dies mid-build.
package mask

import (
	"bytes"
	"io"
	"slices"
)

var stars = []byte("***")

// Writer masks secrets in a stream. It writes everything it can be sure of at
// once and holds back only a trailing run of bytes that could still become
// the start of a secret when the next write arrives (fewer bytes than the
// longest secret). Flush writes what is held.
type Writer struct {
	w       io.Writer
	secrets [][]byte
	ac      *automaton // set when there are too many secrets to look for one by one
	buf     []byte
	covered int // leading bytes of buf already represented by a *** that was written
}

// NewWriter masks every non-empty secret. Occurrences that overlap or touch
// are replaced by a single ***.
func NewWriter(w io.Writer, secrets []string) *Writer {
	m := &Writer{w: w}
	for _, s := range secrets {
		if s != "" {
			m.secrets = append(m.secrets, []byte(s))
		}
	}
	if len(m.secrets) > manySecrets {
		m.ac = newAutomaton(m.secrets)
	}
	return m
}

// manySecrets is how many secrets are looked for with an automaton instead
// of one search each: a file secret gives a pattern per line, and one search
// per pattern per write does not keep up with thousands of them.
const manySecrets = 16

func (m *Writer) spansOf() [][2]int {
	if m.ac != nil {
		return m.ac.spans(m.buf, m.covered)
	}
	return spans(m.buf, m.secrets, m.covered)
}

func (m *Writer) heldOf() int {
	if m.ac != nil {
		return m.ac.heldFrom(m.buf)
	}
	return heldFrom(m.buf, m.secrets)
}

// Write implements io.Writer. It reports len(p) and any error from the
// underlying writer; after such an error the bytes are gone, not retried.
func (m *Writer) Write(p []byte) (int, error) {
	if len(m.secrets) == 0 {
		return m.w.Write(p)
	}
	m.buf = append(m.buf, p...)
	return len(p), m.emit(m.heldOf())
}

// Flush writes the held bytes, masked, and empties the buffer.
func (m *Writer) Flush() error {
	err := m.emit(len(m.buf))
	m.covered = 0
	return err
}

// emit writes buf[:upto] with every span replaced by ***, except the span that
// holds the already-written prefix, and drops it from the buffer whether or
// not the write succeeded.
func (m *Writer) emit(upto int) error {
	if upto == 0 {
		return nil
	}
	out := make([]byte, 0, upto)
	at, covered := 0, 0
	for _, s := range m.spansOf() {
		if s[0] >= upto {
			break
		}
		out = append(out, m.buf[at:s[0]]...)
		if s[0] != 0 || m.covered == 0 {
			out = append(out, stars...)
		}
		at = min(s[1], upto)
		if s[1] > upto {
			covered = s[1] - upto
		}
	}
	out = append(out, m.buf[at:upto]...)
	m.buf = append(m.buf[:0], m.buf[upto:]...)
	m.covered = covered
	_, err := m.w.Write(out)
	return err
}

// spans returns the merged [start, end) intervals of every complete secret in
// buf, plus [0, covered) when that prefix is already starred.
func spans(buf []byte, secrets [][]byte, covered int) [][2]int {
	var found [][2]int
	if covered > 0 {
		found = append(found, [2]int{0, covered})
	}
	for _, s := range secrets {
		for from := 0; ; {
			i := bytes.Index(buf[from:], s)
			if i < 0 {
				break
			}
			found = append(found, [2]int{from + i, from + i + len(s)})
			from += i + 1
		}
	}
	slices.SortFunc(found, func(a, b [2]int) int { return a[0] - b[0] })
	var merged [][2]int
	for _, f := range found {
		if n := len(merged); n > 0 && f[0] <= merged[n-1][1] {
			merged[n-1][1] = max(merged[n-1][1], f[1])
		} else {
			merged = append(merged, f)
		}
	}
	return merged
}

// heldFrom is where the held tail starts: the longest suffix of buf that is a
// proper prefix of a secret.
func heldFrom(buf []byte, secrets [][]byte) int {
	hold := len(buf)
	for _, s := range secrets {
		for k := min(len(s)-1, len(buf)); k > 0 && len(buf)-k < hold; k-- {
			if bytes.HasSuffix(buf, s[:k]) {
				hold = len(buf) - k
			}
		}
	}
	return hold
}
