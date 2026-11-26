package oauth

import (
	"sync"
	"time"
)

const (
	registerBurst = 10
	registerEvery = time.Minute
	rateKeys      = 10_000
	rateFullWait  = 2 * time.Second
)

type rate struct {
	mu    sync.Mutex
	now   func() time.Time
	burst int
	every time.Duration
	limit int
	next  map[string]time.Time
}

func newRate(now func() time.Time, burst int, every time.Duration) *rate {
	return &rate{now: now, burst: burst, every: every, limit: rateKeys, next: map[string]time.Time{}}
}

func (l *rate) take(key string) (wait time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	at, known := l.next[key]
	if !known && len(l.next) >= l.limit && !l.sweep(now) {
		return rateFullWait, false
	}
	if at.Before(now) {
		at = now
	}
	if wait := at.Sub(now) - time.Duration(l.burst-1)*l.every; wait > 0 {
		return wait, false
	}
	l.next[key] = at.Add(l.every)
	return 0, true
}

func (l *rate) refund(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	at, known := l.next[key]
	if !known {
		return
	}
	if at = at.Add(-l.every); at.After(l.now()) {
		l.next[key] = at
		return
	}
	delete(l.next, key)
}

func (l *rate) sweep(now time.Time) bool {
	for key, at := range l.next {
		if !at.After(now) {
			delete(l.next, key)
		}
	}
	return len(l.next) < l.limit
}
