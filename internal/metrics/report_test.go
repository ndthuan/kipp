package metrics

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func byName(ms []Metric) map[string]Metric {
	out := map[string]Metric{}
	for _, m := range ms {
		out[m.Name] = m
	}
	return out
}

func hasError(errs []error, want ...string) bool {
	for _, err := range errs {
		ok := true
		for _, w := range want {
			ok = ok && strings.Contains(err.Error(), w)
		}
		if ok {
			return true
		}
	}
	return false
}

func TestAReportOfTwoFilesKeepsTheLaterItem(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "out/metrics-a.json", `{"metrics": [{"name": "seconds", "unit": "s", "values": {"one": 1, "two": 2}}]}`)
	write(t, dir, "out/metrics-b.json", `{"metrics": [{"name": "seconds", "complete": true, "values": {"two": 20, "three": 3}},
		{"name": "coverage", "unit": "%", "values": {"total": 87.19}}]}`)
	ms, errs := ParseGlob(dir, "out/metrics-*.json")
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	got := byName(ms)
	s := got["seconds"]
	if len(ms) != 2 || ms[0].Name != "seconds" || s.Unit != "s" || !s.Complete || len(s.Values) != 3 || s.Values["one"] != 1 || s.Values["two"] != 20 || s.Values["three"] != 3 {
		t.Fatalf("%+v", ms)
	}
	if c := got["coverage"]; c.Unit != "%" || c.Complete || c.Values["total"] != 87.19 {
		t.Fatalf("%+v", c)
	}
}

func TestAMetricNamedKippIsRefusedAndTheRestKept(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "m.json", `{"metrics": [{"name": "kipp.stage_seconds", "values": {"s": 1}},
		{"name": "Seconds", "values": {"s": 1}}, {"name": "seconds", "unit": "far too long a unit", "values": {"s": 1}},
		{"name": "coverage", "values": {"total": 1}}]}`)
	ms, errs := ParseGlob(dir, "m.json")
	if len(ms) != 1 || ms[0].Name != "coverage" {
		t.Fatalf("%+v", ms)
	}
	if !hasError(errs, "m.json", `"kipp.stage_seconds"`, "kipp.") || !hasError(errs, `"Seconds"`) || !hasError(errs, "unit") {
		t.Fatal(errs)
	}
}

func TestAnItemOver512BytesIsRefused(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("x", 513)
	write(t, dir, "m.json", `{"metrics": [{"name": "seconds", "values": {"`+long+`": 1, "`+strings.Repeat("y", 512)+`": 2, "": 3, "a\u0001b": 4, "ok": 5}}]}`)
	ms, errs := ParseGlob(dir, "m.json")
	if len(ms) != 1 || len(ms[0].Values) != 2 || ms[0].Values["ok"] != 5 {
		t.Fatalf("%+v", ms)
	}
	if len(errs) != 3 || !hasError(errs, "512 bytes") || !hasError(errs, "control character") {
		t.Fatal(errs)
	}
}

func TestANonFiniteValueIsRefused(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "m.json", `{"metrics": [{"name": "seconds", "values": {"big": 1e999, "text": "12", "none": null, "ok": -1.5}}]}`)
	ms, errs := ParseGlob(dir, "m.json")
	if len(ms) != 1 || len(ms[0].Values) != 1 || ms[0].Values["ok"] != -1.5 {
		t.Fatalf("%+v", ms)
	}
	if len(errs) != 3 || !hasError(errs, `"big"`, "finite number") || !hasError(errs, `"text"`) {
		t.Fatal(errs)
	}
}

func TestAFileOver16MiBIsRefused(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.json", `{"metrics": [{"name": "seconds", "values": {"a": 1}}], "pad": "`+strings.Repeat(" ", MaxReportBytes)+`"}`)
	write(t, dir, "b.json", `{"metrics": [{"name": "seconds", "values": {"b": 1}}]}`)
	write(t, dir, "c.json", `{"metrics": [`)
	ms, errs := ParseGlob(dir, "*.json")
	if len(ms) != 1 || len(ms[0].Values) != 1 || ms[0].Values["b"] != 1 {
		t.Fatalf("%+v", ms)
	}
	if !hasError(errs, "a.json", "16 MiB") || !hasError(errs, "c.json") {
		t.Fatal(errs)
	}
}

func TestAReportOverTheItemLimitIsCutAndSaysSo(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString(`{"metrics": [{"name": "seconds", "values": {`)
	for i := range MaxItems + 1 {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"%08d": 1`, i)
	}
	b.WriteString(`}}]}`)
	write(t, dir, "m.json", b.String())
	ms, errs := ParseGlob(dir, "m.json")
	if len(ms) != 1 || len(ms[0].Values) != MaxItems || !hasError(errs, "100000 items") {
		t.Fatalf("%d metrics, %v", len(ms), errs)
	}
}

func TestAGlobThatMatchesNothingSaysSo(t *testing.T) {
	ms, errs := ParseGlob(t.TempDir(), "metrics-*.json")
	if len(ms) != 0 || len(errs) != 1 || !errors.Is(errs[0], ErrNoReport) || !strings.Contains(errs[0].Error(), "metrics-*.json: matched nothing") {
		t.Fatal(ms, errs)
	}
	if _, errs := ParseGlob(t.TempDir(), "../m.json"); len(errs) != 1 || !strings.Contains(errs[0].Error(), "leaves") {
		t.Fatal(errs)
	}
}

func TestTheBaselineHoldsTheStoreValuesAndWhereEachCameFrom(t *testing.T) {
	at := time.Date(2026, 10, 29, 3, 0, 0, 0, time.FixedZone("x", 7*3600))
	data, err := Baseline("site", []Remembered{
		{Metric: "coverage", Item: "total", Unit: "%", Value: 87.19, From: From{Job: "site-deploy", Number: 123, At: at}},
		{Metric: "seconds", Item: "a", Unit: "s", Value: 1, From: From{Job: "cli", At: at}},
		{Metric: "seconds", Item: "b", Unit: "s", Value: 2, From: From{Job: "site-smoke", Number: 4, At: at}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"store":"site","metrics":[` +
		`{"name":"coverage","unit":"%","values":{"total":87.19},"from":{"total":{"job":"site-deploy","number":123,"at":"2026-10-28T20:00:00Z"}}},` +
		`{"name":"seconds","unit":"s","values":{"a":1,"b":2},"from":{"a":{"job":"cli","number":0,"at":"2026-10-28T20:00:00Z"},"b":{"job":"site-smoke","number":4,"at":"2026-10-28T20:00:00Z"}}}],` +
		`"guards":[]}`
	if got := strings.TrimSpace(string(data)); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	var back File
	if err := json.Unmarshal(data, &back); err != nil || back.Metrics[0].From["total"].Number != 123 {
		t.Fatal(err, back)
	}
	empty, err := Baseline("site", nil, nil)
	if err != nil || strings.TrimSpace(string(empty)) != `{"store":"site","metrics":[],"guards":[]}` {
		t.Fatal(err, string(empty))
	}
}
