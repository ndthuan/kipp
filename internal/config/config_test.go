package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const smoke = `
name: site-smoke
group: site
params:
  TIER: {type: choice, options: [quick, full], default: quick}
  DRY_RUN: {type: bool, default: true}
triggers:
  cron: "H 3 * * *"
  after: [build]
concurrency: drop
locks: [checkout]
timeout: 2h
keep: {builds: 50}
env: {FOO: bar}
secrets: [CHAT_TOKEN]
workdir: /tmp
stages:
  lint: {run: make lint}
  unit: {run: make test}
  smoke: {needs: [lint, unit], run: make smoke TIER=$TIER, junit: reports/*.xml}
on_failure: {run: ./notify.sh failed}
`

func TestParsesAFullJobKeepingTheFilesOrder(t *testing.T) {
	j, err := ParseJob("site-smoke.yml", []byte(smoke))
	if err != nil {
		t.Fatal(err)
	}
	if j.Name != "site-smoke" || j.Group != "site" || j.Concurrency != Drop || j.Timeout != 2*time.Hour || j.Keep.Builds != 50 {
		t.Fatalf("%+v", j)
	}
	if len(j.Params) != 2 || j.Params[0].Name != "TIER" || j.Params[1].Name != "DRY_RUN" || j.Params[1].Type != "bool" {
		t.Fatalf("params %+v", j.Params)
	}
	if len(j.Stages) != 3 || j.Stages[0].Name != "lint" || j.Stages[2].Name != "smoke" || j.Stages[2].Junit != "reports/*.xml" {
		t.Fatalf("stages %+v", j.Stages)
	}
	if j.Triggers.Cron != "H 3 * * *" || j.Triggers.After[0] != "build" || j.OnFailure.Run != "./notify.sh failed" {
		t.Fatalf("%+v", j)
	}
	if n := j.Needs(); len(n["smoke"]) != 2 {
		t.Fatalf("needs %v", n)
	}
}

