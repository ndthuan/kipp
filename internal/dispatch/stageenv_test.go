package dispatch

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/agenthub"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

func TestKippEnvIsReadLikeGitHubsEnvFile(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		want     []envVar
		problems int
	}{
		{"plain lines", "A=1\nB=two words\n", []envVar{{"A", "1"}, {"B", "two words"}}, 0},
		{"a later line wins", "A=1\nA=2\n", []envVar{{"A", "2"}}, 0},
		{"blank lines are ignored", "\n\nA=1\n\n", []envVar{{"A", "1"}}, 0},
		{"no newline at the end", "A=1", []envVar{{"A", "1"}}, 0},
		{"an empty value", "E=\n", []envVar{{"E", ""}}, 0},
		{"the first equals sign splits", "V=a=b\n", []envVar{{"V", "a=b"}}, 0},
		{"a heredoc marker after the equals sign is value", "A=x<<y\n", []envVar{{"A", "x<<y"}}, 0},
		{"a heredoc", "M<<EOF\nl1\nl2\nEOF\nN=3\n", []envVar{{"M", "l1\nl2"}, {"N", "3"}}, 0},
		{"a heredoc keeps its blank lines", "H<<D\n\nx\nD\n", []envVar{{"H", "\nx"}}, 0},
		{"a heredoc delimiter may hold an equals sign", "H<<A=B\nx\nA=B\n", []envVar{{"H", "x"}}, 0},
		{"a line that is neither is skipped", "nonsense\nA=1\n", []envVar{{"A", "1"}}, 1},
		{"an empty name is skipped", "=v\nA=1\n", []envVar{{"A", "1"}}, 1},
		{"a heredoc never closed is skipped", "A=1\nM<<EOF\nl1\n", []envVar{{"A", "1"}}, 1},
		{"a heredoc with no delimiter is skipped", "M<<\nA=1\n", []envVar{{"A", "1"}}, 1},
		{"a NUL in a value is skipped", "B=a\x00b\nA=1\n", []envVar{{"A", "1"}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, problems := parseEnvFile([]byte(tc.in))
			if !slices.Equal(got, tc.want) || len(problems) != tc.problems {
				t.Fatalf("%q: got %v and %d problems %v, want %v and %d", tc.in, got, len(problems), problems, tc.want, tc.problems)
			}
		})
	}
}

func TestAStageGetsTheVariablesOfTheStagesItNeedsDirectlyOrThroughOthersAndNoOther(t *testing.T) {
	stages := []*config.Stage{
		{Name: "a"}, {Name: "b", Needs: []string{"a"}}, {Name: "c", Needs: []string{"b"}},
		{Name: "d"}, {Name: "e", Needs: []string{"a", "d"}},
	}
	o := newStageOutputs()
	o.add("a", []envVar{{"X", "1"}, {"Y", "1"}})
	o.add("d", []envVar{{"X", "2"}})
	for stage, want := range map[string][]string{
		"a": nil,
		"d": nil,
		"b": {"X=1", "Y=1"},
		"c": {"X=1", "Y=1"},
		"e": {"X=2", "Y=1"}, // d finished after a
	} {
		var s *config.Stage
		for _, x := range stages {
			if x.Name == stage {
				s = x
			}
		}
		if got := o.upstream(stages, s); !slices.Equal(got, want) {
			t.Errorf("stage %s: %v, want %v", stage, got, want)
		}
	}
	if got, want := o.all(), []string{"X=2", "Y=1"}; !slices.Equal(got, want) {
		t.Errorf("every stage that ended: %v, want %v", got, want)
	}
}

// envJob is a job of the stages given, one per line after "stages:".
func envJob(stages string, head ...string) string {
	return "name: e\n" + strings.Join(head, "\n") + "\nstages:\n" + stages
}

func runEnvJob(t *testing.T, job string, opts ...func(*Options)) (*harness, *store.Build) {
	t.Helper()
	h := newHarness(t, map[string]string{"e": job}, opts...)
	return h, h.wait(t, h.trigger(t, "e").ID)
}

func TestAValueAStageWritesReachesTheStagesThatNeedItDirectlyAndThroughOthers(t *testing.T) {
	h, b := runEnvJob(t, envJob(`
  a: {run: 'echo "V=1.2.3" >> "$KIPP_ENV"'}
  b: {needs: [a], run: 'echo "W=$V-b" >> "$KIPP_ENV"; echo "b sees $V"'}
  c: {needs: [b], run: 'echo "c sees $V and $W"'}
`))
	if b.Status != store.Success {
		t.Fatalf("%v: %s", b.Status, h.log(t, b, "c"))
	}
	if got := h.log(t, b, "b"); got != "b sees 1.2.3\n" {
		t.Errorf("b: %q", got)
	}
	if got := h.log(t, b, "c"); got != "c sees 1.2.3 and 1.2.3-b\n" {
		t.Errorf("c: %q", got)
	}
}

