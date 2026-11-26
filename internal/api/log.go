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

const logChunk = 1 << 20

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

func (a *API) log(w http.ResponseWriter, r *http.Request, u *store.User) {
	a.serveLog(w, r, u, false)
}

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
	if err == nil {
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

func download(w io.Writer, path string) {
	f, err := dispatch.OpenLog(path, os.O_RDONLY)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = io.Copy(w, f)
}

func times(path string) *logtime.Reader {
	return logtime.NewReader(func() (*os.File, error) { return dispatch.OpenLog(path+logtime.Suffix, os.O_RDONLY) })
}

func lineStamp(began time.Time) string { return began.UTC().Format(millisLayout) }

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
	whole := true
	for {
		line, err := in.ReadSlice('\n')
		at += int64(len(line))
		if whole && len(line) > 0 && !errors.Is(err, bufio.ErrBufferFull) {
			if began, ok := t.At(at); ok {
				line = append([]byte(lineStamp(began)+" "), line...)
			}
		}
		if _, werr := w.Write(line); werr != nil {
			return
		}
		whole = !errors.Is(err, bufio.ErrBufferFull)
		if err != nil && whole {
			return
		}
	}
}

type logStream struct {
	w       io.Writer
	flush   func() error
	path    string
	offset  int64
	partial string
	wrote   time.Time
	times   *logtime.Reader
}

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

func (s *logStream) drain() error {
	for {
		at := s.offset - int64(len(s.partial))
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
		if len(s.partial) >= logChunk {
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

func (s *logStream) finish() {
	if s.partial != "" && s.sendPartial() != nil {
		return
	}
	if s.write("event: end\ndata: \n\n") == nil {
		_ = s.flush()
	}
}

func (s *logStream) breathe(every time.Duration) error {
	if time.Since(s.wrote) >= every {
		if err := s.write(keepAliveComment); err != nil {
			return err
		}
	}
	return s.flush()
}

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
