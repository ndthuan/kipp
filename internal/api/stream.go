package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

const (
	// maxStreams is how many streams one user may hold open at once: the
	// log streams, the queue's events and the long-polls. Each holds a
	// connection and a goroutine for as long as it lasts.
	maxStreams = 16
	// recheckEvery is how often an open stream asks again whether its
	// session or token, and what it shows, are still allowed.
	recheckEvery = 15 * time.Second
)

// keepAliveComment is the line both event streams send when they have
// nothing to say, so that a proxy does not take them for dead.
const keepAliveComment = ": keep-alive\n\n"

var (
	// errTooManyStreams refuses a stream beyond maxStreams.
	errTooManyStreams = fmt.Errorf("at most %d streams may be open at once for one user", maxStreams)
	// errSignedOut ends a stream whose session or token is gone, or whose
	// user is no longer the one it was opened for.
	errSignedOut = errors.New("the session or token this stream was opened with is no longer valid")
)

// streams counts the open streams of each user.
type streams struct {
	mu   sync.Mutex
	open map[int64]int
}

func (s *streams) take(user int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open[user] >= maxStreams {
		return false
	}
	if s.open == nil {
		s.open = map[int64]int{}
	}
	s.open[user]++
	return true
}

func (s *streams) give(user int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open[user]--; s.open[user] <= 0 {
		delete(s.open, user)
	}
}

// allowed says whether u may still have what a stream shows; nil for a
// stream whose every answer is checked as it is made.
type allowed func(ctx context.Context, u *store.User) error

// openStream takes one of u's places for a stream that answers r. Its
// context ends, with the reason as its cause, once r's session or token
// is gone, its user has changed, or may returns an error. done ends it
// and gives the place back.
func (a *API) openStream(r *http.Request, u *store.User, may allowed) (ctx context.Context, done func(), err error) {
	if !a.streams.take(u.ID) {
		return nil, nil, errTooManyStreams
	}
	ctx, cancel := context.WithCancelCause(r.Context())
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		a.watch(ctx, cancel, r, u, may)
	}()
	return ctx, func() {
		cancel(nil)
		<-watched
		a.streams.give(u.ID)
	}, nil
}

// watch re-checks the stream every a.recheck until it ends.
func (a *API) watch(ctx context.Context, cancel context.CancelCauseFunc, r *http.Request, u *store.User, may allowed) {
	tick := time.NewTicker(a.recheck)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if err := a.reauthorize(ctx, r, u, may); err != nil {
			cancel(err)
			return
		}
	}
}

// reauthorize identifies r again as it was at first, and asks may. The
// user must be the same one, admin or not as before: a stream of the
// queue tells an admin of every job.
func (a *API) reauthorize(ctx context.Context, r *http.Request, u *store.User, may allowed) error {
	now, _, err := a.who(r.WithContext(ctx))
	if err != nil {
		return err
	}
	if now == nil || now.ID != u.ID || now.Admin != u.Admin {
		return errSignedOut
	}
	if may == nil {
		return nil
	}
	return may(ctx, now)
}

// endedBy is why a stream's context ended when the stream was refused,
// nil when it ended because the client left or the stream was done.
func endedBy(ctx context.Context) error {
	cause := context.Cause(ctx)
	if cause == nil || errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return nil
	}
	return cause
}
