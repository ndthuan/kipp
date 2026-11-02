package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestPutRemembersAReportAndIsAudited(t *testing.T) {
	data := newData(t)
	report := filepath.Join(t.TempDir(), "report.json")
	write(t, report, `{"metrics": [{"name": "seconds", "unit": "s", "complete": true, "values": {"a": 1.5, "b": 2}},
		{"name": "coverage", "unit": "%", "values": {"total": 87.19}}]}`, 0o600)
	out := ok(t, "", "metrics", "put", "site", "--file", report, "--job", "site-deploy", "--data", data)
	eq(t, "put says", out, "site: 3 items of seconds, coverage remembered as from site-deploy\n")
	st := openDB(t, data)
	rs, err := st.RememberedValues(context.Background(), "site", "")
	must(t, err)
	if len(rs) != 3 || rs[0].Metric != "coverage" || rs[0].Value != 87.19 || rs[0].Job != "site-deploy" || rs[0].Number != 0 || rs[0].BuildID != 0 {
		t.Fatalf("%+v", rs)
	}
	ok(t, "", "metrics", "put", "site", "--file", report, "--data", data)
	rs, err = st.RememberedValues(context.Background(), "site", "seconds")
	must(t, err)
	eq(t, "without --job", rs[0].Job, "cli")
	eq(t, "the audit", strings.Join(audit(t, st), "\n"),
		"cli metrics.put site: 3 items of seconds, coverage as from site-deploy\ncli metrics.put site: 3 items of seconds, coverage as from cli")

	bad := filepath.Join(t.TempDir(), "bad.json")
	write(t, bad, `{"metrics": [{"name": "kipp.stage_seconds", "values": {"a": 1}}, {"name": "x", "values": {"a": 1}}]}`, 0o600)
	code, _, errOut := kipp("", "metrics", "put", "site", "--file", bad, "--data", data)
	eq(t, "a report with an entry that breaks a rule", code, 1)
	contains(t, errOut, "bad.json: metric 1", "nothing was remembered")
	for _, args := range [][]string{
		{"put", "Site", "--file", report},
		{"put", "site", "--file", report, "--job", "Not A Job"},
		{"put", "site"},
		{"put", "site", "--file", filepath.Join(t.TempDir(), "none.json")},
	} {
		if code, _, _ := kipp("", append(append([]string{"metrics"}, args...), "--data", data)...); code == 0 {
			t.Fatalf("%v was taken", args)
		}
	}
	eq(t, "refused puts are not audited", len(audit(t, st)), 2)
}

func TestForgetOneItemLeavesTheRest(t *testing.T) {
	data := newData(t)
	report := filepath.Join(t.TempDir(), "report.json")
	write(t, report, `{"metrics": [{"name": "seconds", "values": {"a": 1, "b": 2}}, {"name": "coverage", "values": {"total": 1}}]}`, 0o600)
	ok(t, "", "metrics", "put", "site", "--file", report, "--data", data)
	eq(t, "forget an item", ok(t, "", "metrics", "forget", "site", "--metric", "seconds", "--item", "a", "--data", data), "site: 1 item forgotten\n")
	st := openDB(t, data)
	rs, err := st.RememberedValues(context.Background(), "site", "")
	must(t, err)
	if len(rs) != 2 || rs[0].Metric != "coverage" || rs[1].Item != "b" {
		t.Fatalf("%+v", rs)
	}
	eq(t, "forget nothing", ok(t, "", "metrics", "forget", "site", "--metric", "seconds", "--item", "a", "--data", data), "site: 0 items forgotten\n")
	if code, _, errOut := kipp("", "metrics", "forget", "site", "--item", "b", "--data", data); code != 2 || !strings.Contains(errOut, "--item needs --metric") {
		t.Fatal(code, errOut)
	}
	eq(t, "forget the store", ok(t, "", "metrics", "forget", "site", "--data", data), "site: 2 items forgotten\n")
	eq(t, "the audit", strings.Join(audit(t, st), "\n"), strings.Join([]string{
		"cli metrics.put site: 3 items of seconds, coverage as from cli",
		"cli metrics.forget site: metric seconds, item a: 1 item",
		"cli metrics.forget site: everything: 2 items",
	}, "\n"))
}

func TestLsMarksAStoreNoJobNames(t *testing.T) {
	data := newData(t)
	report := filepath.Join(t.TempDir(), "report.json")
	write(t, report, `{"metrics": [{"name": "seconds", "values": {"a": 1, "b": 2}}, {"name": "coverage", "values": {"total": 1}}]}`, 0o600)
	for _, s := range []string{"site", "site-smoke", "gone"} {
		ok(t, "", "metrics", "put", s, "--file", report, "--data", data)
	}
	jobs := t.TempDir()
	write(t, filepath.Join(jobs, "site-deploy.yml"), "name: site-deploy\nmetrics: {store: site}\nstages:\n  s: {run: 'true'}\n", 0o600)
	write(t, filepath.Join(jobs, "site-smoke.yml"), "name: site-smoke\nstages:\n  s: {run: 'true'}\n", 0o600)
	eq(t, "ls", ok(t, "", "metrics", "ls", "--jobs", jobs, "--data", data), strings.Join([]string{
		"gone\tcoverage\t1 item\torphaned: no job names it",
		"gone\tseconds\t2 items\torphaned: no job names it",
		"site\tcoverage\t1 item",
		"site\tseconds\t2 items",
		"site-smoke\tcoverage\t1 item",
		"site-smoke\tseconds\t2 items",
	}, "\n")+"\n")
	eq(t, "ls one store, no jobs", ok(t, "", "metrics", "ls", "--store", "site", "--data", data), "site\tcoverage\t1 item\nsite\tseconds\t2 items\n")
	if code, _, _ := kipp("", "metrics", "ls", "--jobs", filepath.Join(jobs, "none"), "--data", data); code == 0 {
		t.Fatal("a jobs directory that is not there was taken")
	}
}

// kippMetricsFile is a KIPP_METRICS of a stage with a guard on coverage
// total, which the store remembers as 87.19 from site-deploy #123.
const kippMetricsFile = `{"store": "site", "metrics": [{"name": "coverage", "unit": "%", "values": {"total": 87.19},
	"from": {"total": {"job": "site-deploy", "number": 123, "at": "2026-10-29T03:00:00Z"}}}],
	"guards": [{"metric": "coverage", "item": "total", "no_lower": true, "tolerance": 0.01, "on_fall": "failed", "required": true}]}`

func TestMetricsCheckJudgesInsideAStageWithNoDataDirectory(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "metrics.json"), kippMetricsFile, 0o600)
	write(t, filepath.Join(dir, "metrics-coverage.json"), `{"metrics": [{"name": "coverage", "values": {"total": 86.90}}]}`, 0o600)
	t.Setenv("KIPP_METRICS", filepath.Join(dir, "metrics.json"))
	code, out, _ := kipp("", "metrics", "check", filepath.Join(dir, "metrics-*.json"))
	eq(t, "exit", code, 1)
	eq(t, "says", out, "kipp: metrics guard: coverage total 87.19 -> 86.90 (-0.29), remembered from site-deploy #123, tolerance 0.01: failed\n")
	t.Setenv("KIPP_METRICS", "")
	code, out, _ = kipp("", "metrics", "check", filepath.Join(dir, "metrics-*.json"))
	if code != 2 || !strings.Contains(out, "KIPP_METRICS is not set") {
		t.Fatalf("%d %q", code, out)
	}
}
