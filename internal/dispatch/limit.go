package dispatch

import (
	"context"
	"sync"
)

// NoStagesPrepares is how many builds a server that runs no stage of its
// own prepares at once: the clone and merge still happen on it.
const NoStagesPrepares = 4

// limit is the built-in runner's executors: each stage the server runs and
// each build's prepare holds one while it runs. Its size can change while
// it is held; what holds more than the new size keeps it until it gives it
// back.
type limit struct {
	mu   sync.Mutex
	max  int // 0: the server runs no stage, and prepares NoStagesPrepares at once
	busy int
	// waiters are the stages blocked in take. While there is one, tryTake
	// refuses, so a started build's stage gets the next executor before a
	// queued build's prepare.
	waiters int
	freed   chan struct{} // closed and replaced when an executor frees or the size grows
}

func newLimit(max int) *limit { return &limit{max: max, freed: make(chan struct{})} }

// room is how many may hold it at once. mu is held.
func (l *limit) room() int {
	if l.max == 0 {
		return NoStagesPrepares
	}
	return l.max
}

// take blocks until an executor is free and takes it, or reports false
// when ctx ends first.
func (l *limit) take(ctx context.Context) bool {
	l.mu.Lock()
	l.waiters++
	defer func() {
		l.waiters--
		l.mu.Unlock()
	}()
	for l.busy >= l.room() {
		freed := l.freed
		l.mu.Unlock()
		select {
		case <-freed:
		case <-ctx.Done():
			l.mu.Lock()
			return false
		}
		l.mu.Lock()
	}
	l.busy++
	return true
}

// tryTake takes an executor if one is free and no stage waits for one.
func (l *limit) tryTake() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.waiters > 0 || l.busy >= l.room() {
		return false
	}
	l.busy++
	return true
}

// full reports whether tryTake would refuse.
func (l *limit) full() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.waiters > 0 || l.busy >= l.room()
}

// give hands back what take or tryTake took.
func (l *limit) give() {
	l.mu.Lock()
	l.busy--
	l.wakeLocked()
	l.mu.Unlock()
}

func (l *limit) wakeLocked() {
	close(l.freed)
	l.freed = make(chan struct{})
}

// load is how many hold it and how many may.
func (l *limit) load() (busy, room int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.busy, l.room()
}

// runsStages reports whether the server runs stages of its own.
func (l *limit) runsStages() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.max > 0
}

// waiting is how many stages wait in take.
func (l *limit) waiting() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.waiters
}
