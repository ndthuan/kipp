package api

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/store"
	"github.com/ndthuan/kipp/internal/waitfor"
)

// openStream opens path as an event stream with hdr and returns its status
// and a channel that is closed when the server ends the stream.
func (e *env) openStream(t *testing.T, tok, path string, hdr ...string) (int, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req := e.request(ctx, t, tok, "GET", path, nil, append([]string{"Accept", sseAccept}, hdr...)...)
	res, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	must(t, err)
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		_, _ = io.Copy(io.Discard, res.Body)
	}()
	t.Cleanup(func() { cancel(); _ = res.Body.Close() })
	return res.StatusCode, ended
}

// endsSoon fails unless the stream ends within a time far longer than the
// re-check takes.
func endsSoon(t *testing.T, what string, ended <-chan struct{}) {
	t.Helper()
	select {
	case <-ended:
	case <-time.After(waitfor.Safety):
		t.Fatalf("%s: the stream did not end", what)
	}
}

// staysOpen fails if the stream ends within a few re-checks.
func staysOpen(t *testing.T, what string, ended <-chan struct{}) {
	t.Helper()
	select {
	case <-ended:
		t.Fatalf("%s: the stream ended", what)
	case <-time.After(200 * time.Millisecond):
	}
}

func quickRecheck(a *API) { a.recheck = 20 * time.Millisecond }

func TestAUserHasAtMost16StreamsOpenAndThe17thIs429(t *testing.T) {
	e := newEnv(t)
	for i := range maxStreams {
		code, _ := e.openStream(t, e.viewer, "/api/v1/queue/events")
		eq(t, "stream "+strconv.Itoa(i+1), code, 200)
	}
	code, _ := e.openStream(t, e.viewer, "/api/v1/queue/events")
	eq(t, "the 17th queue stream", code, http.StatusTooManyRequests)
	id := e.trigger(t, "smoke", "")
	code, _ = e.openStream(t, e.viewer, "/api/v1/builds/"+id+"/stages/s/log/events")
	eq(t, "a log stream beyond 16", code, http.StatusTooManyRequests)
	text, _ := e.call(t, http.StatusTooManyRequests, e.viewer, "GET", "/api/v1/builds/"+id+"?wait=true", "")
	contains(t, text, "streams")
	code, _ = e.openStream(t, e.agent, "/api/v1/queue/events")
	eq(t, "another user's stream", code, 200)
}

func TestAClosedStreamGivesItsPlaceBack(t *testing.T) {
	e := newEnv(t)
	cancels := make([]context.CancelFunc, 0, maxStreams)
	for range maxStreams {
		ctx, cancel := context.WithCancel(context.Background())
		res, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(e.request(ctx, t, e.viewer, "GET", "/api/v1/queue/events", nil, "Accept", sseAccept))
		must(t, err)
		eq(t, "status", res.StatusCode, 200)
		t.Cleanup(func() { _ = res.Body.Close() })
		cancels = append(cancels, cancel)
	}
	cancels[0]()
	eventually(t, func() bool {
		code, _ := e.openStream(t, e.viewer, "/api/v1/queue/events")
		return code == 200
	})
	for _, c := range cancels {
		c()
	}
}

func TestAStreamEndsOnceItsTokenIsRevoked(t *testing.T) {
	e := newEnv(t, quickRecheck)
	tok := token(t, e.st, "short", false, map[string][]string{"smoke": {"view"}})
	code, ended := e.openStream(t, tok, "/api/v1/queue/events")
	eq(t, "status", code, 200)
	staysOpen(t, "while the token holds", ended)
	toks, err := e.st.Tokens(context.Background(), e.user(t, "short").ID)
	must(t, err)
	must(t, e.st.DeleteToken(context.Background(), toks[0].ID))
	endsSoon(t, "after the token is revoked", ended)
}

func TestAStreamEndsOnceItsSessionIsGone(t *testing.T) {
	e := newEnv(t, quickRecheck)
	sess := &store.Session{ID: "sess-stream", UserID: e.user(t, "viewer").ID, CSRF: "c", ExpiresAt: time.Now().Add(time.Hour)}
	must(t, e.st.CreateSession(context.Background(), sess))
	code, ended := e.openStream(t, "", "/api/v1/queue/events", "Cookie", SessionCookieFor(sess).String())
	eq(t, "status", code, 200)
	staysOpen(t, "while the session holds", ended)
	must(t, e.st.DeleteSession(context.Background(), sess.ID))
	endsSoon(t, "after the session is gone", ended)
}

func TestALogStreamEndsOnceItsUserMayNoLongerViewTheJob(t *testing.T) {
	e := newEnv(t, quickRecheck)
	tok := token(t, e.st, "watcher", false, map[string][]string{"slow": {"view"}})
	id := e.started(t, "slow")
	code, ended := e.openStream(t, tok, "/api/v1/builds/"+id+"/stages/s/log/events")
	eq(t, "status", code, 200)
	staysOpen(t, "while the grant holds", ended)
	must(t, e.st.ReplaceGrants(context.Background(), e.user(t, "watcher").ID, map[string]store.Perm{}))
	endsSoon(t, "after the grant is gone", ended)
	e.call(t, 202, e.admin, "POST", "/api/v1/builds/"+id+"/cancel", "")
}

func TestAQueueStreamEndsOnceItsUserIsNoLongerAnAdmin(t *testing.T) {
	e := newEnv(t, quickRecheck)
	tok := token(t, e.st, "boss", true, nil)
	code, ended := e.openStream(t, tok, "/api/v1/queue/events")
	eq(t, "status", code, 200)
	staysOpen(t, "while an admin", ended)
	must(t, e.st.SetAdmin(context.Background(), e.user(t, "boss").ID, false))
	endsSoon(t, "after admin is taken away", ended)
}

func TestALongPollEndsWith401OnceItsTokenIsRevoked(t *testing.T) {
	e := newEnv(t, quickRecheck)
	tok := token(t, e.st, "poller", false, map[string][]string{"slow": {"view"}})
	id := e.started(t, "slow")
	toks, err := e.st.Tokens(context.Background(), e.user(t, "poller").ID)
	must(t, err)
	var (
		code int
		text string
	)
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		code, text, _ = e.raw(t, tok, "GET", "/api/v1/builds/"+id+"?wait=true", "")
	}()
	// Revoked while it waits: the build's own poll is the one handler open.
	eventually(t, func() bool { return e.active.Load() == 1 })
	must(t, e.st.DeleteToken(context.Background(), toks[0].ID))
	select {
	case <-answered:
	case <-time.After(waitfor.Safety):
		t.Fatal("the long-poll did not end")
	}
	if code != http.StatusUnauthorized {
		t.Fatalf("the long-poll ended %d: %s", code, text)
	}
	e.call(t, 202, e.admin, "POST", "/api/v1/builds/"+id+"/cancel", "")
}
