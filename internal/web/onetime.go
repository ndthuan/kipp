package web

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/store"
)

const (
	oneTimeLife       = time.Hour
	oneTimePerSession = 32
	oneTimeSessions   = 10_000
)

type oneTimeForms struct {
	mu    sync.Mutex
	now   func() time.Time
	given map[string]map[string]time.Time
}

func newOneTimeForms(now func() time.Time) *oneTimeForms {
	return &oneTimeForms{now: now, given: map[string]map[string]time.Time{}}
}

func (o *oneTimeForms) give(session string) (string, error) {
	v, err := auth.NewID()
	if err != nil {
		return "", err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	now := o.now()
	values, ok := o.given[session]
	if !ok {
		o.makeRoom(now)
		values = map[string]time.Time{}
		o.given[session] = values
	}
	forget(values, now)
	for len(values) >= oneTimePerSession {
		delete(values, oldest(values))
	}
	values[v] = now
	return v, nil
}

func (o *oneTimeForms) makeRoom(now time.Time) {
	if len(o.given) < oneTimeSessions {
		return
	}
	for session, values := range o.given {
		if forget(values, now); len(values) == 0 {
			delete(o.given, session)
		}
	}
	for session := range o.given {
		if len(o.given) < oneTimeSessions {
			return
		}
		delete(o.given, session)
	}
}

func (o *oneTimeForms) take(session, v string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	at, ok := o.given[session][v]
	if !ok {
		return false
	}
	delete(o.given[session], v)
	return o.now().Sub(at) < oneTimeLife
}

func (o *oneTimeForms) giveBack(session, v string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if values, ok := o.given[session]; ok {
		values[v] = o.now()
	}
}

func forget(values map[string]time.Time, now time.Time) {
	for v, at := range values {
		if now.Sub(at) >= oneTimeLife {
			delete(values, v)
		}
	}
}

func oldest(values map[string]time.Time) string {
	var first string
	var at time.Time
	for v, t := range values {
		if first == "" || t.Before(at) {
			first, at = v, t
		}
	}
	return first
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

// WriteHeader keeps code and sends it on.
func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

func (s *Site) oneTime(h pageHandler) pageHandler {
	return func(w http.ResponseWriter, r *http.Request, u *store.User) {
		sess := api.SessionFrom(r.Context())
		v := r.PostFormValue("once")
		if !s.forms.take(sess.ID, v) {
			if strings.HasPrefix(r.Pattern, "POST /admin/") && !u.Admin {
				s.fail(w, r, api.ErrForbidden)
				return
			}
			s.errorPage(w, r, http.StatusConflict, "Sent already", "This form was sent already, or its page is too old, so nothing was made this time: "+
				"what a first sending made was shown once, on its answer, and is not shown again. To make another, open the form's page again.")
			return
		}
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		h(sw, r, u)
		if sw.code != http.StatusOK {
			s.forms.giveBack(sess.ID, v)
		}
	}
}
