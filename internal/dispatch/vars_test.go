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

// the places a variable X is given, lowest first. Each gives X its own name.
var levels = []string{"server", "global", "group", "pattern", "jobfile", "stagesfile", "stage", "param"}

// precedenceHarness is a source job that gives X at each of the levels but
// those in skip, and says which of them wins: the highest left.
func precedenceHarness(t *testing.T, skip ...string) (*harness, string) {
	t.Helper()
	has := func(level string) bool { return !slices.Contains(skip, level) }
	vars, job, file, stage := "", "name: src\ngroup: site\nsource: {repo: o/r, ref: main, path: ci.yml}\n", "", "{run: 'echo $X'"
	if has("global") {
		vars += "global: {X: global}\n"
	}
	if has("group") {
		vars += "groups:\n  site: {X: group}\n"
	}
	if has("pattern") {
		vars += "jobs:\n  'sr?': {X: pattern}\n"
	}
	if has("jobfile") {
		job += "env: {X: jobfile}\n"
	}
	if has("param") {
		job += "params:\n  X: {default: param}\n"
	}
	if has("stagesfile") {
		file += "env: {X: stagesfile}\n"
	}
	if has("stage") {
		stage += ", env: {X: stage}"
	}
	if has("server") {
		t.Setenv("X", "server")
	}
	files := map[string]string{"src": job}
	if vars != "" {
		files["_vars"] = vars
	}
	h := newHarness(t, files, func(o *Options) {
		o.PassEnv = []string{"PATH", "X"}
		o.Clone = func(_ context.Context, dir, _, _ string, _ io.Writer) error {
			return os.WriteFile(filepath.Join(dir, "ci.yml"), []byte(file+"stages:\n  s: "+stage+"}\n"), 0o600)
		}
	})
	for _, level := range slices.Backward(levels) {
		if has(level) {
			return h, level
		}
	}
	return h, ""
}

func TestWhateverLevelIsLeftOutTheHighestLevelLeftWins(t *testing.T) {
	for _, skip := range append([]string{""}, levels...) {
		t.Run("without "+skip, func(t *testing.T) {
			h, want := precedenceHarness(t, skip)
			b := h.wait(t, h.trigger(t, "src").ID)
			if got := strings.TrimSpace(h.log(t, b, "s")); b.Status != store.Success || got != want {
				t.Fatalf("%v: X is %q, want %q (prepare: %q)", b.Status, got, want, h.log(t, b, config.PrepareLog))
			}
		})
	}
}

func TestEachLevelAloneBeatsEveryLevelBelowIt(t *testing.T) {
	for i := 1; i < len(levels); i++ {
		t.Run(levels[i], func(t *testing.T) {
			h, want := precedenceHarness(t, levels[i+1:]...)
			b := h.wait(t, h.trigger(t, "src").ID)
			if got := strings.TrimSpace(h.log(t, b, "s")); b.Status != store.Success || got != levels[i] || want != levels[i] {
				t.Fatalf("%v: X is %q, want %q", b.Status, got, levels[i])
			}
		})
	}
}

func TestAStagesEnvReachesThatStageAloneAndNotTheStageBesideIt(t *testing.T) {
	h := newHarness(t, map[string]string{"a": "name: a\nstages:\n  one: {env: {ONLY: here}, run: 'echo one=${ONLY-unset}'}\n  two: {run: 'echo two=${ONLY-unset}'}\n"})
	b := h.wait(t, h.trigger(t, "a").ID)
	if one, two := h.log(t, b, "one"), h.log(t, b, "two"); one != "one=here\n" || two != "two=unset\n" {
		t.Fatalf("one %q two %q", one, two)
	}
}

func TestABuildWhoseStagesFileEnvNamesWhatKippOwnsFailsInPrepareNamingTheFileAndTheKey(t *testing.T) {
	for why, c := range map[string]struct{ key, file string }{
		"a KIPP_ name in the file's env": {"KIPP_JOB", "env: {KIPP_JOB: other}\nstages:\n  s: {run: echo never}\n"},
		"CI in a stage's env":            {"CI", "stages:\n  s: {run: echo never, env: {CI: '0'}}\n"},
		"a secret the job lists":         {"TOKEN", "env: {TOKEN: plain}\nstages:\n  s: {run: echo never}\n"},
		"a secret in a stage's env":      {"TOKEN", "stages:\n  s: {run: echo never, env: {TOKEN: plain}}\n"},
	} {
		key, file := c.key, c.file
		t.Run(why, func(t *testing.T) {
			h := newHarness(t, map[string]string{"src": "name: src\nsecrets: [TOKEN]\nsource: {repo: o/r, ref: main, path: ci.yml}\non_failure: {run: 'echo hook'}\n"}, func(o *Options) {
				o.Clone = func(_ context.Context, dir, _, _ string, _ io.Writer) error {
					return os.WriteFile(filepath.Join(dir, "ci.yml"), []byte(file), 0o600)
				}
			})
			b := h.wait(t, h.trigger(t, "src").ID)
			prepare := h.log(t, b, config.PrepareLog)
			if b.Status != store.Failed || !strings.Contains(prepare, "ci.yml") || !strings.Contains(prepare, key) || strings.Contains(prepare, "plain") {
				t.Fatalf("%v: prepare log %q, want one naming ci.yml and %s", b.Status, prepare, key)
			}
			if st := h.stages(t, b.ID); len(st) != 0 {
				t.Fatalf("a stage was recorded: %v", st)
			}
		})
	}
}

