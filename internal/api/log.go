package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/logtime"
	"github.com/ndthuan/kipp/internal/store"
)

// logChunk is the most of a log one read returns, and so the most of it
// held in memory for one request.
const logChunk = 1 << 20

// logQuery is what a request for a log asks for: the two flags are true or
// false and the offset a whole number, and anything else is refused.
type logQuery struct {
	download, timestamps bool
	offset               int64
}

func readLogQuery(r *http.Request) (q logQuery, err error) {
	if q.download, err = flagOf(r, "download"); err != nil {
		return q, err
	}
	if q.timestamps, err = flagOf(r, "timestamps"); err != nil {
		return q, err
	}
	q.offset, err = offsetOf(r)
	return q, err
}

// log serves a stage's console in one of three forms: one chunk from
// ?offset=, with the log's size beside it (a HEAD is the size alone); with
// ?download=true the whole log as a file; or, at /log/events, every line
// as it is written until the build's logs are complete, as "id:" and "data:" events closed by "event: end". With
// ?timestamps=true the download begins each line with when it began, and the
// stream sends that before each line as "event: time", in RFC 3339 in UTC; a
// line kipp kept no time for has none.
func (a *API) log(w http.ResponseWriter, r *http.Request, u *store.User) {
	a.serveLog(w, r, u, false)
}

// logEvents is the stream of a stage's log: every line as it is written.
func (a *API) logEvents(w http.ResponseWriter, r *http.Request, u *store.User) {
	a.serveLog(w, r, u, true)
}

