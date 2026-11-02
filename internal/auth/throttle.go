package auth

import (
	"container/list"
	"fmt"
	"slices"
	"sync"
	"time"
)

// What a run of wrong passwords costs. The first freeFailures in a row
// cost nothing, so that a person who mistypes is never kept waiting; each
// one after waits twice as long as the one before, up to maxWait, and
// never more, so that whoever sends wrong passwords for a name cannot
// shut its owner out for good. NIST SP 800-63B allows at most 100 in a row
// on an account; this lets about 100 through in a day.
const (
	freeFailures = 5
	maxWait      = 15 * time.Minute
	// throttleKeys bounds the table: a flood of made-up names costs it no
	// more than this many entries. Only a cold entry makes room, the one
	// that failed longest ago first; one that waits or has run past its
	// free failures is never dropped, since dropping it would hand its
	// name a fresh run, and a new key finds the table full instead.
	throttleKeys = 10_000
	// ownKeys is the room past throttleKeys for the keys of devices and of
	// the server's own machine, which strangers cannot make: a table that
	// strangers have filled still counts them.
	ownKeys = 1_000
	// fullWait is how long a new key waits when the table has no room for
	// it: the wait after the free failures.
	fullWait = 2 * time.Second
	// forgetAfter is how long a key keeps its count without a new
	// failure. A key a success does not clear (a client address, which
	// many people may share) would otherwise count for as long as the
	// server runs.
	forgetAfter = 24 * time.Hour
)

// ThrottledError refuses a password that was not checked: too many wrong
// ones came before it on the same counter.
type ThrottledError struct {
	// RetryAfter is how long until the next one is checked.
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
	// order holds the entries, the latest to fail at the front.
	order *list.List
	keys  map[string]*list.Element
}

type failures struct {
	key   string
	count int
	last  time.Time // of the latest failure
	until time.Time // before which the key is not checked
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

// room drops cold entries until the keys not in the table fit within
// limit, and says whether they do. It never drops one of keys.
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

// coldest is the cold entry that failed longest ago, other than one of
// keys, or nil.
func (t *Throttle) coldest(keys []string, now time.Time) *list.Element {
	for e := t.order.Back(); e != nil; e = e.Prev() {
		f := e.Value.(*failures)
		if cold(f, now) && !slices.Contains(keys, f.key) {
			return e
		}
	}
	return nil
}

// cold: the entry may be dropped to make room. It is forgotten already, or
// within its free failures with no wait running.
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

// entry is key's count, or nil when it has none or has had no failure
// for forgetAfter.
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

// backoff is the wait after count wrong passwords in a row: none for the
// free ones, then 2 seconds before the first one after them, doubling
// with each failure up to maxWait.
func backoff(count int) time.Duration {
	over := count - freeFailures
	switch {
	case over < 0:
		return 0
	case over >= 10: // 2^10 s is past the cap already, and a larger shift overflows
		return maxWait
	}
	return min(time.Second<<(over+1), maxWait)
}
