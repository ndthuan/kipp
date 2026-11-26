package dispatch

import (
	"context"
	"sync"
)

// NoStagesPrepares is how many builds a server that runs no stage of its
// own prepares at once: the clone and merge still happen on it.
const NoStagesPrepares = 4

type limit struct {
	mu      sync.Mutex
	max     int
	busy    int
	waiters int
	freed   chan struct{}
}

func newLimit(max int) *limit { return &limit{max: max, freed: make(chan struct{})} }

func (l *limit) room() int {
	if l.max == 0 {
		return NoStagesPrepares
	}
	return l.max
}

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

func (l *limit) tryTake() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.waiters > 0 || l.busy >= l.room() {
		return false
	}
	l.busy++
	return true
}

func (l *limit) full() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.waiters > 0 || l.busy >= l.room()
}

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

func (l *limit) set(max int) {
	l.mu.Lock()
	l.max = max
	l.wakeLocked()
	l.mu.Unlock()
}

func (l *limit) load() (busy, room int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.busy, l.room()
}

func (l *limit) runsStages() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.max > 0
}

func (l *limit) waiting() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.waiters
}
