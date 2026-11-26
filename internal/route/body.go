package route

import (
	"io"
	"net/http"
	"sync"
	"time"
)

// BodyWithin gives each request of next the time d to send its body, so
// that a body bounded in size is bounded in time too: a read past it fails,
// and so does the server's own reading of what a handler left unread, which
// closes the connection. The time ends with the body, so an answer may take
// as long as it likes once the body was read; a route whose body is a long
// upload or a stream is not to be wrapped.
func BodyWithin(d time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.Body == http.NoBody {
			next.ServeHTTP(w, r)
			return
		}
		rc := http.NewResponseController(w)
		if err := rc.SetReadDeadline(time.Now().Add(d)); err != nil {
			next.ServeHTTP(w, r)
			return
		}
		r2 := *r
		r2.Body = &timedBody{ReadCloser: r.Body, done: sync.OnceFunc(func() { _ = rc.SetReadDeadline(time.Time{}) })}
		next.ServeHTTP(w, &r2)
	})
}

type timedBody struct {
	io.ReadCloser
	done func()
}

// Read reads the body, and ends its time at the body's end.
func (b *timedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.done()
	}
	return n, err
}
