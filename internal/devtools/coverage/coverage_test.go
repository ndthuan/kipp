package main

import (
	"bytes"
	"encoding/json"
	"maps"
	"strings"
	"testing"
)

const profile = `mode: atomic
github.com/ndthuan/kipp/internal/a/a.go:1.1,2.2 3 1
github.com/ndthuan/kipp/internal/a/a.go:3.1,4.2 1 0
github.com/ndthuan/kipp/internal/b/b.go:1.1,2.2 2 0
github.com/ndthuan/kipp/internal/b/b.go:1.1,2.2 2 5
github.com/ndthuan/kipp/cmd/kipp/main.go:1.1,2.2 4 0
`

func TestAReportIsKippMetricsOfEachPackageAndTheTotal(t *testing.T) {
	r, err := parseProfile(strings.NewReader(profile), "github.com/ndthuan/kipp")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := writeMetrics(&out, r); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Metrics []struct {
			Name     string             `json:"name"`
			Unit     string             `json:"unit"`
			Complete bool               `json:"complete"`
			Values   map[string]float64 `json:"values"`
		} `json:"metrics"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("%v in %s", err, out.String())
	}
	if len(got.Metrics) != 1 || got.Metrics[0].Name != "coverage" || got.Metrics[0].Unit != "%" || !got.Metrics[0].Complete {
		t.Fatalf("not one complete coverage metric in %%: %s", out.String())
	}
	// A block a second profile line names again counts once, covered if
	// either line ran it.
	want := map[string]float64{"internal/a": 75, "internal/b": 100, "cmd/kipp": 0, "total": 50}
	for item, v := range want {
		if got.Metrics[0].Values[item] != v {
			t.Errorf("%s = %v, want %v (all: %v)", item, got.Metrics[0].Values[item], v, got.Metrics[0].Values)
		}
	}
	if len(got.Metrics[0].Values) != len(want) {
		t.Errorf("items %v, want %v", got.Metrics[0].Values, want)
	}
}

func TestAProfileThatIsNotOneIsRefused(t *testing.T) {
	for name, in := range map[string]string{
		"empty":     "",
		"no mode":   "github.com/ndthuan/kipp/internal/a/a.go:1.1,2.2 3 1\n",
		"bad block": "mode: set\nnot a block\n",
		"bad count": "mode: set\ngithub.com/ndthuan/kipp/internal/a/a.go:1.1,2.2 x 1\n",
	} {
		if _, err := parseProfile(strings.NewReader(in), "github.com/ndthuan/kipp"); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
}

func values(total float64, pkgs map[string]float64) map[string]float64 {
	out := map[string]float64{"total": total}
	maps.Copy(out, pkgs)
	return out
}

func TestTheGuardFailsAFallOfTheTotalBeyondTheTolerance(t *testing.T) {
	for name, tc := range map[string]struct {
		base, head map[string]float64
		fell       bool
		says       string
	}{
		"held":              {values(80, nil), values(80, nil), false, "coverage total 80.00 -> 80.00 (+0.00), tolerance 0.10: passed"},
		"rose":              {values(80, nil), values(81.5, nil), false, "(+1.50), tolerance 0.10: passed"},
		"within tolerance":  {values(80, nil), values(79.95, nil), false, "(-0.05), tolerance 0.10: passed"},
		"fell":              {values(80, nil), values(79.5, nil), true, "coverage total 80.00 -> 79.50 (-0.50), tolerance 0.10: failed"},
		"no baseline":       {nil, values(70, nil), false, "coverage total 70.00: no baseline, passed"},
		"a package fell":    {values(80, map[string]float64{"internal/a": 90}), values(80, map[string]float64{"internal/a": 60}), false, "coverage total 80.00 -> 80.00"},
		"the total missing": {values(80, nil), map[string]float64{"internal/a": 1}, true, "coverage total: missing from the report, failed"},
	} {
		line, fell := guard(tc.base, tc.head, 0.1)
		if fell != tc.fell || !strings.Contains(line, tc.says) {
			t.Errorf("%s: fell %v, said %q; want %v and %q", name, fell, line, tc.fell, tc.says)
		}
	}
}

func TestTheSummaryListsWhatChangedLargestFallFirst(t *testing.T) {
	base := values(80, map[string]float64{"internal/a": 90, "internal/b": 50, "internal/c": 70, "internal/gone": 10})
	head := values(79, map[string]float64{"internal/a": 60, "internal/b": 55, "internal/c": 70, "internal/new": 40})
	got := summary(base, head)
	for _, want := range []string{"| total | 80.00 | 79.00 | -1.00 |", "| internal/a | 90.00 | 60.00 | -30.00 |", "| internal/b | 50.00 | 55.00 | +5.00 |", "| internal/new |  | 40.00 | new |", "| internal/gone | 10.00 |  | gone |"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "internal/c ") || strings.Index(got, "internal/a") > strings.Index(got, "internal/b") {
		t.Errorf("an unchanged package listed, or not the largest fall first:\n%s", got)
	}
}