func TestDefaultsAreQueueHundredBuildsNoTimeout(t *testing.T) {
	j, err := ParseJob("a.yml", []byte("name: a\nstages:\n  s: {run: true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if j.Concurrency != Queue || j.Keep.Builds != 100 || j.Keep.Days != 0 || j.Timeout != 0 {
		t.Fatalf("%+v", j)
	}
}

func TestParseJobRefusesWhatCouldNotRunAsWritten(t *testing.T) {
	cases := map[string]string{
		"name differs from file":  "name: b\nstages:\n  s: {run: true}\n",
		"unknown key":             "name: a\nstagez:\n  s: {run: true}\n",
		"no stages and no source": "name: a\n",
		"stages and source":       "name: a\nsource: {repo: x/y, path: p}\nstages:\n  s: {run: true}\n",
		"stage without run":       "name: a\nstages:\n  s: {}\n",
		"need cycle":              "name: a\nstages:\n  s: {run: x, needs: [t]}\n  t: {run: y, needs: [s]}\n",
		"bad cron":                "name: a\ntriggers: {cron: '* * *'}\nstages:\n  s: {run: true}\n",
		"bad concurrency":         "name: a\nconcurrency: maybe\nstages:\n  s: {run: true}\n",
		"choice without options":  "name: a\nparams:\n  P: {type: choice}\nstages:\n  s: {run: true}\n",
		"bad param type":          "name: a\nparams:\n  P: {type: number}\nstages:\n  s: {run: true}\n",
		"bad name chars":          "name: a b\nstages:\n  s: {run: true}\n",
		"after itself":            "name: a\ntriggers: {after: [a]}\nstages:\n  s: {run: true}\n",
		"secret with equals":      "name: a\nsecrets: [A=B]\nstages:\n  s: {run: true}\n",
	}
	for why, src := range cases {
		if _, err := ParseJob("a.yml", []byte(src)); err == nil {
			t.Errorf("%s: accepted", why)
		}
	}
}

func TestAWebhookBranchIsAnExactNameOrAPathMatchPattern(t *testing.T) {
	w := &Webhook{Repo: "o/r", Branches: []string{"main", "APP-*", "epic/APP-*"}}
	cases := map[string]bool{
		"main":            true,
		"APP-12-a-fix":    true,
		"epic/APP-3-big":  true,
		"APP-":            true,
		"mainline":        false,
		"APP":             false,
		"feature/APP-1-x": false,
		"epic/APP-3/deep": false,
		"app-1-lower":     false,
	}
	for branch, want := range cases {
		if got := w.Watches(branch); got != want {
			t.Errorf("%s: watched %v, want %v", branch, got, want)
		}
	}
	if !(&Webhook{Repo: "o/r"}).Watches("anything/at-all") {
		t.Error("a webhook without branches does not watch every branch")
	}
}

func TestAWebhookTagIsAPatternAndABranchPushAndATagPushNeverMatchEachOther(t *testing.T) {
	tags := &Webhook{Repo: "o/r", Tags: []string{"v*", "release/*"}}
	for tag, want := range map[string]bool{"v1.2.0": true, "v": true, "release/2026": true, "release/a/b": false, "1.2.0": false, "V1": false} {
		if got := tags.WatchesTag(tag); got != want {
			t.Errorf("tag %s: watched %v, want %v", tag, got, want)
		}
	}
	if tags.Watches("v1") || tags.Watches("main") {
		t.Error("a webhook with tags alone watches a branch")
	}
	both := &Webhook{Repo: "o/r", Branches: []string{"main"}, Tags: []string{"v*"}}
	if !both.Watches("main") || !both.WatchesTag("v2") || both.Watches("v2") || both.WatchesTag("main") {
		t.Error("a webhook with branches and tags does not watch each by its own list")
	}
	if (&Webhook{Repo: "o/r", Branches: []string{"*"}}).WatchesTag("v1") || (&Webhook{Repo: "o/r"}).WatchesTag("v1") {
		t.Error("a webhook without tags watches a tag")
	}
}

func TestABadWebhookTagPatternIsAnErrorNamingTheJob(t *testing.T) {
	_, err := ParseJob("rel.yml", []byte("name: rel\ntriggers: {webhook: {repo: o/r, tags: [\"v[\"]}}\nstages:\n  s: {run: true}\n"))
	if err == nil || !strings.Contains(err.Error(), "rel") || !strings.Contains(err.Error(), "triggers.webhook.tags") || !strings.Contains(err.Error(), "v[") {
		t.Fatalf("got %v, want an error naming the job, the key and the pattern", err)
	}
	if _, err := ParseJob("rel.yml", []byte("name: rel\ntriggers: {webhook: {repo: o/r, tags: [\"v*\"]}}\nstages:\n  s: {run: true}\n")); err != nil {
		t.Fatalf("a webhook with tags alone was refused: %v", err)
	}
}

func TestABadWebhookBranchPatternIsAnErrorNamingTheJob(t *testing.T) {
	_, err := ParseJob("ci.yml", []byte("name: ci\ntriggers: {webhook: {repo: o/r, branches: [main, \"APP-[\"]}}\nstages:\n  s: {run: true}\n"))
	if err == nil {
		t.Fatal("a pattern path.Match cannot read was accepted")
	}
	if !strings.Contains(err.Error(), "ci") || !strings.Contains(err.Error(), "APP-[") || !strings.Contains(err.Error(), "triggers.webhook.branches") {
		t.Fatalf("the error does not name the job, the key and the pattern: %v", err)
	}
}

func TestResolveParamsFillsDefaultsAndRefusesUnknownNamesAndBadValues(t *testing.T) {
	j, _ := ParseJob("site-smoke.yml", []byte(smoke))
	got, err := j.ResolveParams(map[string]string{"TIER": "full"})
	if err != nil || got["TIER"] != "full" || got["DRY_RUN"] != "true" {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := j.ResolveParams(map[string]string{"TIER": "nightly"}); err == nil {
		t.Fatal("choice outside options accepted")
	}
	if _, err := j.ResolveParams(map[string]string{"DRY_RUN": "yes"}); err == nil {
		t.Fatal("bool not true/false accepted")
	}
	if _, err := j.ResolveParams(map[string]string{"NOPE": "1"}); err == nil {
		t.Fatal("unknown param accepted")
	}
}

func TestLoadReportsEveryBadFileAndKeepsNothing(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "good.yml"), []byte("name: good\nstages:\n  s: {run: true}\n"))
	write(t, filepath.Join(dir, "bad1.yml"), []byte("name: other\nstages:\n  s: {run: true}\n"))
	write(t, filepath.Join(dir, "bad2.yml"), []byte("name: bad2\n"))
	write(t, filepath.Join(dir, "notes.txt"), []byte("ignored"))
	_, err := Load(dir)
	var errs *Errors
	if !errors.As(err, &errs) || len(errs.Items) != 2 {
		t.Fatalf("err %v", err)
	}
	if !strings.Contains(err.Error(), "bad1.yml") || !strings.Contains(err.Error(), "bad2.yml") {
		t.Fatalf("err %v", err)
	}
}

func TestLoadSnapshotIsSortedAndHashed(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "b.yml"), []byte("name: b\nstages:\n  s: {run: true}\n"))
	write(t, filepath.Join(dir, "a.yml"), []byte("name: a\nstages:\n  s: {run: true}\n"))
	s1, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s1.Names) != 2 || s1.Names[0] != "a" || s1.Jobs["b"] == nil || s1.Hash == "" {
		t.Fatalf("%+v", s1)
	}
	write(t, filepath.Join(dir, "a.yml"), []byte("name: a\nstages:\n  s: {run: false}\n"))
	s2, _ := Load(dir)
	if s2.Hash == s1.Hash {
		t.Fatal("hash did not change with the content")
	}
}

