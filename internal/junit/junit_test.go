package junit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const report = `<?xml version="1.0"?>
<testsuites>
  <testsuite name="parser" tests="3" failures="1" skipped="1">
    <testcase classname="pkg.Parser" name="parses" time="1.25"/>
    <testcase classname="pkg.Parser" name="rejects" time="0.5"><failure message="no error">stack</failure></testcase>
    <testcase classname="pkg.Parser" name="skips"><skipped message="no network"/></testcase>
  </testsuite>
  <testsuite name="build"><testcase name="links" time="30"><error message="exit status 1"/></testcase></testsuite>
</testsuites>`

func TestParsesSuitesCasesAndOutcomes(t *testing.T) {
	cases, err := Parse(strings.NewReader(report))
	if err != nil || len(cases) != 4 {
		t.Fatal(err, cases)
	}
	if c := cases[0]; c.Suite != "parser" || c.Name != "parses" || c.Status != "passed" || c.DurationMs != 1250 {
		t.Fatalf("%+v", c)
	}
	if c := cases[1]; c.Status != "failed" || c.Message != "no error" {
		t.Fatalf("%+v", c)
	}
	if c := cases[2]; c.Status != "skipped" || c.Message != "no network" {
		t.Fatalf("%+v", c)
	}
	if c := cases[3]; c.Suite != "build" || c.Status != "failed" || c.Message != "exit status 1" {
		t.Fatalf("%+v", c)
	}
}

func TestASingleTestsuiteRootAndClassnameFallback(t *testing.T) {
	cases, err := Parse(strings.NewReader(`<testsuite><testcase classname="pkg.Thing" name="works"/></testsuite>`))
	if err != nil || len(cases) != 1 || cases[0].Suite != "pkg.Thing" {
		t.Fatal(err, cases)
	}
}

func TestGlobReadsEveryFileAndReportsTheBadOne(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "reports"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "reports", "a.xml"), []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "reports", "b.xml"), []byte("<not xml"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases, errs := ParseGlob(dir, "reports/*.xml")
	if len(cases) != 4 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "b.xml") {
		t.Fatal(len(cases), errs)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGlobCannotLeaveTheWorkspace(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "ws")
	writeFile(t, filepath.Join(base, "outside.xml"), report)
	writeFile(t, filepath.Join(dir, "reports", "in.xml"), `<testsuite name="s"><testcase name="x"/></testsuite>`)
	if err := os.Symlink(filepath.Join(base, "outside.xml"), filepath.Join(dir, "reports", "link.xml")); err != nil {
		t.Fatal(err)
	}
	cases, errs := ParseGlob(dir, "../outside.xml")
	if len(cases) != 0 || len(errs) != 1 {
		t.Fatal(cases, errs)
	}
	cases, errs = ParseGlob(dir, "reports/*.xml")
	if len(cases) != 1 || cases[0].Suite != "s" || len(errs) != 1 {
		t.Fatal(cases, errs)
	}
}

func TestGlobSkipsDirectories(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "d.xml"), 0o750); err != nil {
		t.Fatal(err)
	}
	cases, errs := ParseGlob(dir, "*.xml")
	if len(cases) != 0 || len(errs) != 0 {
		t.Fatal(cases, errs)
	}
}

func TestAReportOverTheCapIsAnError(t *testing.T) {
	big := `<testsuite>` + strings.Repeat(" ", maxReportBytes) + `</testsuite>`
	if _, err := Parse(strings.NewReader(big)); err == nil {
		t.Fatal("expected an error")
	}
}

func TestMessagesAreTrimmedAndTruncated(t *testing.T) {
	long := strings.Repeat("e", maxMessageBytes*2)
	doc := `<testsuite name="s"><testcase name="a"><failure>  ` + long + `  </failure></testcase>` +
		`<testcase name="b"><failure>  short
</failure></testcase></testsuite>`
	cases, err := Parse(strings.NewReader(doc))
	if err != nil || len(cases) != 2 {
		t.Fatal(err, cases)
	}
	if len(cases[0].Message) != maxMessageBytes || cases[1].Message != "short" {
		t.Fatalf("%d %q", len(cases[0].Message), cases[1].Message)
	}
}

func TestNestedSuitesAreWalked(t *testing.T) {
	root := `<testsuite name="outer"><testcase name="own"/><testsuite name="inner"><testcase name="deep"/></testsuite></testsuite>`
	wrapped := `<testsuites><testsuite name="outer"><testsuite name="inner"><testcase name="deep"/></testsuite></testsuite></testsuites>`
	cases, err := Parse(strings.NewReader(root))
	if err != nil || len(cases) != 2 || cases[0].Name != "own" || cases[1].Suite != "inner" {
		t.Fatal(err, cases)
	}
	cases, err = Parse(strings.NewReader(wrapped))
	if err != nil || len(cases) != 1 || cases[0].Name != "deep" {
		t.Fatal(err, cases)
	}
}

func TestUnknownRootIsAnError(t *testing.T) {
	_, err := Parse(strings.NewReader(`<html><body/></html>`))
	if err == nil || !strings.Contains(err.Error(), "html") {
		t.Fatal(err)
	}
}

func TestOddTimesBecomeZeroOrClamp(t *testing.T) {
	doc := `<testsuite name="s">` +
		`<testcase name="nan" time="NaN"/><testcase name="inf" time="Inf"/>` +
		`<testcase name="neg" time="-3"/><testcase name="junk" time="1,5s"/>` +
		`<testcase name="huge" time="1e300"/></testsuite>`
	cases, err := Parse(strings.NewReader(doc))
	if err != nil || len(cases) != 5 {
		t.Fatal(err, cases)
	}
	for _, c := range cases[:4] {
		if c.DurationMs != 0 {
			t.Fatalf("%+v", c)
		}
	}
	if cases[4].DurationMs <= 0 {
		t.Fatalf("%+v", cases[4])
	}
}

func TestSkippedWithOnlyABody(t *testing.T) {
	cases, err := Parse(strings.NewReader(`<testsuite name="s"><testcase name="a"><skipped>why not</skipped></testcase></testsuite>`))
	if err != nil || len(cases) != 1 || cases[0].Status != "skipped" || cases[0].Message != "why not" {
		t.Fatal(err, cases)
	}
}
