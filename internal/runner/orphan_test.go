package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"syscall"
	"testing"
	"time"
)

// supported skips where the system does not say when a process was born.
func supported(t *testing.T) {
	t.Helper()
	if _, err := bornOf(os.Getpid(), ""); errors.Is(err, errors.ErrUnsupported) {
		t.Skipf("no way to read a process's birth on %s", runtime.GOOS)
	}
}

// reaped starts a stage with a child in its group, reaps it as a later
// server would, and fails unless nothing of the group is left.
func reaped(t *testing.T, command string) {
	t.Helper()
	p, _, dir := start(t, context.Background(), command)
	child := pidIn(t, dir)
	g := p.Group()
	if g.Pgid <= 1 || g.Born == "" {
		t.Fatalf("group %+v", g)
	}
	killed, err := reap(g, 5*time.Second)
	if err != nil || !killed {
		t.Fatalf("killed %v, err %v", killed, err)
	}
	// Strictly: reap returns once nothing of the group is left.
	for _, pid := range []int{g.Pgid, child} {
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("process %d outlived the reap: %v", pid, err)
		}
	}
	if code, _ := p.Wait(); code == 0 {
		t.Fatal("a reaped stage reported success")
	}
	if killed, err := reap(g, time.Second); killed || err != nil {
		t.Fatalf("a group that is gone: killed %v, err %v", killed, err)
	}
}

func TestReapKillsTheWholeGroupOfAStage(t *testing.T) {
	supported(t)
	reaped(t, "sleep 300 & echo $! > $DIR/pid; wait")
}

// An exec keeps the pid and the birth, so the group is still the stage's.
func TestReapKillsAStageWhoseShellHasBecomeItsCommand(t *testing.T) {
	supported(t)
	reaped(t, "sleep 300 & echo $! > $DIR/pid; exec sleep 300")
}

func TestReapLeavesAGroupItCannotProveIsTheStages(t *testing.T) {
	supported(t)
	p, _, _ := start(t, context.Background(), "sleep 300")
	defer p.Kill(100 * time.Millisecond)
	// A process that took the stage's group id over after the stage: it
	// leads the group, and was born at another moment.
	foreign := exec.Command("sleep", "300")
	foreign.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := foreign.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = foreign.Process.Kill(); _ = foreign.Wait() }()
	for what, g := range map[string]Group{
		"a birth that is another process's": {Pgid: foreign.Process.Pid, Born: p.Group().Born},
		"a birth that was never read":       {Pgid: p.Group().Pgid},
		"a birth that is no birth":          {Pgid: p.Group().Pgid, Born: "0.000001"},
	} {
		if killed, err := reap(g, time.Second); killed || !errors.Is(err, ErrNotProven) {
			t.Fatalf("%s: killed %v, err %v", what, killed, err)
		}
	}
	for _, pgid := range []int{-1, 0, 1} {
		if killed, err := reap(Group{Pgid: pgid, Born: p.Group().Born}, time.Second); killed || err != nil {
			t.Fatalf("group %d: killed %v, err %v", pgid, killed, err)
		}
	}
	for _, pid := range []int{p.Group().Pgid, foreign.Process.Pid} {
		if err := syscall.Kill(pid, 0); err != nil {
			t.Fatalf("process %d was killed: %v", pid, err)
		}
	}
}

func TestReapKillsAGroupThatIgnoresSIGTERMOnceTheGraceIsOver(t *testing.T) {
	supported(t)
	p, _, dir := start(t, context.Background(), "trap '' TERM; echo $$ > $DIR/pid; while :; do sleep 1; done")
	pidIn(t, dir) // the trap is set
	asked := time.Now()
	killed, err := reap(p.Group(), 300*time.Millisecond)
	if err != nil || !killed {
		t.Fatalf("killed %v, err %v", killed, err)
	}
	if took := time.Since(asked); took < 300*time.Millisecond || took > 5*time.Second {
		t.Fatalf("the reap took %v", took)
	}
	if err := syscall.Kill(-p.Group().Pgid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("the group outlived the reap: %v", err)
	}
}

