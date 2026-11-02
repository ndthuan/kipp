// Package logtime keeps when each line of a log began, in a file beside
// the log, so the log itself stays as the build wrote it. The file is
// <log>.times: one record a line, the offset just past the line's end and
// the Unix time in milliseconds at which its first byte was written, each
// a big-endian int64, in the order the lines were written.
package logtime

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"sort"
	"time"
)

// Suffix makes a log's path the path of its times.
const Suffix = ".times"

const recordSize = 16

// Writer passes what is written on to a log and records the lines it
// ends. A line's record goes out before the line does, so whoever reads a
// line from the log finds its time already there.
type Writer struct {
	log, times io.Writer
	offset     int64 // the log's size: where the next byte lands
	inLine     bool  // the bytes since the last newline began a line
	start      time.Time
	now        func() time.Time
}

// NewWriter appends to log, which is offset bytes long, and records to
// times.
func NewWriter(log io.Writer, offset int64, times io.Writer) *Writer {
	return &Writer{log: log, times: times, offset: offset, now: time.Now}
}

// Write implements io.Writer. A failure to record a time is returned, as
// the log's own failure is: the stage's output is lost either way.
func (w *Writer) Write(p []byte) (int, error) {
	var recs []byte
	rest, at := p, w.offset
	for len(rest) > 0 {
		if !w.inLine {
			w.inLine, w.start = true, w.now()
		}
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			break
		}
		at += int64(i) + 1
		recs = appendRecord(recs, at, w.start)
		rest, w.inLine = rest[i+1:], false
	}
	if len(recs) > 0 {
		if _, err := w.times.Write(recs); err != nil {
			return 0, err
		}
	}
	n, err := w.log.Write(p)
	w.offset += int64(n)
	return n, err
}

// Close records a last line the log was left without a newline on.
func (w *Writer) Close() error {
	if !w.inLine {
		return nil
	}
	w.inLine = false
	_, err := w.times.Write(appendRecord(nil, w.offset, w.start))
	return err
}

func appendRecord(b []byte, end int64, t time.Time) []byte {
	b, _ = binary.Append(b, binary.BigEndian, [2]int64{end, t.UnixMilli()}) // a fixed-size value always encodes
	return b
}

type record struct{ end, ms int64 }

// batch is how many records one read brings in.
const batch = 512

// Reader finds the times of a log's lines, asked for in the order they
// stand in the log, while the file may still be growing. A file that is
// not there yet, or that cannot be read, has no times; the log goes on
// without them.
type Reader struct {
	open   func() (*os.File, error)
	f      *os.File
	placed bool     // the first line asked for has been looked up
	next   int64    // the index of the record to look at next
	buf    []record // records from first on
	first  int64
}

// NewReader reads the times file that open opens, when it is first needed.
func NewReader(open func() (*os.File, error)) *Reader { return &Reader{open: open} }

// Close closes the file.
func (r *Reader) Close() {
	if r.f != nil {
		_ = r.f.Close()
	}
}

// At is when the line that ends at end began, if that was recorded. end
// does not go down from one call to the next.
func (r *Reader) At(end int64) (time.Time, bool) {
	if !r.ready() {
		return time.Time{}, false
	}
	if !r.placed {
		r.placed, r.next = true, r.search(end)
	}
	for {
		rec, ok := r.record(r.next)
		switch {
		case !ok || rec.end > end:
			return time.Time{}, false
		case rec.end == end:
			r.next++
			return time.UnixMilli(rec.ms), true
		}
		r.next++
	}
}

func (r *Reader) ready() bool {
	if r.f == nil {
		f, err := r.open()
		if err != nil {
			return false
		}
		r.f = f
	}
	return true
}

// search is the index of the first record that ends at or past end.
func (r *Reader) search(end int64) int64 {
	info, err := r.f.Stat()
	if err != nil {
		return 0
	}
	n := int(info.Size() / recordSize)
	return int64(sort.Search(n, func(i int) bool {
		rec, ok := r.record(int64(i))
		return !ok || rec.end >= end
	}))
}

// record is the record at index i, read with the batch after it.
func (r *Reader) record(i int64) (record, bool) {
	if i < r.first || i >= r.first+int64(len(r.buf)) {
		r.fill(i)
	}
	if k := i - r.first; k >= 0 && k < int64(len(r.buf)) {
		return r.buf[k], true
	}
	return record{}, false
}

func (r *Reader) fill(i int64) {
	raw := make([]byte, batch*recordSize)
	n, err := r.f.ReadAt(raw, i*recordSize)
	if err != nil && !errors.Is(err, io.EOF) {
		n = 0
	}
	r.first, r.buf = i, r.buf[:0]
	for k := 0; k+recordSize <= n; k += recordSize {
		var rec [2]int64
		if _, err := binary.Decode(raw[k:k+recordSize], binary.BigEndian, &rec); err != nil {
			return
		}
		r.buf = append(r.buf, record{end: rec[0], ms: rec[1]})
	}
}
