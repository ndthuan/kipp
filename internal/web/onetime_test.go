package web

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func give(t *testing.T, o *oneTimeForms, session string) string {
	t.Helper()
	v, err := o.give(session)
	must(t, err)
	return v
}

func TestAOneTimeValueIsGoodOnceForItsOwnSessionWithinAnHour(t *testing.T) {
	c := &testClock{now: time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)}
	o := newOneTimeForms(c.Now)
	v := give(t, o, "s1")
	if o.take("s2", v) {
		t.Fatal("another session used it")
	}
	if !o.take("s1", v) || o.take("s1", v) {
		t.Fatal("not good exactly once")
	}
	o.giveBack("s1", v)
	if !o.take("s1", v) {
		t.Fatal("a value given back was not good again")
	}
	late := give(t, o, "s1")
	c.advance(oneTimeLife)
	if o.take("s1", late) {
		t.Fatal("a value an hour old was good")
	}
}

func TestASessionKeepsItsNewestValuesAndTheTableItsNewestSessions(t *testing.T) {
	c := &testClock{now: time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)}
	o := newOneTimeForms(c.Now)
	first := give(t, o, "s")
	for range oneTimePerSession {
		c.advance(time.Second)
		give(t, o, "s")
	}
	if len(o.given["s"]) != oneTimePerSession || o.take("s", first) {
		t.Fatal("the oldest value was kept past the limit")
	}
	for i := range oneTimeSessions + 1 {
		give(t, o, fmt.Sprint("session-", i))
	}
	if len(o.given) > oneTimeSessions {
		t.Fatalf("%d sessions kept", len(o.given))
	}
}