func TestAfterMustNameAJobInTheSet(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.yml"), []byte("name: a\ntriggers: {after: [zzz]}\nstages:\n  s: {run: true}\n"))
	if _, err := Load(dir); err == nil {
		t.Fatal("after an unknown job accepted")
	}
}

func TestParseStagesForASourceFile(t *testing.T) {
	st, err := stagesOf([]byte("stages:\n  build: {run: make}\n  test: {run: make test, needs: [build]}\n"))
	if err != nil || len(st) != 2 || st[1].Needs[0] != "build" {
		t.Fatalf("%v %v", st, err)
	}
	if _, err := stagesOf([]byte("stages:\n  b: {run: make}\nsecrets: [X]\n")); err == nil {
		t.Fatal("a source file may carry only stages")
	}
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAnUnknownKeyASecondDocumentAndAnEmptyFileAreRefused(t *testing.T) {
	jobs := map[string]string{
		"stage typo":      "name: a\nstages:\n  s: {run: true, nedes: [q]}\n",
		"param typo":      "name: a\nparams:\n  P: {def" + "ualt: x}\nstages:\n  s: {run: true}\n",
		"second document": "name: a\nstages:\n  s: {run: true}\n---\nname: b\n",
		"empty file":      "",
	}
	for why, src := range jobs {
		if _, err := ParseJob("a.yml", []byte(src)); err == nil {
			t.Errorf("%s: accepted", why)
		}
	}
	stages := map[string]string{
		"stage typo":      "stages:\n  s: {run: true, junt: x}\n",
		"second document": "stages:\n  s: {run: true}\n---\nstages:\n  t: {run: true}\n",
		"empty file":      "",
	}
	for why, src := range stages {
		if _, err := stagesOf([]byte(src)); err == nil {
			t.Errorf("ParseStages %s: accepted", why)
		}
	}
	_, err := ParseJob("a.yml", nil)
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty file error: %v", err)
	}
}

func jobWithAfter(name string, after ...string) string {
	return "name: " + name + "\ntriggers: {after: [" + strings.Join(after, ", ") + "]}\nstages:\n  s: {run: true}\n"
}

func TestAfterLoopsAreRefusedAndChainsLoad(t *testing.T) {
	loops := map[string][]string{
		"two":   {"a:b", "b:a"},
		"three": {"a:c", "b:a", "c:b"},
	}
	for why, jobs := range loops {
		dir := t.TempDir()
		for _, j := range jobs {
			name, after, _ := strings.Cut(j, ":")
			write(t, filepath.Join(dir, name+".yml"), []byte(jobWithAfter(name, after)))
		}
		_, err := Load(dir)
		if err == nil || !strings.Contains(err.Error(), "cycle") {
			t.Fatalf("%s loop: %v", why, err)
		}
		for _, j := range jobs {
			if name, _, _ := strings.Cut(j, ":"); !strings.Contains(err.Error(), name) {
				t.Errorf("%s loop: error does not name %s: %v", why, name, err)
			}
		}
	}
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.yml"), []byte(jobWithAfter("a")))
	write(t, filepath.Join(dir, "b.yml"), []byte(jobWithAfter("b", "a")))
	write(t, filepath.Join(dir, "c.yml"), []byte(jobWithAfter("c", "b")))
	if _, err := Load(dir); err != nil {
		t.Fatalf("chain: %v", err)
	}
	// Two ways from d up to a are not a way round.
	dir = t.TempDir()
	write(t, filepath.Join(dir, "a.yml"), []byte(jobWithAfter("a")))
	write(t, filepath.Join(dir, "b.yml"), []byte(jobWithAfter("b", "a")))
	write(t, filepath.Join(dir, "c.yml"), []byte(jobWithAfter("c", "a")))
	write(t, filepath.Join(dir, "d.yml"), []byte(jobWithAfter("d", "b", "c")))
	if _, err := Load(dir); err != nil {
		t.Fatalf("diamond: %v", err)
	}
}

func TestANameIsSafeAsAFileNameAndAnEnvNameAsAShellVariable(t *testing.T) {
	for name, want := range map[string]bool{"smoke": true, "a.b-c_1": true, "9lives": true, "": false, ".hidden": false, "Upper": false, "a/b": false, "a b": false, "..": false} {
		if got := ValidName(name); got != want {
			t.Errorf("ValidName(%q): got %v, want %v", name, got, want)
		}
	}
	for name, want := range map[string]bool{"TIER": true, "_x9": true, "lower": true, "": false, "9X": false, "A-B": false, "A=B": false, "A B": false} {
		if got := ValidEnvName(name); got != want {
			t.Errorf("ValidEnvName(%q): got %v, want %v", name, got, want)
		}
	}
}