func TestAGlobalVariableNamedLikeASecretFailsOnlyTheBuildsOfTheJobThatListsIt(t *testing.T) {
	h := newHarness(t, map[string]string{
		"_vars": "global: {TOKEN: shared}\n",
		"with":  "name: with\nsecrets: [TOKEN]\nstages:\n  s: {run: echo never}\n",
		"free":  "name: free\nstages:\n  s: {run: 'echo token=$TOKEN'}\n",
	})
	b := h.wait(t, h.trigger(t, "with").ID)
	if prepare := h.log(t, b, config.PrepareLog); b.Status != store.Failed || !strings.Contains(prepare, "_vars.yml") || !strings.Contains(prepare, "TOKEN") {
		t.Fatalf("%v: %q", b.Status, prepare)
	}
	if b := h.wait(t, h.trigger(t, "free").ID); b.Status != store.Success || h.log(t, b, "s") != "token=shared\n" {
		t.Fatalf("%v: %q", b.Status, h.log(t, b, "s"))
	}
}

func TestAChildThatTwoPatternsReachFailsItsBuildsInPrepareNamingBothPatterns(t *testing.T) {
	r := newRepo(t)
	r.commit("main", map[string]string{".kipp.yml": "stages:\n  s: {run: 'echo url=$URL'}\n"})
	// The parent matches 'site' alone; its child matches that by its parent's
	// name and 'site.*' by its own.
	h := newHarness(t, map[string]string{
		"_vars": "jobs:\n  'site': {URL: a}\n  'site.*': {URL: b}\n",
		"site":  parentFile(r.dir, ""),
	})
	h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"})
	b := h.wait(t, h.trigger(t, "site").ID)
	prepare := h.log(t, b, config.PrepareLog)
	if b.Status != store.Failed || !strings.Contains(prepare, `"site"`) || !strings.Contains(prepare, `"site.*"`) || !strings.Contains(prepare, "URL") {
		t.Fatalf("%v: prepare log %q", b.Status, prepare)
	}
}

func TestAChildTakesTheEnvOfItsBranchsStagesFile(t *testing.T) {
	r := newRepo(t)
	r.commit("main", map[string]string{".kipp.yml": "env: {MODE: fast}\nstages:\n  s: {env: {LEVEL: '2'}, run: 'echo $MODE $LEVEL $SHARED'}\n"})
	h := newHarness(t, map[string]string{"_vars": "jobs:\n  'site': {SHARED: from-vars}\n", "site": parentFile(r.dir, "")})
	h.withChildren(t, config.ChildRecord{Key: "main", Branch: "main"})
	b := h.wait(t, h.trigger(t, "site").ID)
	if got := h.log(t, b, "s"); b.Status != store.Success || got != "fast 2 from-vars\n" {
		t.Fatalf("%v: %q", b.Status, got)
	}
}

func TestAStageOnAnAgentGetsTheSameVariablesAsOneOnTheServer(t *testing.T) {
	ag := newAgents(t, agenthub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	ag.runBox(t.TempDir())
	const echo = "echo $G $PAT $J $F $S $P"
	h := newHarness(t, map[string]string{
		"_vars":  "global: {G: g}\njobs:\n  'remote': {PAT: p}\n",
		"remote": "name: remote\nenv: {J: j}\nparams:\n  P: {default: param}\nsource: {repo: o/r, ref: main, path: ci.yml}\n",
	}, ag.opts, func(o *Options) {
		o.Clone = func(_ context.Context, dir, _, _ string, _ io.Writer) error {
			stages := "stages:\n  s: {env: {S: s, P: stage}, run: '" + echo + "'}\n  r: {runs_on: linux, env: {S: s, P: stage}, run: '" + echo + "'}\n"
			return os.WriteFile(filepath.Join(dir, "ci.yml"), []byte("env: {F: f}\n"+stages), 0o600)
		}
	})
	b := h.wait(t, h.trigger(t, "remote").ID)
	if b.Status != store.Success {
		t.Fatalf("%v: %q", b.Status, h.log(t, b, config.PrepareLog))
	}
	const want = "g p j f s param\n"
	if local, remote := h.log(t, b, "s"), h.log(t, b, "r"); local != want || remote != want {
		t.Fatalf("server %q agent %q, want %q", local, remote, want)
	}
}
