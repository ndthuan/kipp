package waitfor

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestUntilReturnsAsSoonAsTheStateHolds(t *testing.T) {
	var calls atomic.Int32
	Until(t, "the third look", func() bool { return calls.Add(1) == 3 })
	if calls.Load() != 3 {
		t.Fatalf("looked %d times", calls.Load())
	}
}

func TestWithinSaysFalseOnceItsTimeIsUp(t *testing.T) {
	if Within(10*time.Millisecond, func() bool { return false }) {
		t.Fatal("a state that never holds was reported")
	}
	if !Within(time.Hour, func() bool { return true }) {
		t.Fatal("a state that holds was not")
	}
}