func TestAStageMayNotTakeTheNameOfOneOfTheBuildsOwnLogs(t *testing.T) {
	for _, n := range []string{HooksLog, CloneLog, PrepareLog} {
		_, err := ParseJob("a.yml", []byte("name: a\nstages:\n  "+n+": {run: true}\n"))
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("ParseJob %s: %v", n, err)
		}
		_, err = stagesOf([]byte("stages:\n  " + n + ": {run: true}\n"))
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("ParseStages %s: %v", n, err)
		}
	}
}

// The web form that starts a build sends its CSRF token in a field of that name.
func TestAParamMayNotTakeTheFormsCSRFField(t *testing.T) {
	_, err := ParseJob("a.yml", []byte("name: a\nparams:\n  csrf: {}\nstages:\n  s: {run: true}\n"))
	if err == nil || !strings.Contains(err.Error(), `param "csrf"`) || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("a param called csrf: %v", err)
	}
	if _, err := ParseJob("a.yml", []byte("name: a\nparams:\n  CSRF: {}\nstages:\n  s: {run: true}\n")); err != nil {
		t.Fatalf("only the exact name is taken: %v", err)
	}
}

const textJob = "name: a\nparams:\n  NOTE: {type: text, default: \"one\\ntwo\\n\"}\n  BLANK: {type: text}\n  WHO: {default: world}\nstages:\n  s: {run: true}\n"

// A text param is a string of several lines: a request or instructions
// that span lines.
func TestATextParamKeepsItsLines(t *testing.T) {
	j, err := ParseJob("a.yml", []byte(textJob))
	if err != nil {
		t.Fatal(err)
	}
	if j.Params[0].Type != "text" || j.Params[0].Default != "one\ntwo\n" {
		t.Fatalf("the text param: %+v", j.Params[0])
	}
	got, err := j.ResolveParams(nil)
	if err != nil || got["NOTE"] != "one\ntwo\n" || got["BLANK"] != "" || got["WHO"] != "world" {
		t.Fatalf("defaults: %q %v", got, err)
	}
	const lines = "first\r\n  second\n\nlast"
	got, err = j.ResolveParams(map[string]string{"NOTE": lines})
	if err != nil || got["NOTE"] != lines {
		t.Fatalf("a text value is kept as it came: %q %v", got["NOTE"], err)
	}
}

// What to type into a param is said beside it, on the build form: a comment
// in the job's file is read by whoever opens the file, not by whoever builds.
func TestAParamMayCarryADescriptionOfAnyNumberOfLines(t *testing.T) {
	j, err := ParseJob("a.yml", []byte("name: a\nparams:\n  SECONDS: {description: \"  Seconds to order, 90 to 165.\\nEmpty is 120.\\n\"}\n  WHO: {default: world}\nstages:\n  s: {run: true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := j.Params[0].Description; got != "Seconds to order, 90 to 165.\nEmpty is 120." {
		t.Fatalf("the description, trimmed: %q", got)
	}
	if j.Params[1].Description != "" {
		t.Fatalf("a param with none: %q", j.Params[1].Description)
	}
	_, err = ParseJob("a.yml", []byte("name: a\nparams:\n  WHO: {description: \"a\\0b\"}\nstages:\n  s: {run: true}\n"))
	if err == nil || !strings.Contains(err.Error(), "description") {
		t.Fatalf("a description with a zero byte: %v", err)
	}
}

func TestAStringParamIsOneLineAndNoParamHoldsAZeroByte(t *testing.T) {
	j, err := ParseJob("a.yml", []byte(textJob))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"a\nb", "a\rb", "a\n"} {
		if _, err := j.ResolveParams(map[string]string{"WHO": v}); err == nil || !strings.Contains(err.Error(), "param WHO") || !strings.Contains(err.Error(), "one line") {
			t.Errorf("a string param took %q: %v", v, err)
		}
	}
	// No environment holds a zero byte: the stage would not start.
	for _, name := range []string{"NOTE", "WHO"} {
		if _, err := j.ResolveParams(map[string]string{name: "a\x00b"}); err == nil {
			t.Errorf("%s took a zero byte", name)
		}
	}
	_, err = ParseJob("a.yml", []byte("name: a\nparams:\n  WHO: {default: \"one\\ntwo\"}\nstages:\n  s: {run: true}\n"))
	if err == nil || !strings.Contains(err.Error(), "one line") {
		t.Fatalf("a string default of two lines: %v", err)
	}
}

