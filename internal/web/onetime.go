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

// A form whose answer shows a value once (a token, a generated password)
// carries a one-time value, and its change is made for that value once. A
// reload or the back button sends the form again: without it, each would
// make another token or another password, and the one shown first would
// be lost with nobody the wiser.
const (
	// oneTimeLife is how long a page's form may wait to be sent.
	oneTimeLife = time.Hour
	// oneTimePerSession is how many unsent forms a session keeps: the
	// newest, enough for the tabs a person has open.
	oneTimePerSession = 32
	// oneTimeSessions bounds the table. The values are in memory, as the
	// throttle's counts are, so it holds no more than this many sessions,
	// and a restart forgets them: a page from before is sent once more
	// after a reload.
	oneTimeSessions = 10_000
)

// oneTimeForms holds the one-time values given out and not yet used, by
// session. It is safe for concurrent use.
type oneTimeForms struct {
	mu  sync.Mutex
	now func() time.Time
	// given is by session id, then by value, when it was given.
	given map[string]map[string]time.Time
}

func newOneTimeForms(now func() time.Time) *oneTimeForms {
	return &oneTimeForms{now: now, given: map[string]map[string]time.Time{}}
}

// give makes a one-time value for a form of session's.
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

// makeRoom keeps the table within oneTimeSessions before a session is
// added: sessions whose values are all past are dropped first, then any
// one, whose unsent forms are then answered as sent already.
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

// take uses v for session: true once, for a value given to it and not past.
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

// giveBack makes v good again for session: its form made nothing.
func (o *oneTimeForms) giveBack(session, v string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if values, ok := o.given[session]; ok {
		values[v] = o.now()
	}
}

// forget drops the values past their life.
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

// statusWriter keeps the status a handler answered with.
type statusWriter struct {
	http.ResponseWriter
	code int
}

// WriteHeader keeps code and sends it on.
func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

// oneTime runs h, a form whose answer shows a value once, for a one-time
// value its session was given and has not used. Any other is answered
// that the form was sent already, and h does not run; but whoever may not
// make an admin's change at all is told that, as the change would tell
// them. The value is used before h runs, so that two sendings at once
// cannot both pass, and given back when h answers anything but the value
// (a refusal, the password form), since then nothing was made.
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
