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
	maxStreams   = 16
	recheckEvery = 15 * time.Second
)

const keepAliveComment = ": keep-alive\n\n"

var (
	errSignedOut = errors.New("the session or token this stream was opened with is no longer valid")
)

// ErrTooManyStreams refuses a stream or a wait of a user who holds as many
// as one user may.
var ErrTooManyStreams = fmt.Errorf("at most %d streams may be open at once for one user", maxStreams)

// HoldStream counts a stream or a wait against the user until release is
// called, or is ErrTooManyStreams. The API's streams and its wait, and the
// waits of /mcp, count together.
func (s *Service) HoldStream(u *store.User) (release func(), err error) {
	if !s.streams.take(u.ID) {
		return nil, ErrTooManyStreams
	}
	return sync.OnceFunc(func() { s.streams.give(u.ID) }), nil
}

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

type allowed func(ctx context.Context, u *store.User) error

func (a *API) openStream(r *http.Request, u *store.User, may allowed) (ctx context.Context, done func(), err error) {
	release, err := a.svc.HoldStream(u)
	if err != nil {
		return nil, nil, err
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
		release()
	}, nil
}

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

func endedBy(ctx context.Context) error {
	cause := context.Cause(ctx)
	if cause == nil || errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return nil
	}
	return cause
}