func TestTheSampleJobsHoldATextParam(t *testing.T) {
	snap, err := Load("../../testdata/jobs")
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range snap.Jobs {
		for _, p := range j.Params {
			if p.Type == "text" {
				return
			}
		}
	}
	t.Fatal("no text param in testdata/jobs, so kipp check does not cover the type")
}

// An alias inside a stage may point at an anchor anywhere in the file.
func TestAStageMayUseAnAnchorDefinedOutsideIt(t *testing.T) {
	j, err := ParseJob("a.yml", []byte("name: a\ndescription: &cmd make test\nstages:\n  s: {run: *cmd}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if j.Stages[0].Run != "make test" {
		t.Fatalf("run: got %q", j.Stages[0].Run)
	}
}

// An anchor that holds an alias to itself has no end; it is refused, and
// reading it must not run away.
func TestAnAnchorThatHoldsItselfIsRefused(t *testing.T) {
	_, err := ParseJob("a.yml", []byte("name: a\nstages:\n  s: &loop {run: x, needs: [*loop]}\n"))
	if err == nil || !strings.Contains(err.Error(), "holds itself") {
		t.Fatalf("got %v", err)
	}
}

func TestAMergeKeyInsideAStageIsRead(t *testing.T) {
	j, err := ParseJob("a.yml", []byte("name: a\nenv: &base {X: y}\nstages:\n  s: &first {run: make a}\n  t: {<<: *first, needs: [s]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if j.Stages[1].Run != "make a" || len(j.Stages[1].Needs) != 1 {
		t.Fatalf("%+v", j.Stages[1])
	}
}

// Each level doubles the one before: thirty of them stand for a billion
// values, which must be refused before they are written out.
func TestAliasesThatMultiplyAreRefusedBeforeTheyAreExpanded(t *testing.T) {
	var doc strings.Builder
	doc.WriteString("name: a\nlocks:\n  - &l0 x\n")
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&doc, "  - &l%d [*l%d, *l%d]\n", i, i-1, i-1)
	}
	doc.WriteString("stages:\n  s: {run: x, needs: *l30}\n")
	done := make(chan error, 1)
	go func() {
		_, err := ParseJob("a.yml", []byte(doc.String()))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "once its aliases are written out") {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still expanding after 5s")
	}
}

// The limit is on the file, not on each stage: many stages that each use
// one large anchor would otherwise cost the limit once apiece.
func TestTheAliasLimitIsSharedByAllOfAFilesStages(t *testing.T) {
	var doc strings.Builder
	doc.WriteString("name: a\nlocks: &big [" + strings.TrimSuffix(strings.Repeat("x, ", 6000), ", ") + "]\nstages:\n")
	for i := range 3 {
		fmt.Fprintf(&doc, "  s%d: {run: x, needs: *big}\n", i)
	}
	_, err := ParseJob("a.yml", []byte(doc.String()))
	if err == nil || !strings.Contains(err.Error(), "once its aliases are written out") {
		t.Fatalf("got %v", err)
	}
}

// An alias as a key would name the stage after its anchor, not its value.
func TestAStageOrParamKeyMustBeAPlainName(t *testing.T) {
	for what, doc := range map[string]string{
		"an alias as a stage's name": "name: a\ndescription: &k build\nstages:\n  *k : {run: x}\n",
		"an alias as a param's name": "name: a\ndescription: &k TIER\nparams:\n  *k : {default: x}\nstages:\n  s: {run: x}\n",
		"a list as a stage's name":   "name: a\nstages:\n  [a, b]: {run: x}\n",
	} {
		_, err := ParseJob("a.yml", []byte(doc))
		if err == nil || !strings.Contains(err.Error(), "a key must be a plain name") {
			t.Errorf("%s: got %v", what, err)
		}
	}
}

func TestAJobIsDisabledOnlyWhenItsFileSaysSo(t *testing.T) {
	const body = "triggers: {cron: '* * * * *'}\nstages:\n  s: {run: 'true'}\n"
	for src, want := range map[string]bool{"": false, "disabled: false\n": false, "disabled: true\n": true} {
		j, err := ParseJob("off.yml", []byte("name: off\n"+src+body))
		if err != nil || j.Disabled != want {
			t.Fatalf("%q: disabled %v, err %v", src, j, err)
		}
	}
	if _, err := ParseJob("off.yml", []byte("name: off\ndisabled: for now\n"+body)); err == nil {
		t.Fatal("a disabled that is neither true nor false was taken")
	}
}

