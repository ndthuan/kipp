package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The birth alone, a tick, is what a stranger with a reused pid can share
// with the stage; the mark is what it cannot.
func TestAProcessBornInTheStagesTickWithoutItsMarkIsNotTheStages(t *testing.T) {
	p, _, _ := start(t, context.Background(), "sleep 300")
	defer p.Kill(100 * time.Millisecond)
	g := p.Group()
	birth := g.Born[:strings.LastIndexByte(g.Born, ':')]
	for what, born := range map[string]string{
		"the same tick and another mark": birth + ":ANOTHERSTAGESMARKANOTHERST",
		"the same tick and no mark":      birth + ":",
		"the tick alone":                 birth,
		"the mark and another tick":      "0:" + g.Born[strings.LastIndexByte(g.Born, ':')+1:],
	} {
		if killed, err := reap(Group{Pgid: g.Pgid, Born: born}, time.Second); killed || !errors.Is(err, ErrNotProven) {
			t.Errorf("%s: killed %v, err %v", what, killed, err)
		}
	}
	if err := syscall.Kill(g.Pgid, 0); err != nil {
		t.Fatalf("the stage was killed: %v", err)
	}
}

// A stage that drops its environment drops the mark with it, and is then
// nobody's to kill.
func TestALeaderThatDroppedItsEnvironmentIsLeftAlone(t *testing.T) {
	p, _, dir := start(t, context.Background(), "echo $$ > $DIR/pid; exec env -i sleep 300")
	defer p.Kill(100 * time.Millisecond)
	pidIn(t, dir)
	deadline := time.Now().Add(10 * time.Second)
	for {
		ours, _ := stillLeads(p.Group())
		if !ours {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the leader still carries the mark after env -i")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if killed, err := reap(p.Group(), time.Second); killed || !errors.Is(err, ErrNotProven) {
		t.Fatalf("killed %v, err %v", killed, err)
	}
}

// While a process execs, the system briefly shows its environment as
// empty: the new image is in place before its environment is. A leader
// that keeps its environment through a chain of execs is the stage's at
// every moment of the chain, and is never taken for one that dropped it.
func TestALeaderIsTheStagesThroughoutAChainOfExecs(t *testing.T) {
	p, _, dir := start(t, context.Background(),
		`printf '[ "$1" -gt 0 ] && exec sh "$0" $(($1 - 1)); echo $$ > $DIR/pid; exec sleep 300' > $DIR/again; exec sh $DIR/again 2000`)
	defer p.Kill(100 * time.Millisecond)
	for reads := 0; ; reads++ {
		if ours, err := stillLeads(p.Group()); !ours {
			t.Fatalf("read %d: the leader was not shown to be the stage's: %v", reads, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "pid")); err == nil {
			break
		}
	}
}
