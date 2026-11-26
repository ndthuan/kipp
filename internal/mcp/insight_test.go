package mcp

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/store"
)

func (e *env) keep(t *testing.T, b *store.Build, stage, path string, body []byte) {
	t.Helper()
	full := filepath.Join(e.d.StageArtifactsDir(b.Job, b.Number, stage), filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.st.PutArtifacts(context.Background(), b.ID, stage, []store.Artifact{{Stage: stage, Path: path, Size: int64(len(body)), SHA256: "ab"}}); err != nil {
		t.Fatal(err)
	}
}

func TestReadArtifactReadsAKeptTextFileByTheByteAndRefusesABinaryOne(t *testing.T) {
	e := newEnv(t, nil)
	s := e.session(t, e.admin)
	b := e.built(t, "smoke", nil)
	e.keep(t, b, "s", "out/report.txt", []byte("first line\nsecond line\n"))
	e.keep(t, b, "t", "bin/tool", []byte("\x7fELF\x00\x01"))
	args := func(stage, path string, more map[string]any) map[string]any {
		m := map[string]any{"job": "smoke", "number": b.Number, "stage": stage, "path": path}
		maps.Copy(m, more)
		return m
	}
	var got readArtifactOut
	ok(t, s, "read_artifact", args("s", "out/report.txt", nil), &got)
	if got.Text != "first line\nsecond line\n" || got.Size != 23 || got.Truncated || got.Next != 23 {
		t.Fatalf("whole file: %+v", got)
	}
	ok(t, s, "read_artifact", args("s", "out/report.txt", map[string]any{"offset": 11, "limit_bytes": 6}), &got)
	if got.Text != "second" || got.Offset != 11 || got.Next != 17 || !got.Truncated {
		t.Fatalf("a range: %+v", got)
	}
	refused(t, s, "read_artifact", args("t", "bin/tool", nil), "not text")
	refused(t, s, "read_artifact", args("s", "out/other.txt", nil), "not found")
	refused(t, s, "read_artifact", args("s", "../../../db", nil), "not found")
	refused(t, s, "read_artifact", args("s", "out/report.txt", map[string]any{"limit_bytes": maxBytes + 1}), "limit_bytes")
	refused(t, e.session(t, e.stopper), "read_artifact", args("s", "out/report.txt", nil), "not found")
}

func TestSearchLogFindsTheLinesThatMatchWithWhereEachBegins(t *testing.T) {
	e := newEnv(t, nil)
	s := e.session(t, e.admin)
	b := e.built(t, "long", nil)
	var got searchLogOut
	ok(t, s, "search_log", map[string]any{"job": "long", "number": b.Number, "stage": "s", "pattern": `^line 29\d\d$`, "max_matches": 10}, &got)
	if got.Total != 100 || len(got.Matches) != 10 || !got.Cut || got.Matches[0].Text != "line 2900" || got.Matches[9].Text != "line 2909" {
		t.Fatalf("%+v", got)
	}
	if got.Matches[1].Line != got.Matches[0].Line+1 || got.Matches[1].Offset <= got.Matches[0].Offset {
		t.Fatalf("lines and offsets do not run on: %+v", got.Matches[:2])
	}
	var at readLogOut
	ok(t, s, "read_log", map[string]any{"job": "long", "number": b.Number, "stage": "s", "offset": got.Matches[0].Offset, "limit_bytes": 9}, &at)
	if at.Text != "line 2900" {
		t.Fatalf("read_log at the match's offset reads %q", at.Text)
	}
	ok(t, s, "search_log", map[string]any{"job": "long", "number": b.Number, "stage": "s", "pattern": "LINE 3000", "ignore_case": true}, &got)
	if got.Total != 1 || got.Matches[0].Text != "line 3000" || got.Cut {
		t.Fatalf("ignoring case: %+v", got)
	}
	refused(t, s, "search_log", map[string]any{"job": "long", "number": b.Number, "stage": "s", "pattern": "("}, "bad input")
	refused(t, s, "search_log", map[string]any{"job": "long", "number": b.Number, "stage": "s", "pattern": ""}, "bad input")
	refused(t, s, "search_log", map[string]any{"job": "long", "number": b.Number, "stage": "nope", "pattern": "x"}, "not found")
}

func TestALongLineIsSearchedInFullAndAnsweredClipped(t *testing.T) {
	long := strings.Repeat("x", 3*searchBuffer) + " needle"
	matches, total, err := searchLines(context.Background(), strings.NewReader("a\n"+long+"\nneedle\n"), func(b []byte) bool { return strings.Contains(string(b), "needle") }, 10)
	if err != nil || total != 2 || matches[0].Line != 2 || len(matches[0].Text) > maxMessage+3 || matches[1].Line != 3 || matches[1].Offset != int64(2+len(long)+1) {
		t.Fatalf("%d %+v", total, matches)
	}
}

type endless struct{}

func (endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	p[len(p)-1] = '\n'
	return len(p), nil
}

func TestASearchWhoseCallerIsGoneStopsReadingTheLog(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	lines := 0
	_, _, err := searchLines(ctx, endless{}, func([]byte) bool {
		if lines++; lines == 3 {
			cancel()
		}
		return false
	}, 10)
	if !errors.Is(err, context.Canceled) || lines != 3 {
		t.Fatalf("stopped with %v after %d lines", err, lines)
	}
}

func putTests(t *testing.T, e *env, b *store.Build, tests ...store.Test) {
	t.Helper()
	if err := e.st.PutTests(context.Background(), b.ID, tests); err != nil {
		t.Fatal(err)
	}
}

func TestCompareBuildsSaysWhatChangedSinceTheBuildBefore(t *testing.T) {
	e := newEnv(t, nil)
	s := e.session(t, e.admin)
	one := e.built(t, "smoke", map[string]any{"TIER": "quick"})
	putTests(t, e, one, store.Test{Suite: "a", Name: "steady", Status: "passed"}, store.Test{Suite: "a", Name: "mended", Status: "failed"}, store.Test{Suite: "a", Name: "broken", Status: "passed"})
	two := e.built(t, "smoke", map[string]any{"TIER": "full"})
	putTests(t, e, two, store.Test{Suite: "a", Name: "steady", Status: "passed"}, store.Test{Suite: "a", Name: "mended", Status: "passed"},
		store.Test{Suite: "a", Name: "broken", Status: "failed", Message: "boom"}, store.Test{Suite: "a", Name: "new", Status: "failed"})

	var got compareOut
	ok(t, s, "compare_builds", map[string]any{"job": "smoke", "number": two.Number}, &got)
	if got.Base.Number != one.Number || got.Build.Number != two.Number {
		t.Fatalf("compared %d with %d", got.Build.Number, got.Base.Number)
	}
	if len(got.Params) != 1 || got.Params[0] != (paramChange{Name: "TIER", Base: "quick", Build: "full"}) {
		t.Fatalf("params: %+v", got.Params)
	}
	names := func(cs []caseChange) string {
		var out []string
		for _, c := range cs {
			out = append(out, c.Name)
		}
		return strings.Join(out, ",")
	}
	if names(got.NewlyFailing) != "broken,new" || names(got.Fixed) != "mended" || got.NewlyFailing[0].Message != "boom" {
		t.Fatalf("tests: newly failing %+v, fixed %+v", got.NewlyFailing, got.Fixed)
	}
	ok(t, s, "compare_builds", map[string]any{"job": "smoke", "number": one.Number, "base": two.Number}, &got)
	if names(got.NewlyFailing) != "mended" || names(got.Fixed) != "broken" {
		t.Fatalf("the other way round: %+v %+v", got.NewlyFailing, got.Fixed)
	}
	refused(t, s, "compare_builds", map[string]any{"job": "smoke", "number": one.Number}, "no earlier build")
	refused(t, s, "compare_builds", map[string]any{"job": "smoke", "number": one.Number, "base": one.Number}, "bad input")
	refused(t, e.session(t, e.stopper), "compare_builds", map[string]any{"job": "smoke", "number": two.Number}, "not found")
}

func TestTestHistoryPutsTheTestsThatFlipFirstWithAStatusPerBuild(t *testing.T) {
	e := newEnv(t, nil)
	s := e.session(t, e.admin)
	var numbers []int
	for _, flaky := range []string{"passed", "failed", "passed"} {
		b := e.built(t, "smoke", nil)
		numbers = append(numbers, b.Number)
		putTests(t, e, b, store.Test{Suite: "a", Name: "steady", Status: "passed"}, store.Test{Suite: "a", Name: "flaky", Status: flaky})
	}
	var got testHistoryOut
	ok(t, s, "test_history", map[string]any{"job": "smoke"}, &got)
	if len(got.Builds) != 3 || got.Builds[0] != numbers[2] || got.Builds[2] != numbers[0] {
		t.Fatalf("builds %v, want newest first of %v", got.Builds, numbers)
	}
	if len(got.Tests) != 2 || got.Tests[0].Name != "flaky" || got.Tests[0].Flips != 2 || got.Tests[0].Failed != 1 ||
		strings.Join(got.Tests[0].Statuses, ",") != "passed,failed,passed" || got.Tests[1].Flips != 0 {
		t.Fatalf("%+v", got.Tests)
	}
	ok(t, s, "test_history", map[string]any{"job": "smoke", "name": "STEAD"}, &got)
	if len(got.Tests) != 1 || got.Tests[0].Name != "steady" {
		t.Fatalf("by name: %+v", got.Tests)
	}
	ok(t, s, "test_history", map[string]any{"job": "smoke", "limit": 1}, &got)
	if len(got.Tests) != 1 || !got.Cut {
		t.Fatalf("limit: %+v", got)
	}
	refused(t, s, "test_history", map[string]any{"job": "smoke", "builds": maxHistory + 1}, "builds is 1 to")
	refused(t, e.session(t, e.stopper), "test_history", map[string]any{"job": "smoke"}, "not found")
}