// longJob is a job file with twenty params and twenty stages, then one more
// of each as given: what is wrong with those is far down the file.
func longJob(param, stage string) string {
	var doc strings.Builder
	doc.WriteString("name: a\nparams:\n") // lines 1 and 2
	for i := range 20 {
		fmt.Fprintf(&doc, "  P%d: {default: x}\n", i) // lines 3 to 22
	}
	doc.WriteString("  LAST:\n    type: string\n" + param) // LAST on line 23, param from line 25
	doc.WriteString("stages:\n")                           // line 26
	for i := range 20 {
		fmt.Fprintf(&doc, "  s%d:\n    run: x\n", i) // lines 27 to 66
	}
	doc.WriteString("  deep:\n    run: x\n" + stage) // deep on line 67, stage from line 69
	return doc.String()
}

func TestAnErrorInsideAParamOrAStageNamesItsLineInTheFile(t *testing.T) {
	if _, err := ParseJob("a.yml", []byte(longJob("    default: x\n", "    needs: [s0]\n"))); err != nil {
		t.Fatal(err)
	}
	for what, c := range map[string]struct{ param, stage, want string }{
		"a key of a param that is not one":   {"    dflt: x\n", "    needs: [s0]\n", "LAST: line 25: field dflt not found"},
		"a key of a stage that is not one":   {"    default: x\n", "    nedes: [s0]\n", "deep: line 69: field nedes not found"},
		"a value of the wrong kind":          {"    default: x\n", "    needs: {s0: s1}\n", "line 69: cannot unmarshal"},
		"a key that came in through a merge": {"    default: x\n", "    <<: {junit: r.xml, junt: r.xml}\n", "deep: line 69: field junt not found"},
	} {
		_, err := ParseJob("a.yml", []byte(longJob(c.param, c.stage)))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q in it", what, err, c.want)
		}
	}
	// A stage that is a list is the decoder's to refuse, in its own words.
	if _, err := stagesOf([]byte("stages:\n  a:\n    - {nope: x}\n")); err == nil || !strings.Contains(err.Error(), "line 3: cannot unmarshal !!seq") {
		t.Errorf("a stage that is a list: got %v", err)
	}
	if _, err := stagesOf([]byte("stages:\n  a: {run: x}\n  b:\n    <<: [{run: x}, {junt: r.xml}]\n")); err == nil || !strings.Contains(err.Error(), "b: line 4: field junt not found") {
		t.Errorf("a merge of several: got %v", err)
	}
	// A source file's stages are read the same way.
	_, err := stagesOf([]byte("stages:\n  a: {run: x}\n  b:\n    run: x\n    junt: r.xml\n"))
	if err == nil || !strings.Contains(err.Error(), "b: line 5: field junt not found") {
		t.Errorf("a source file: got %v", err)
	}
}

func TestTheWorkdirOfASourceJobIsAPathInsideItsClone(t *testing.T) {
	const job = "name: a\nsource: {repo: o/r, path: ci.yml}\nworkdir: %s\n"
	for _, dir := range []string{"ci", "services/api", "."} {
		j, err := ParseJob("a.yml", fmt.Appendf(nil, job, dir))
		if err != nil || j.Workdir != dir {
			t.Errorf("%s: %v, %v", dir, j, err)
		}
	}
	for _, dir := range []string{"/srv/site", "..", "ci/../../elsewhere"} {
		_, err := ParseJob("a.yml", fmt.Appendf(nil, job, dir))
		if err == nil || !strings.Contains(err.Error(), "inside the clone") {
			t.Errorf("%s: got %v", dir, err)
		}
	}
	// With stages of its own a job runs where it says, as before.
	if j, err := ParseJob("a.yml", []byte("name: a\nworkdir: /srv/site\nstages:\n  s: {run: x}\n")); err != nil || j.Workdir != "/srv/site" {
		t.Errorf("%v, %v", j, err)
	}
}