func (a *API) serveLog(w http.ResponseWriter, r *http.Request, u *store.User, events bool) {
	id, err := BuildID(r)
	var q logQuery
	if err == nil {
		q, err = readLogQuery(r)
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	stage := r.PathValue("stage")
	path, b, err := a.svc.LogPath(r.Context(), u, id, stage)
	if err == nil { // a stage the build does not have is not found
		err = a.svc.HasStage(r.Context(), u, id, stage)
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	sealed(w)
	switch {
	case events:
		if err := drain(r); err != nil {
			a.fail(w, r, err)
			return
		}
		ctx, done, err := a.openStream(r, u, func(ctx context.Context, u *store.User) error {
			_, _, err := a.svc.LogPath(ctx, u, id, stage)
			return err
		})
		if err != nil {
			a.fail(w, r, err)
			return
		}
		defer done()
		a.streamLog(ctx, w, r, path, id, q)
	case q.download:
		// Job and stage are names config took: nothing in them to quote.
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%d-%s.log"`, b.Job, b.Number, stage))
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if q.timestamps {
			downloadStamped(w, path)
		} else {
			download(w, path)
		}
	default:
		a.chunk(w, r, path, b, q.offset)
	}
}

func (a *API) chunk(w http.ResponseWriter, r *http.Request, path string, b *store.Build, offset int64) {
	limit := int64(logChunk)
	if r.Method == http.MethodHead {
		limit = 0
	}
	complete := a.svc.complete(b)
	data, next, size := readFrom(path, offset, limit)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Kipp-Offset", strconv.FormatInt(next, 10))
	w.Header().Set("X-Kipp-Size", strconv.FormatInt(size, 10))
	w.Header().Set("X-Kipp-More", strconv.FormatBool(size > next || !complete))
	_, _ = w.Write(data)
}

// readFrom returns up to limit bytes of the file from offset, the offset
// after them and the size of the file once they were read. A log that is
// not there yet is empty.
func readFrom(path string, offset, limit int64) (data []byte, next, size int64) {
	offset = max(offset, 0)
	f, err := dispatch.OpenLog(path, os.O_RDONLY)
	if err != nil {
		return nil, offset, 0
	}
	defer func() { _ = f.Close() }()
	data, _ = io.ReadAll(io.NewSectionReader(f, offset, limit))
	next = offset + int64(len(data))
	if info, err := f.Stat(); err == nil {
		size = info.Size()
	}
	return data, next, size
}

// download copies the log as it is now, a buffer at a time, however long
// it is. A log that is not there yet is empty.
func download(w io.Writer, path string) {
	f, err := dispatch.OpenLog(path, os.O_RDONLY)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = io.Copy(w, f) // the client leaving is the only failure, and nobody is left to tell
}

// times reads when the lines of the log at path began.
func times(path string) *logtime.Reader {
	return logtime.NewReader(func() (*os.File, error) { return dispatch.OpenLog(path+logtime.Suffix, os.O_RDONLY) })
}

// lineStamp is when a line began as a download and a stream tell it.
func lineStamp(began time.Time) string { return began.UTC().Format(millisLayout) }

// downloadStamped copies the log a line at a time, each with when it
// began in front. A line
// longer than logChunk is sent as it is, unstamped, so a download never
// holds more than that.
func downloadStamped(w io.Writer, path string) {
	f, err := dispatch.OpenLog(path, os.O_RDONLY)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	t := times(path)
	defer t.Close()
	in := bufio.NewReaderSize(f, logChunk)
	var at int64
	whole := true // the read begins a line
	for {
		line, err := in.ReadSlice('\n')
		at += int64(len(line))
		if whole && len(line) > 0 && !errors.Is(err, bufio.ErrBufferFull) {
			if began, ok := t.At(at); ok {
				line = append([]byte(lineStamp(began)+" "), line...)
			}
		}
		if _, werr := w.Write(line); werr != nil {
			return // the client left, and nobody is left to tell
		}
		whole = !errors.Is(err, bufio.ErrBufferFull)
		if err != nil && whole {
			return
		}
	}
}

// logStream is one client's place in a log.
type logStream struct {
	w       io.Writer
	flush   func() error
	path    string
	offset  int64           // how far the file has been read
	partial string          // read and not sent: a line whose end is not written yet
	wrote   time.Time       // when the client last heard anything
	times   *logtime.Reader // nil when the client did not ask for them
}

// streamStart is where a stream begins: after the last line the client
// says it has (a browser sends Last-Event-ID when it reconnects by itself),
// or at ?offset=.
func streamStart(r *http.Request, offset int64) int64 {
	if from := r.Header.Get("Last-Event-ID"); from != "" {
		offset, _ = strconv.ParseInt(from, 10, 64)
	}
	return max(offset, 0)
}

func (s *logStream) write(text string) error {
	s.wrote = time.Now()
	_, err := io.WriteString(s.w, text)
	return err
}

// send writes one line as data events, the last with id, the file offset a
// reconnecting client resumes from. A carriage return ends a line to an
// event-stream parser, so each piece between them is its own event:
// otherwise a build could print "\revent: end" and close the stream.
func (s *logStream) send(line string, id int64) error {
	if s.times != nil {
		if began, ok := s.times.At(id); ok {
			if err := s.write("event: time\ndata: " + lineStamp(began) + "\n\n"); err != nil {
				return err
			}
		}
	}
	pieces := strings.Split(strings.TrimSuffix(line, "\r"), "\r")
	last := len(pieces) - 1
	for _, piece := range pieces[:last] {
		if err := s.write("data: " + piece + "\n\n"); err != nil {
			return err
		}
	}
	return s.write(fmt.Sprintf("id: %d\ndata: %s\n\n", id, pieces[last]))
}

// drain sends every complete line the file holds past the stream's place.
func (s *logStream) drain() error {
	for {
		at := s.offset - int64(len(s.partial)) // where the first unsent line starts
		data, next, _ := readFrom(s.path, s.offset, logChunk)
		if len(data) == 0 {
			return nil
		}
		s.offset = next
		lines := strings.Split(s.partial+string(data), "\n")
		s.partial = lines[len(lines)-1]
		for _, line := range lines[:len(lines)-1] {
			at += int64(len(line)) + 1
			if err := s.send(line, at); err != nil {
				return err
			}
		}
		if len(s.partial) >= logChunk { // a line this long goes out in pieces
			if err := s.sendPartial(); err != nil {
				return err
			}
		}
	}
}

func (s *logStream) sendPartial() error {
	line := s.partial
	s.partial = ""
	return s.send(line, s.offset)
}

// finish sends what the build left without a newline, then the end event.
func (s *logStream) finish() {
	if s.partial != "" && s.sendPartial() != nil {
		return
	}
	if s.write("event: end\ndata: \n\n") == nil {
		_ = s.flush()
	}
}

// breathe flushes what was sent, after a comment line if the client has
// heard nothing for a while: a proxy closes a stream it takes for dead.
func (s *logStream) breathe(every time.Duration) error {
	if time.Since(s.wrote) >= every {
		if err := s.write(keepAliveComment); err != nil {
			return err
		}
	}
	return s.flush()
}

// streamLog follows the log until it is complete or the client leaves. If
// the build's status cannot be read the stream just closes, with no end
// event: the log may not be complete, and a client that reconnects resumes
// from the last id it has.
func (a *API) streamLog(ctx context.Context, w http.ResponseWriter, r *http.Request, path string, id int64, q logQuery) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	s := &logStream{w: w, flush: http.NewResponseController(w).Flush, path: path, offset: streamStart(r, q.offset), wrote: time.Now()}
	if q.timestamps {
		s.times = times(path)
		defer s.times.Close()
	}
	tick := time.NewTicker(a.logPoll)
	defer tick.Stop()
	for {
		b, err := a.deps.Store.Build(ctx, id)
		if err != nil {
			if ctx.Err() == nil {
				a.deps.Logger.Error("api: a log stream lost its build", "build", id, "err", err)
			}
			return
		}
		complete := a.svc.complete(b)
		if s.drain() != nil {
			return
		}
		if complete {
			s.finish()
			return
		}
		if s.breathe(a.keepAlive) != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