func TestAValueDoesNotReachAStageBesideTheWriter(t *testing.T) {
	h, b := runEnvJob(t, envJob(`
  root: {run: 'true'}
  a: {needs: [root], run: 'echo "V=1" >> "$KIPP_ENV"; touch "$KIPP_WORKSPACE/a.done"'}
  beside: {needs: [root], run: 'while [ ! -e "$KIPP_WORKSPACE/a.done" ]; do sleep 0.05; done; sleep 0.5; echo "beside [$V]"'}
`))
	if got := h.log(t, b, "beside"); got != "beside []\n" {
		t.Fatalf("%q", got)
	}
}

func TestAnEmptyKippEnvFileIsGivenToEveryStage(t *testing.T) {
	h, b := runEnvJob(t, envJob(`
  s: {run: 'test -f "$KIPP_ENV" && echo size=$(wc -c < "$KIPP_ENV" | tr -d " ")'}
`))
	if got := h.log(t, b, "s"); got != "size=0\n" {
		t.Fatalf("%v %q", b.Status, got)
	}
}

func TestAMultiLineValueIsReadWithAHeredoc(t *testing.T) {
	h, b := runEnvJob(t, envJob(`
  a: {run: 'printf "NOTES<<END\nline one\nline two\nEND\nAFTER=1\n" >> "$KIPP_ENV"'}
  b: {needs: [a], run: 'printf "[%s] [%s]\n" "$NOTES" "$AFTER"'}
`))
	if got := h.log(t, b, "b"); got != "[line one\nline two] [1]\n" {
		t.Fatalf("%v %q", b.Status, got)
	}
}

func TestAFailedStagesWritesAreIgnoredAndItsOutcomeIsUnchanged(t *testing.T) {
	h, b := runEnvJob(t, envJob(`
  a: {run: 'echo "V=lost" >> "$KIPP_ENV"; exit 3'}
  b: {needs: [a], when: {needs_status: always}, run: 'echo "[$V]"'}
`))
	if got := h.log(t, b, "b"); got != "[]\n" || h.stages(t, b.ID)["a"].Status != store.StageFailed {
		t.Fatalf("%q %v", got, h.stages(t, b.ID)["a"].Status)
	}
}