// A directory of .yaml files would otherwise load as no jobs at all, and
// kipp check would call it fine.
func TestAJobFileNamedYamlIsAnErrorThatSaysToRenameIt(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "good.yml"), []byte("name: good\nstages:\n  s: {run: x}\n"))
	write(t, filepath.Join(dir, "smoke.yaml"), []byte("name: smoke\nstages:\n  s: {run: x}\n"))
	write(t, filepath.Join(dir, "notes.txt"), []byte("not a job"))
	if err := os.Mkdir(filepath.Join(dir, "old.yaml"), 0o750); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	var errs *Errors
	if !errors.As(err, &errs) || len(errs.Items) != 1 {
		t.Fatalf("got %v", err)
	}
	if got, want := err.Error(), "smoke.yaml: a job file ends in .yml; rename it to smoke.yml"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestAStageNamesTheExitCodesThatEndItUnstable(t *testing.T) {
	j, err := ParseJob("a.yml", []byte("name: a\nstages:\n  s: {run: make smoke, unstable_exit: [4, 75]}\n  t: {run: make}\n"))
	if err != nil {
		t.Fatal(err)
	}
	s, plain := j.Stages[0], j.Stages[1]
	if !slices.Equal(s.UnstableExit, ExitCodes{4, 75}) || plain.UnstableExit != nil {
		t.Fatalf("unstable_exit %v and %v", s.UnstableExit, plain.UnstableExit)
	}
	for code, want := range map[int]bool{4: true, 75: true, 0: false, 1: false, 5: false} {
		if s.Unstable(code) != want {
			t.Errorf("a stage with unstable_exit [4, 75] exiting %d: unstable %v, want %v", code, !want, want)
		}
	}
	if plain.Unstable(4) || plain.Unstable(0) {
		t.Fatal("a stage with no unstable_exit has an unstable exit code")
	}
	st, err := stagesOf([]byte("stages:\n  b: {run: make, unstable_exit: [255]}\n  c: {run: make, unstable_exit: }\n"))
	if err != nil || !st[0].Unstable(255) || st[1].UnstableExit != nil {
		t.Fatalf("a source file's stages, the second with the key left empty: %v %v", st, err)
	}
}

func TestUnstableExitIsAListOfDistinctCodesFromOneTo255(t *testing.T) {
	cases := map[string]string{
		"zero, which is success":   "[0]",
		"zero among others":        "[4, 0]",
		"a negative code":          "[-1]",
		"a code above 255":         "[256]",
		"a code twice":             "[4, 5, 4]",
		"an empty list":            "[]",
		"a code that is no number": "[four]",
		"a fraction":               "[4.5]",
		"one code, not a list":     "4",
		"a mapping":                "{code: 4}",
		"a list inside the list":   "[[4]]",
		"a code as text":           `["4"]`,
		"a code in hex":            "[0x10]",
	}
	for why, value := range cases {
		src := "name: a\nstages:\n  s: {run: make, unstable_exit: " + value + "}\n"
		_, err := ParseJob("a.yml", []byte(src))
		if err == nil {
			t.Errorf("%s: accepted", why)
			continue
		}
		if !strings.Contains(err.Error(), "s: line 3: unstable_exit") {
			t.Errorf("%s: the error does not name the stage, the line and the key: %v", why, err)
		}
		if _, err := stagesOf([]byte("stages:\n  s: {run: make, unstable_exit: " + value + "}\n")); err == nil {
			t.Errorf("%s: accepted in a source file", why)
		}
	}
	for _, src := range []string{
		"name: a\nstages:\n  s: {run: make, unstable_exits: [4]}\n",
		"name: a\nunstable_exit: [4]\nstages:\n  s: {run: make}\n",
		"name: a\nstages:\n  s: {run: make}\non_success: {run: make, unstable_exit: [4]}\n",
	} {
		if _, err := ParseJob("a.yml", []byte(src)); err == nil {
			t.Errorf("accepted a key that is not a stage's unstable_exit:\n%s", src)
		}
	}
}

func TestTheSampleJobsHoldAStageWithAnUnstableExit(t *testing.T) {
	snap, err := Load("../../testdata/jobs")
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range snap.Jobs {
		for _, s := range j.Stages {
			if len(s.UnstableExit) > 0 {
				return
			}
		}
	}
	t.Fatal("no unstable_exit in testdata/jobs, so kipp check does not cover the key")
}

// YAML reads 010 as eight and a shell reads it as ten: neither is what the
// file would be taken to say, so a code is written without a leading zero.
func TestAnUnstableExitCodeIsNotWrittenWithALeadingZero(t *testing.T) {
	for _, value := range []string{"[010]", "[04]", "[08]", "[4, 00]"} {
		_, err := ParseJob("a.yml", []byte("name: a\nstages:\n  s: {run: make, unstable_exit: "+value+"}\n"))
		if err == nil || !strings.Contains(err.Error(), "s: line 3: unstable_exit") || !strings.Contains(err.Error(), "leading zero") {
			t.Errorf("%s: got %v, want a refusal that names the leading zero", value, err)
		}
	}
	if j, err := ParseJob("a.yml", []byte("name: a\nstages:\n  s: {run: make, unstable_exit: [10, 100]}\n")); err != nil || !j.Stages[0].Unstable(100) {
		t.Fatalf("a zero inside or at the end of a code: %v", err)
	}
}

