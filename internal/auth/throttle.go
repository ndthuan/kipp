package auth

import (
	"container/list"
	"fmt"
	"slices"
	"sync"
	"time"
)

const (
	freeFailures = 5
	maxWait      = 15 * time.Minute
	throttleKeys = 10_000
	ownKeys      = 1_000
	fullWait     = 2 * time.Second
	forgetAfter  = 24 * time.Hour
)

// ThrottledError refuses a password that was not checked: too many wrong
// ones came before it on the same counter.
type ThrottledError struct {
	RetryAfter time.Duration
}

func (e *ThrottledError) Error() string {
	return fmt.Sprintf("too many wrong passwords; try again in %s", e.RetryAfter.Round(time.Second))
}

// Throttle counts wrong passwords in a row by key, in memory: a restart
// clears it, and takes someone on the machine. It is safe for concurrent
// use.
type Throttle struct {
	mu    sync.Mutex
	now   func() time.Time
	limit int
	order *list.List
	keys  map[string]*list.Element
}

type failures struct {
	key   string
	count int
	last  time.Time
	until time.Time
}

// NewThrottle makes an empty Throttle that tells time by now.
func NewThrottle(now func() time.Time) *Throttle {
	return &Throttle{now: now, limit: throttleKeys, order: list.New(), keys: map[string]*list.Element{}}
}

// Wait is how long the longest-waiting of keys has before its next check,
// and zero when none has to wait.
func (t *Throttle) Wait(keys ...string) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.wait(keys, t.now())
}

func (t *Throttle) wait(keys []string, now time.Time) time.Duration {
	var most time.Duration
	for _, k := range keys {
		if f := t.entry(k, now); f != nil {
			most = max(most, f.until.Sub(now))
		}
	}
	return most
}

// Reserve counts an attempt on every one of keys before its password is
// looked at, or refuses it. Counting first is what holds attempts sent at
// once: each one in flight has taken its place in the run, so no more than
// the free ones are ever checked together, however many arrive. ok false is
// a refusal, and the duration how long until the next attempt is checked;
// ok true is a reservation, and the duration the wait that holds once it
// turns out wrong. A wrong password leaves it counted; Clear or Refund
// gives it back. A key the full table has no room for is refused for
// fullWait.
func (t *Throttle) Reserve(keys ...string) (time.Duration, bool) {
	return t.reserve(keys, t.limit)
}

// ReserveOwn is Reserve for the keys of a device or of the server's own
// machine, which may also take the ownKeys past the table's limit.
func (t *Throttle) ReserveOwn(keys ...string) (time.Duration, bool) {
	return t.reserve(keys, t.limit+ownKeys)
}

func (t *Throttle) reserve(keys []string, limit int) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if wait := t.wait(keys, now); wait > 0 {
		return wait, false
	}
	if !t.room(keys, limit, now) {
		return fullWait, false
	}
	var most time.Duration
	for _, k := range keys {
		f := t.entry(k, now)
		if f == nil {
			f = &failures{key: k}
			t.keys[k] = t.order.PushFront(f)
		} else {
			t.order.MoveToFront(t.keys[k])
		}
		f.count++
		f.last = now
		f.until = now.Add(backoff(f.count))
		most = max(most, f.until.Sub(now))
	}
	return most, true
}

func (t *Throttle) room(keys []string, limit int, now time.Time) bool {
	missing := 0
	for _, k := range keys {
		if t.keys[k] == nil {
			missing++
		}
	}
	for t.order.Len()+missing > limit {
		e := t.coldest(keys, now)
		if e == nil {
			return false
		}
		t.remove(e)
	}
	return true
}

func (t *Throttle) coldest(keys []string, now time.Time) *list.Element {
	for e := t.order.Back(); e != nil; e = e.Prev() {
		f := e.Value.(*failures)
		if cold(f, now) && !slices.Contains(keys, f.key) {
			return e
		}
	}
	return nil
}

func cold(f *failures, now time.Time) bool {
	return now.Sub(f.last) >= forgetAfter || (f.count <= freeFailures && !f.until.After(now))
}

// Refund gives back an attempt Reserve counted on keys whose password was
// never judged wrong: one that found no slot, whose request ended, or that
// was right on a key a right password does not clear.
func (t *Throttle) Refund(keys ...string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, k := range keys {
		e, ok := t.keys[k]
		if !ok {
			continue
		}
		f := e.Value.(*failures)
		f.count--
		if f.count <= 0 {
			t.remove(e)
			continue
		}
		f.until = f.last.Add(backoff(f.count))
	}
}

// Clear forgets keys: a right password ends their run.
func (t *Throttle) Clear(keys ...string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, k := range keys {
		if e, ok := t.keys[k]; ok {
			t.remove(e)
		}
	}
}

func (t *Throttle) entry(key string, now time.Time) *failures {
	e, ok := t.keys[key]
	if !ok {
		return nil
	}
	f := e.Value.(*failures)
	if now.Sub(f.last) >= forgetAfter {
		t.remove(e)
		return nil
	}
	return f
}

func (t *Throttle) remove(e *list.Element) {
	delete(t.keys, e.Value.(*failures).key)
	t.order.Remove(e)
}

func backoff(count int) time.Duration {
	over := count - freeFailures
	switch {
	case over < 0:
		return 0
	case over >= 10:
		return maxWait
	}
	return min(time.Second<<(over+1), maxWait)
}
