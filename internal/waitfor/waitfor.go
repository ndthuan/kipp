// Package waitfor is how a test waits for a state its code cannot announce:
// a file another process writes, a row a goroutine of the server stores.
// What the code can announce (a channel, a hook, a state it reaches) is
// waited for on that, never here. The deadline is a safety net for a state
// that never comes, long enough that a loaded machine does not reach it, and
// it is checked on a ticker so that no test sleeps.
package waitfor

import (
	"testing"
	"time"
)

// Safety is how long Until waits for a state before it gives up.
const Safety = 30 * time.Second

// Poll is how often Until looks.
const Poll = 5 * time.Millisecond

// Until calls ok until it is true, and fails the test with the name of the
// state it was waiting for if that does not happen within Safety.
func Until(t testing.TB, what string, ok func() bool) {
	t.Helper()
	if !Within(Safety, ok) {
		t.Fatalf("timed out waiting until %s", what)
	}
}

// Within calls ok until it is true and says so, or says false once d has
// passed.
func Within(d time.Duration, ok func() bool) bool {
	if ok() {
		return true
	}
	tick := time.NewTicker(Poll)
	defer tick.Stop()
	timeout := time.NewTimer(d)
	defer timeout.Stop()
	for {
		select {
		case <-tick.C:
			if ok() {
				return true
			}
		case <-timeout.C:
			return false
		}
	}
}