func TestAnUnstableExitCodeWrittenAsTextIsToldItMustBeANumber(t *testing.T) {
	for _, value := range []string{`["4"]`, "[!!str 4]", "[four]"} {
		_, err := ParseJob("a.yml", []byte("name: a\nstages:\n  s: {run: make, unstable_exit: "+value+"}\n"))
		if err == nil || !strings.Contains(err.Error(), "s: line 3: unstable_exit") || !strings.Contains(err.Error(), "a number, not text") {
			t.Errorf("%s: got %v, want a refusal that says a code is a number, not text", value, err)
		}
		if err != nil && strings.Contains(err.Error(), "from 1 to 255") {
			t.Errorf("%s: the refusal reads as if the code were out of range: %v", value, err)
		}
	}
	// A number out of range is still told the range.
	_, err := ParseJob("a.yml", []byte("name: a\nstages:\n  s: {run: make, unstable_exit: [256]}\n"))
	if err == nil || !strings.Contains(err.Error(), "from 1 to 255") || strings.Contains(err.Error(), "not text") {
		t.Errorf("[256]: got %v", err)
	}
}

func TestAJobMayNameTheTimeZoneItsCronIsReadIn(t *testing.T) {
	j, err := ParseJob("tz.yml", []byte("name: tz\ntimezone: Asia/Tokyo\ntriggers: {cron: '0 9 * * *'}\nstages:\n  s: {run: 'true'}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if j.TimeZone == nil || j.TimeZone.String() != "Asia/Tokyo" {
		t.Fatalf("time zone %v", j.TimeZone)
	}
	server := time.FixedZone("server", 7*3600)
	if got := j.Zone(server); got != j.TimeZone {
		t.Fatalf("the job's own zone gave way to %v", got)
	}

	j, err = ParseJob("tz.yml", []byte("name: tz\ntriggers: {cron: '0 9 * * *'}\nstages:\n  s: {run: 'true'}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if j.TimeZone != nil {
		t.Fatalf("a job that names no zone has one: %v", j.TimeZone)
	}
	if got := j.Zone(server); got != server {
		t.Fatalf("a job with no zone of its own is read in %v, not the server's", got)
	}
	if got := j.Zone(nil); got != time.Local {
		t.Fatalf("with no zone of the job's or the server's, %v and not the host's", got)
	}
}

func TestATimeZoneIsAnIANANameOnAJobWithACron(t *testing.T) {
	for why, src := range map[string]string{
		"not a zone": "timezone: Mars/Olympus\ntriggers: {cron: '0 9 * * *'}\n",
		"Local":      "timezone: Local\ntriggers: {cron: '0 9 * * *'}\n",
		"an offset":  "timezone: '+07:00'\ntriggers: {cron: '0 9 * * *'}\n",
		"no cron":    "timezone: Asia/Tokyo\n",
		"blank":      "timezone: ' '\ntriggers: {cron: '0 9 * * *'}\n",
	} {
		_, err := ParseJob("tz.yml", []byte("name: tz\n"+src+"stages:\n  s: {run: 'true'}\n"))
		if err == nil || !strings.Contains(err.Error(), "timezone") {
			t.Errorf("%s: %v", why, err)
		}
	}
	if _, err := ParseJob("tz.yml", []byte("name: tz\ntimezone: UTC\ntriggers: {cron: '0 9 * * *'}\nstages:\n  s: {run: 'true'}\n")); err != nil {
		t.Errorf("UTC: %v", err)
	}
}

func TestLoadZoneTakesAnIANANameAndNothingElse(t *testing.T) {
	if z, err := LoadZone("Asia/Tokyo"); err != nil || z.String() != "Asia/Tokyo" {
		t.Fatalf("%v %v", z, err)
	}
	for _, bad := range []string{"", "Local", "local", "Nowhere/City", "+07:00", "../etc/passwd"} {
		if _, err := LoadZone(bad); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
}

func TestRetriesIsAWholeNumberFromZeroToTen(t *testing.T) {
	for _, v := range []string{"0", "1", "10"} {
		j, err := ParseJob("a.yml", []byte("name: a\nstages:\n  s: {run: make, retries: "+v+"}\n"))
		if err != nil {
			t.Errorf("retries %s refused: %v", v, err)
			continue
		}
		if got := strconv.Itoa(int(j.Stages[0].Retries)); got != v {
			t.Errorf("retries %s read as %s", v, got)
		}
	}
	if j, err := ParseJob("a.yml", []byte("name: a\nstages:\n  s: {run: make}\n")); err != nil || j.Stages[0].Retries != 0 {
		t.Fatalf("a stage with no retries: %v", err)
	}
	for _, v := range []string{"11", "-1", "two", "1.5", `"2"`, "[1]"} {
		src := "name: a\nstages:\n  s: {run: make, retries: " + v + "}\n"
		if _, err := ParseJob("a.yml", []byte(src)); err == nil {
			t.Errorf("retries %s accepted", v)
		}
		if _, err := stagesOf([]byte("stages:\n  s: {run: make, retries: " + v + "}\n")); err == nil {
			t.Errorf("retries %s accepted in a source file", v)
		}
	}
}