func TestACancelledStagesWritesAreIgnored(t *testing.T) {
	o := newStageOutputs()
	h := newHarness(t, map[string]string{"e": envJob("  a: {run: 'true'}\n")})
	e := &buildEnv{workspace: t.TempDir(), outs: o}
	s := &config.Stage{Name: "a"}
	if err := os.MkdirAll(filepath.Join(e.workspace, ".kipp"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.workspace, ".kipp", "env-a"), []byte("V=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := &store.Build{Job: "e", Number: 1}
	for _, status := range []store.StageStatus{store.StageFailed, store.StageCancelled, store.StageSkipped} {
		h.d.collectEnv(b, e, s, status)
	}
	if got := o.all(); got != nil {
		t.Fatalf("%v", got)
	}
	h.d.collectEnv(b, e, s, store.StageUnstable)
	if got := o.all(); !slices.Equal(got, []string{"V=1"}) {
		t.Fatalf("%v", got)
	}
}

func TestAnUnstableStagesWritesAreRead(t *testing.T) {
	h, b := runEnvJob(t, envJob(`
  a: {unstable_exit: [4], run: 'echo "V=kept" >> "$KIPP_ENV"; exit 4'}
  b: {needs: [a], run: 'echo "[$V]"'}
`))
	if got := h.log(t, b, "b"); got != "[kept]\n" {
		t.Fatalf("%v %q", b.Status, got)
	}
}

func TestARetriedStageKeepsOnlyWhatTheTryThatEndedWrote(t *testing.T) {
	h, b := runEnvJob(t, envJob(`
  a:
    retries: 2
    run: |
      n=$(cat runs 2>/dev/null || echo 0); n=$((n+1)); echo $n > runs
      if [ $n -lt 3 ]; then echo "OLD=$n" >> "$KIPP_ENV"; exit 7; fi
      echo "NEW=$n" >> "$KIPP_ENV"
  b: {needs: [a], run: 'echo "[$OLD] [$NEW]"'}
`))
	if got := h.log(t, b, "b"); got != "[] [3]\n" {
		t.Fatalf("%v %q", b.Status, got)
	}
}

func TestANameKippRefusesAParamsNameAndAMalformedLineAreSkippedWithOneLineEach(t *testing.T) {
	h, b := runEnvJob(t, envJob(`
  a:
    run: |
      printf 'KIPP_X=1\nCI=0\nTOKEN=x\nP=1\n1BAD=1\nnonsense\nGOOD=ok\n' >> "$KIPP_ENV"
  b: {needs: [a], run: 'echo "[$KIPP_X] [$CI] [$TOKEN] [$P] [$GOOD]"'}
`, "secrets: [TOKEN]", "params:\n  P: {default: given}"))
	if got := h.log(t, b, "b"); got != "[] [true] [***] [given] [ok]\n" {
		t.Fatalf("%v %q", b.Status, got)
	}
	log := h.log(t, b, "a")
	if b.Status != store.Success || strings.Count(log, "kipp: KIPP_ENV a: ") != 6 {
		t.Fatalf("%v: %q", b.Status, log)
	}
	for _, want := range []string{"env KIPP_X:", "env CI:", "env TOKEN:", "env P:", `env "1BAD":`, "line 6 is neither"} {
		if !strings.Contains(log, want) {
			t.Errorf("no line says %s: %q", want, log)
		}
	}
	if strings.Contains(log, "value-of-TOKEN") {
		t.Errorf("a secret's value is in the log: %q", log)
	}
}

func TestAKippEnvFileOverSixtyFourKiBIsRefusedWholeWithOneLine(t *testing.T) {
	h, b := runEnvJob(t, envJob(`
  big:
    run: |
      echo "FIRST=1" >> "$KIPP_ENV"
      printf 'BIG=%065536d\n' 0 >> "$KIPP_ENV"
  b: {needs: [big], run: 'echo "[$FIRST] [${#BIG}]"'}
`))
	log := h.log(t, b, "big")
	if got := h.log(t, b, "b"); got != "[] [0]\n" || strings.Count(log, "kipp: KIPP_ENV big: ") != 1 || b.Status != store.Success {
		t.Fatalf("%v %q %q", b.Status, got, log)
	}
}

func TestAKippEnvFileOfExactlySixtyFourKiBIsRead(t *testing.T) {
	h, b := runEnvJob(t, envJob(`
  big:
    run: 'printf "BIG=%065531d\n" 0 >> "$KIPP_ENV"; test "$(wc -c < "$KIPP_ENV" | tr -d " ")" = 65536'
  b: {needs: [big], run: 'echo "[${#BIG}]"'}
`))
	if got := h.log(t, b, "b"); got != "[65531]\n" {
		t.Fatalf("%v %q: %s", b.Status, got, h.log(t, b, "big"))
	}
}

func TestAWriterThatFinishedLaterWins(t *testing.T) {
	h, b := runEnvJob(t, envJob(`
  slow: {run: 'sleep 0.6; echo "X=slow" >> "$KIPP_ENV"'}
  quick: {run: 'echo "X=quick" >> "$KIPP_ENV"'}
  r: {needs: [slow, quick], run: 'echo "[$X]"'}
`))
	if got := h.log(t, b, "r"); got != "[slow]\n" {
		t.Fatalf("%v %q", b.Status, got)
	}
}

func TestKippEnvSitsAboveTheJobFileAndBelowTheStagesOwnEnv(t *testing.T) {
	h, b := runEnvJob(t, envJob(`
  a: {run: 'printf "X=file\nY=file\n" >> "$KIPP_ENV"'}
  b: {needs: [a], env: {Y: stage}, run: 'echo "[$X] [$Y]"'}
`, "env: {X: job, Y: job}"))
	if got := h.log(t, b, "b"); got != "[file] [stage]\n" {
		t.Fatalf("%v %q", b.Status, got)
	}
}

func TestKippEnvSitsAboveTheStagesFilesEnv(t *testing.T) {
	h := newHarness(t, map[string]string{"src": "name: src\nsource: {repo: o/r, ref: main, path: ci.yml}\n"}, func(o *Options) {
		o.Clone = func(_ context.Context, dir, _, _ string, _ io.Writer) error {
			file := "env: {X: stagesfile}\nstages:\n  a: {run: 'echo \"X=kenv\" >> \"$KIPP_ENV\"'}\n  b: {needs: [a], run: 'echo \"[$X]\"'}\n"
			return os.WriteFile(filepath.Join(dir, "ci.yml"), []byte(file), 0o600)
		}
	})
	b := h.wait(t, h.trigger(t, "src").ID)
	if got := h.log(t, b, "b"); got != "[kenv]\n" {
		t.Fatalf("%v %q", b.Status, got)
	}
}

func TestAHookGetsTheVariablesOfEveryStageThatEnded(t *testing.T) {
	h, b := runEnvJob(t, envJob(`
  a: {run: 'echo "V=1" >> "$KIPP_ENV"'}
  b: {run: 'echo "W=2" >> "$KIPP_ENV"'}
`, "on_success: {run: 'echo \"hook [$V] [$W]\"'}"))
	if got := h.log(t, b, config.HooksLog); got != "hook [1] [2]\n" {
		t.Fatalf("%v %q", b.Status, got)
	}
}

// onAgent is the job's stages on a standing agent called box.
func onAgent(t *testing.T) func(*Options) {
	t.Helper()
	ag := newAgents(t, agenthub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	ag.runBox(t.TempDir())
	return ag.opts
}

func TestAValueAnAgentStageWritesReachesAStageOnTheServerAndOneOnTheAgent(t *testing.T) {
	h, b := runEnvJob(t, envJob(`
  a: {runs_on: linux, run: 'echo "V=from-agent" >> "$KIPP_ENV"'}
  b: {needs: [a], run: 'echo "server [$V]"'}
  c: {needs: [a], runs_on: linux, run: 'echo "agent [$V]"'}
  d: {needs: [b], run: 'echo "far [$V]"'}
`), onAgent(t))
	for stage, want := range map[string]string{"b": "server [from-agent]\n", "c": "agent [from-agent]\n", "d": "far [from-agent]\n"} {
		if got := h.log(t, b, stage); got != want {
			t.Errorf("%s: %q, want %q (%v)", stage, got, want, b.Status)
		}
	}
}

func TestAValueAServerStageWritesReachesAStageOnAnAgent(t *testing.T) {
	h, b := runEnvJob(t, envJob(`
  a: {run: 'echo "V=from-server" >> "$KIPP_ENV"'}
  b: {needs: [a], runs_on: linux, run: 'echo "agent [$V]"'}
`), onAgent(t))
	if got := h.log(t, b, "b"); got != "agent [from-server]\n" {
		t.Fatalf("%v %q", b.Status, got)
	}
}

func TestAnAgentStageGetsAnEmptyKippEnvFile(t *testing.T) {
	h, b := runEnvJob(t, envJob(`
  s: {runs_on: linux, run: 'test -f "$KIPP_ENV" && echo size=$(wc -c < "$KIPP_ENV" | tr -d " ")'}
`), onAgent(t))
	if got := h.log(t, b, "s"); got != "size=0\n" {
		t.Fatalf("%v %q", b.Status, got)
	}
}

func TestARetriedAgentStageKeepsOnlyWhatTheTryThatEndedWrote(t *testing.T) {
	h, b := runEnvJob(t, envJob(`
  a:
    runs_on: linux
    retries: 2
    run: |
      n=$(cat runs 2>/dev/null || echo 0); n=$((n+1)); echo $n > runs
      if [ $n -lt 3 ]; then echo "OLD=$n" >> "$KIPP_ENV"; exit 7; fi
      echo "NEW=$n" >> "$KIPP_ENV"
  b: {needs: [a], run: 'echo "[$OLD] [$NEW]"'}
`), onAgent(t))
	if got := h.log(t, b, "b"); got != "[] [3]\n" {
		t.Fatalf("%v %q", b.Status, got)
	}
}

func TestAFailedAgentStagesWritesAreIgnored(t *testing.T) {
	h, b := runEnvJob(t, envJob(`
  a: {runs_on: linux, run: 'echo "V=lost" >> "$KIPP_ENV"; exit 3'}
  b: {needs: [a], when: {needs_status: always}, run: 'echo "[$V]"'}
`), onAgent(t))
	if got := h.log(t, b, "b"); got != "[]\n" {
		t.Fatalf("%v %q", b.Status, got)
	}
}

func TestAnAgentStagesRefusedNamesAndOversizeFileAreLoggedOnItsOwnLog(t *testing.T) {
	h, b := runEnvJob(t, envJob(`
  a: {runs_on: linux, run: 'printf "KIPP_X=1\nGOOD=ok\n" >> "$KIPP_ENV"'}
  big: {runs_on: linux, run: 'printf "BIG=%065536d\n" 0 >> "$KIPP_ENV"'}
  r: {needs: [a, big], run: 'echo "[$KIPP_X] [$GOOD] [$BIG]"'}
`), onAgent(t))
	if got := h.log(t, b, "r"); got != "[] [ok] []\n" {
		t.Fatalf("%v %q", b.Status, got)
	}
	if n := strings.Count(h.log(t, b, "a"), "kipp: KIPP_ENV a: "); n != 1 {
		t.Errorf("a: %d lines: %q", n, h.log(t, b, "a"))
	}
	if n := strings.Count(h.log(t, b, "big"), "kipp: KIPP_ENV big: "); n != 1 {
		t.Errorf("big: %d lines: %q", n, h.log(t, b, "big"))
	}
}
