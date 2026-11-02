package logtime

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// clock is a time the test moves by hand.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }
func (c *clock) tick()          { c.t = c.t.Add(time.Second) }

func newClock() *clock { return &clock{t: time.UnixMilli(1_700_000_000_000)} }

func write(t *testing.T, log, times *bytes.Buffer, c *clock, offset int64, writes ...string) {
	t.Helper()
	w := NewWriter(log, offset, times)
	w.now = c.now
	for _, p := range writes {
		if _, err := w.Write([]byte(p)); err != nil {
			t.Fatal(err)
		}
		c.tick()
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func readerOf(t *testing.T, times []byte) *Reader {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.log.times")
	if err := os.WriteFile(path, times, 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewReader(func() (*os.File, error) { return os.Open(path) })
	t.Cleanup(r.Close)
	return r
}

func at(t *testing.T, r *Reader, end int64) (int64, bool) {
	t.Helper()
	got, ok := r.At(end)
	if !ok {
		return 0, false
	}
	return got.UnixMilli(), true
}

// A line's time is when its first byte was written, however many writes
// it took, and the log itself is what was written, untouched.
func TestALineIsStampedWithWhenItBegan(t *testing.T) {
	var log, times bytes.Buffer
	c := newClock()
	start := c.t.UnixMilli()
	write(t, &log, &times, c, 0, "one\ntw", "o\nthree\n", "tail")
	if log.String() != "one\ntwo\nthree\ntail" {
		t.Fatalf("the log reads %q", log.String())
	}
	r := readerOf(t, times.Bytes())
	for _, want := range []struct {
		end, ms int64
	}{{4, start}, {8, start}, {14, start + 1000}, {18, start + 2000}} {
		got, ok := at(t, r, want.end)
		if !ok || got != want.ms {
			t.Fatalf("the line ending at %d: %d %v, want %d", want.end, got, ok, want.ms)
		}
	}
}

// A writer that appends to a log carries on from the log's size.
func TestAnAppendCarriesOnFromTheSizeOfTheLog(t *testing.T) {
	var log, times bytes.Buffer
	c := newClock()
	write(t, &log, &times, c, 0, "a\n")
	write(t, &log, &times, c, int64(log.Len()), "bb\n")
	r := readerOf(t, times.Bytes())
	if _, ok := at(t, r, 2); !ok {
		t.Fatal("the first line has no time")
	}
	if got, ok := at(t, r, 5); !ok || got != c.t.UnixMilli()-1000 {
		t.Fatalf("the appended line: %d %v", got, ok)
	}
}

// A stream may start anywhere in a log, and a line with no record (one
// written before times were kept) has no time; the lines after it do.
func TestTheReaderFindsALineFromAnywhereAndSkipsWhatIsNotRecorded(t *testing.T) {
	var log, times bytes.Buffer
	c := newClock()
	var lines []string
	for range 1000 {
		lines = append(lines, "line\n")
	}
	write(t, &log, &times, c, 0, lines...)
	r := readerOf(t, times.Bytes())
	if got, ok := at(t, r, 5*600); !ok || got != newClock().t.UnixMilli()+599_000 {
		t.Fatalf("line 600: %d %v", got, ok)
	}
	if _, ok := at(t, r, 5*600+3); ok {
		t.Fatal("an offset that ends no line has a time")
	}
	if got, ok := at(t, r, 5*601); !ok || got != newClock().t.UnixMilli()+600_000 {
		t.Fatalf("line 601 after a miss: %d %v", got, ok)
	}
	if _, ok := at(t, r, 5*2000); ok {
		t.Fatal("a line past the end has a time")
	}
}

// The file grows while a build runs: what was not there at one call is
// found at the next.
func TestTheReaderSeesRecordsWrittenAfterItLooked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.log.times")
	r := NewReader(func() (*os.File, error) { return os.Open(path) })
	t.Cleanup(r.Close)
	if _, ok := r.At(2); ok {
		t.Fatal("a time from no file")
	}
	var log, times bytes.Buffer
	write(t, &log, &times, newClock(), 0, "a\n", "b\n")
	if err := os.WriteFile(path, times.Bytes()[:recordSize], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.At(2); !ok {
		t.Fatal("the first line, once recorded")
	}
	if _, ok := r.At(4); ok {
		t.Fatal("the second line before it was recorded")
	}
	if err := os.WriteFile(path, times.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.At(4); !ok {
		t.Fatal("the second line, once recorded")
	}
}
