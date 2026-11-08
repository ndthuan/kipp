package runner

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/waitfor"
)

func start(t *testing.T, ctx context.Context, command string) (Proc, *bytes.Buffer, string) {
	t.Helper()
	dir := t.TempDir()
	var out bytes.Buffer
	p, err := Local{}.Start(ctx, Spec{Dir: dir, Command: command, Env: []string{"PATH=" + os.Getenv("PATH"), "DIR=" + dir}, Output: &out})
	if err != nil {
		t.Fatal(err)
	}
	return p, &out, dir
}

func pidIn(t *testing.T, dir string) int {
	t.Helper()
	pid := 0
	waitfor.Until(t, "the stage wrote its pid", func() bool {
		b, err := os.ReadFile(filepath.Join(dir, "pid"))
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil
	})
	return pid
}

func gone(pid int) bool {
	return waitfor.Within(waitfor.Safety, func() bool { return syscall.Kill(pid, 0) != nil || zombie(pid) })
}

// zombie reports whether pid has exited and waits to be reaped. A killed
// grandchild is reaped by whatever adopted it, which in a container or on a
// loaded machine may be slower than the test, and a zombie answers kill 0.
func zombie(pid int) bool {
	out, err := exec.Command("ps", "-ax", "-o", "pid=,stat=").Output()
	if err != nil {
		return false
	}
	for line := range strings.Lines(string(out)) {
		if f := strings.Fields(line); len(f) == 2 && f[0] == strconv.Itoa(pid) {
			return strings.HasPrefix(f[1], "Z")
		}
	}
	return false
}

func TestRunsInDirWithEnvAndReturnsTheExitCode(t *testing.T) {
	t0 := time.Now()
	p, out, dir := start(t, context.Background(), "echo hello $DIR; pwd; echo err >&2; exit 3")
	code, err := p.Wait()
	if err != nil || code != 3 {
		t.Fatalf("code %d err %v", code, err)
	}
	if time.Since(t0) > time.Second {
		t.Fatal("a clean stage waited for the stray grace")
	}
	if s := out.String(); !strings.Contains(s, "hello "+dir) || !strings.Contains(s, dir+"\n") || !strings.Contains(s, "err") {
		t.Fatalf("out %q", s)
	}
}

func TestKillTakesTheGrandchildWithIt(t *testing.T) {
	p, _, dir := start(t, context.Background(), "sleep 300 & echo $! > $DIR/pid; wait")
	pid := pidIn(t, dir)
	p.Kill(200 * time.Millisecond)
	if code, _ := p.Wait(); code == 0 {
		t.Fatal("a killed stage reported success")
	}
	if !gone(pid) {
		t.Fatalf("grandchild %d survived the kill", pid)
	}
}

func TestAStrayLeftBehindDoesNotHoldTheStageAndIsKilled(t *testing.T) {
	p, _, dir := start(t, context.Background(), "sleep 300 & echo $! > $DIR/pid; exit 0")
	t0 := time.Now()
	code, err := p.Wait()
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	if time.Since(t0) > 5*time.Second {
		t.Fatal("Wait hung on the stray's stdout")
	}
	if !gone(pidIn(t, dir)) {
		t.Fatal("the stray outlived the stage")
	}
}

func TestContextCancelKills(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p, _, dir := start(t, ctx, "sleep 300 & echo $! > $DIR/pid; wait")
	pid := pidIn(t, dir)
	cancel()
	done := make(chan int)
	go func() { c, _ := p.Wait(); done <- c }()
	select {
	case c := <-done:
		if c == 0 {
			t.Fatal("cancelled and yet success")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Wait did not return after cancel")
	}
	if !gone(pid) {
		t.Fatalf("grandchild %d survived the cancel", pid)
	}
}

func TestACommandThatCannotStartIsAnError(t *testing.T) {
	_, err := Local{Shell: "/no/such/shell"}.Start(context.Background(), Spec{Dir: t.TempDir(), Command: "true", Output: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("started with no shell")
	}
}

func TestKillGraceIsHonouredForAGrandchildThatIgnoresTerm(t *testing.T) {
	// The grandchild writes its pid after its trap is set: a pid taken from $!
	// is known before the trap, and a TERM that lands first kills it on a busy host.
	p, _, dir := start(t, context.Background(), `sh -c 'trap "" TERM; echo $$ > "$DIR/pid"; sleep 8; echo ok > "$DIR/ok"' & wait`)
	pid := pidIn(t, dir) // the trap is set
	t0 := time.Now()
	killed := make(chan struct{})
	go func() { p.Kill(5 * time.Second); close(killed) }()
	time.Sleep(4500 * time.Millisecond)
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the TERM-ignoring grandchild was killed before the grace ran out")
	}
	if code, _ := p.Wait(); code == 0 {
		t.Fatal("a killed stage reported success")
	}
	<-killed
	if d := time.Since(t0); d < 4900*time.Millisecond {
		t.Fatalf("Wait returned after %v, before the grace", d)
	}
	if !gone(pid) {
		t.Fatal("the grandchild survived SIGKILL")
	}
	if _, err := os.Stat(filepath.Join(dir, "ok")); err == nil {
		t.Fatal("the grandchild finished its work")
	}
}

func TestANilEnvIsAnEmptyEnvironment(t *testing.T) {
	t.Setenv("KIPP_LEAK", "yes")
	var out bytes.Buffer
	p, err := Local{}.Start(context.Background(), Spec{Dir: t.TempDir(), Command: `echo "[$KIPP_LEAK]"`, Output: &out})
	if err != nil {
		t.Fatal(err)
	}
	if code, err := p.Wait(); code != 0 || err != nil {
		t.Fatalf("code %d err %v", code, err)
	}
	if got := out.String(); got != "[]\n" {
		t.Fatalf("stage saw the parent environment: %q", got)
	}
}

func TestKillAfterTheStageExitedIsANoOp(t *testing.T) {
	p, _, _ := start(t, context.Background(), "true")
	if code, err := p.Wait(); code != 0 || err != nil {
		t.Fatalf("code %d err %v", code, err)
	}
	t0 := time.Now()
	p.Kill(5 * time.Second)
	if time.Since(t0) > time.Second {
		t.Fatal("Kill on a finished stage blocked")
	}
	if code, _ := p.Wait(); code != 0 {
		t.Fatalf("a finished stage changed its code to %d", code)
	}
}

func TestACancelledContextDoesNotFork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := t.TempDir()
	_, err := Local{}.Start(ctx, Spec{Dir: dir, Command: "echo x > $DIR/ran", Env: []string{"DIR=" + dir}, Output: &bytes.Buffer{}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "ran")); err == nil {
		t.Fatal("the command ran")
	}
}