func TestAProcessKeepsItsBirthAndAProcessThatIsGoneHasNone(t *testing.T) {
	supported(t)
	p, _, _ := start(t, context.Background(), "sleep 300")
	defer p.Kill(100 * time.Millisecond)
	first, err := bornOf(p.Group().Pgid, "m")
	if err != nil || first == "" {
		t.Fatalf("%q, %v", first, err)
	}
	if again, err := bornOf(p.Group().Pgid, "m"); err != nil || again != first {
		t.Fatalf("read again: %q, %v; first %q", again, err, first)
	}
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	if born, err := bornOf(gone.Process.Pid, "m"); err == nil {
		t.Fatalf("a process that is gone was born at %q", born)
	}
}

// Stages started one after the other are born as close together as two
// processes can be: where the system counts births in ticks, some of them
// in the same one. What is recorded for one must still never fit another,
// which is the case of a row that names the wrong group.
func TestWhatIsRecordedForOneStageNeverFitsAnotherStartedInTheSameMoment(t *testing.T) {
	supported(t)
	var stages []Proc
	for range 12 {
		p, _, _ := start(t, context.Background(), "sleep 300")
		defer p.Kill(100 * time.Millisecond)
		stages = append(stages, p)
	}
	for i, a := range stages {
		if ours, err := stillLeads(a.Group()); err != nil || !ours {
			t.Fatalf("stage %d is not shown to be itself: %v, %v", i, ours, err)
		}
		for j, b := range slices.Concat(stages[:i], stages[i+1:]) {
			tampered := Group{Pgid: b.Group().Pgid, Born: a.Group().Born}
			if killed, err := reap(tampered, time.Second); killed || !errors.Is(err, ErrNotProven) {
				t.Fatalf("another stage (%d) was taken for stage %d (%+v): killed %v, err %v", j, i, tampered, killed, err)
			}
		}
	}
	for i, p := range stages {
		if err := syscall.Kill(p.Group().Pgid, 0); err != nil {
			t.Fatalf("stage %d was killed: %v", i, err)
		}
	}
}

func TestAStageIsStartedWithAMarkOfItsOwn(t *testing.T) {
	a, outA, _ := start(t, context.Background(), "echo $"+MarkEnv)
	b, outB, _ := start(t, context.Background(), "echo $"+MarkEnv)
	for _, p := range []Proc{a, b} {
		if code, err := p.Wait(); code != 0 || err != nil {
			t.Fatal(code, err)
		}
	}
	if markA, markB := outA.String(), outB.String(); len(markA) < 20 || len(markB) < 20 || markA == markB {
		t.Fatalf("marks %q and %q", markA, markB)
	}
}

func TestTheMarkIsFoundOnlyAsAWholeEntryOfTheEnvironment(t *testing.T) {
	env := []byte("PATH=/bin\x00" + MarkEnv + "=abc\x00HOME=/root\x00")
	for mark, want := range map[string]bool{"abc": true, "ab": false, "abcd": false, "": false} {
		if got := carries(env, mark); got != want {
			t.Errorf("mark %q: got %v, want %v", mark, got, want)
		}
	}
	if !carries([]byte(MarkEnv+"=abc"), "abc") {
		t.Error("the mark as the only entry, with no zero byte around it, was not found")
	}
	if carries([]byte("X"+MarkEnv+"=abc\x00"), "abc") || carries([]byte("NOTE="+MarkEnv+"=abc\x00"), "abc") {
		t.Error("the mark was found inside another entry")
	}
}

func TestTheStartTimeIsReadPastACommandNameWithBracketsAndSpaces(t *testing.T) {
	const line = "4242 (a) b (c) R 1 4242 4242 0 -1 4194304 100 0 0 0 1 2 0 0 20 0 1 0 987654 1000000 200 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0\n"
	if got, ok := statBorn([]byte(line)); !ok || got != "987654" {
		t.Fatalf("got %q, %v", got, ok)
	}
	for _, short := range []string{"", "1 (sh) S 1 2 3", "no bracket at all"} {
		if got, ok := statBorn([]byte(short)); ok {
			t.Fatalf("%q: got %q", short, got)
		}
	}
}
