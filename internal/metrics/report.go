// Package metrics reads the reports stages leave of what they measured,
// and writes what a store remembers for a stage to read.
package metrics

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ndthuan/kipp/internal/config"
)

// Metric is one metric of a report: a named number per item. It is
// store.Metric field for field, so the server converts one to the other,
// and kept apart so that kipp-runner, which checks guards inside a
// container, carries no database.
type Metric struct {
	Name, Unit string
	// Complete says the report holds every item of the metric: the store
	// forgets the items it does not name. Otherwise the report merges.
	Complete bool
	Values   map[string]float64
}

// The limits of a report: a stage's numbers, not a place for documents.
const (
	MaxReportBytes = 16 << 20 // one file
	MaxItems       = 100_000  // all the items of one stage's report
	MaxItemBytes   = 512
	MaxUnitBytes   = 16
)

// ErrNoReport is a glob that matched no file.
var ErrNoReport = errors.New("matched nothing")

// report is the file a stage writes.
type report struct {
	Metrics []json.RawMessage `json:"metrics"`
}

type entry struct {
	Name     string                     `json:"name"`
	Unit     string                     `json:"unit"`
	Complete bool                       `json:"complete"`
	Values   map[string]json.RawMessage `json:"values"`
}

// ParseGlob reads every regular file the glob matches under dir, in name
// order, into one report: a later file's item wins over an earlier one's,
// a metric is complete when any file says so. What breaks a rule is an
// error in the second result, said as "<file>: <why>", and the rest is
// kept; a glob that matches nothing is ErrNoReport.
func ParseGlob(dir, glob string) ([]Metric, []error) {
	if !fs.ValidPath(glob) {
		return nil, []error{fmt.Errorf("%s: the pattern leaves the stage's directory", glob)}
	}
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, []error{fmt.Errorf("%s: %w", glob, ErrNoReport)}
	}
	if err != nil {
		return nil, []error{err}
	}
	defer func() { _ = root.Close() }()
	paths, err := fs.Glob(root.FS(), glob)
	if err != nil {
		return nil, []error{err}
	}
	m := &merged{byName: map[string]*Metric{}}
	read := 0
	for _, p := range paths {
		data, err := readRegular(root, p)
		if errors.Is(err, errNotRegular) {
			continue
		}
		read++
		if err != nil {
			m.errs = append(m.errs, fmt.Errorf("%s: %w", p, err))
			continue
		}
		m.add(p, data)
	}
	if read == 0 {
		return nil, []error{fmt.Errorf("%s: %w", glob, ErrNoReport)}
	}
	return m.metrics(), m.errs
}

var errNotRegular = errors.New("not a regular file")

func readRegular(root *os.Root, name string) ([]byte, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errNotRegular
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxReportBytes+1))
	if err == nil && len(data) > MaxReportBytes {
		err = fmt.Errorf("larger than %d bytes (16 MiB)", MaxReportBytes)
	}
	return data, err
}

// merged is the report of a stage as its files are read.
type merged struct {
	order  []string
	byName map[string]*Metric
	items  int
	full   bool // the item limit was reached and said
	errs   []error
}

func (m *merged) fail(file, format string, args ...any) {
	m.errs = append(m.errs, fmt.Errorf("%s: "+format, append([]any{file}, args...)...))
}

func (m *merged) add(file string, data []byte) {
	var r report
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		m.fail(file, "not a report: %v", err)
		return
	}
	for i, raw := range r.Metrics {
		var e entry
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&e); err != nil {
			m.fail(file, "metric %d: %v", i+1, err)
			continue
		}
		if err := e.check(); err != nil {
			m.fail(file, "metric %d: %v", i+1, err)
			continue
		}
		m.addEntry(file, e)
	}
}

func (e entry) check() error {
	switch {
	case !config.ValidName(e.Name):
		return fmt.Errorf("name %q: lower-case letters, digits, dot, dash and underscore only", e.Name)
	case strings.HasPrefix(e.Name, config.MetricPrefix):
		return fmt.Errorf("name %q: names starting %s are the ones kipp records itself", e.Name, config.MetricPrefix)
	case len(e.Unit) > MaxUnitBytes || !utf8.ValidString(e.Unit) || strings.ContainsFunc(e.Unit, unicode.IsControl):
		return fmt.Errorf("%s: unit %q: text of at most %d bytes", e.Name, e.Unit, MaxUnitBytes)
	}
	return nil
}

func (m *merged) addEntry(file string, e entry) {
	got := m.byName[e.Name]
	if got == nil {
		got = &Metric{Name: e.Name, Values: map[string]float64{}}
		m.byName[e.Name] = got
		m.order = append(m.order, e.Name)
	}
	if e.Unit != "" {
		got.Unit = e.Unit
	}
	got.Complete = got.Complete || e.Complete
	for _, item := range slices.Sorted(maps.Keys(e.Values)) {
		v, err := value(item, e.Values[item])
		if err != nil {
			m.fail(file, "%s: %v", e.Name, err)
			continue
		}
		if _, had := got.Values[item]; !had {
			if m.items == MaxItems {
				if !m.full {
					m.full = true
					m.fail(file, "more than %d items in the stage's report; the rest are left out", MaxItems)
				}
				continue
			}
			m.items++
		}
		got.Values[item] = v
	}
}

// value is an item's number, or why it is not one.
func value(item string, raw json.RawMessage) (float64, error) {
	switch {
	case item == "" || len(item) > MaxItemBytes:
		return 0, fmt.Errorf("item %.40q...: an item is 1 to %d bytes", item, MaxItemBytes)
	case !ValidItem(item):
		return 0, fmt.Errorf("item %q: UTF-8 with no control character", item)
	}
	s := string(bytes.TrimSpace(raw))
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsInf(v, 0) || math.IsNaN(v) || !isJSONNumber(s) {
		return 0, fmt.Errorf("item %q: %s is not a finite number", item, s)
	}
	return v, nil
}

// ValidItem reports whether s may name an item: 1 to 512 bytes of UTF-8
// with no control character.
func ValidItem(s string) bool {
	return s != "" && len(s) <= MaxItemBytes && utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

// isJSONNumber reports whether s is written as a JSON number: ParseFloat
// also takes "Inf", "0x1p3" and underscores.
func isJSONNumber(s string) bool {
	return json.Valid([]byte(s)) && s != "" && (s[0] == '-' || (s[0] >= '0' && s[0] <= '9'))
}

func (m *merged) metrics() []Metric {
	out := make([]Metric, 0, len(m.order))
	for _, name := range m.order {
		out = append(out, *m.byName[name])
	}
	return out
}

// File is what KIPP_METRICS names: what the store remembers, with the
// build each item's value came from, and the stage's guards.
type File struct {
	Store   string       `json:"store"`
	Metrics []FileMetric `json:"metrics"`
	Guards  []FileGuard  `json:"guards"`
}

// FileMetric is one metric the store remembers.
type FileMetric struct {
	Name   string             `json:"name"`
	Unit   string             `json:"unit"`
	Values map[string]float64 `json:"values"`
	// From is per item: a report that merges leaves the items of one
	// metric from different builds.
	From map[string]From `json:"from"`
}

// From is the build that set a remembered value; one put by hand is the
// job cli and number 0.
type From struct {
	Job    string    `json:"job"`
	Number int       `json:"number"`
	At     time.Time `json:"at"`
}

// FileGuard is a guard of the stage, as its job file says it with the
// defaults filled in.
type FileGuard struct {
	Metric    string  `json:"metric"`
	Item      string  `json:"item"`
	NoLower   bool    `json:"no_lower"`
	Tolerance float64 `json:"tolerance"`
	OnFall    string  `json:"on_fall"`
	Required  bool    `json:"required"`
}

// Remembered is one value a store remembers, and the build that set it.
type Remembered struct {
	Metric, Item, Unit string
	Value              float64
	From               From
}

// Baseline is the file KIPP_METRICS names, for a stage of a job whose
// store is name: the values remembered, in metric order, and the guards.
func Baseline(name string, remembered []Remembered, guards []config.Guard) ([]byte, error) {
	f := File{Store: name, Metrics: []FileMetric{}, Guards: FileGuards(guards)}
	for _, r := range remembered {
		if n := len(f.Metrics); n == 0 || f.Metrics[n-1].Name != r.Metric {
			f.Metrics = append(f.Metrics, FileMetric{Name: r.Metric, Values: map[string]float64{}, From: map[string]From{}})
		}
		m := &f.Metrics[len(f.Metrics)-1]
		m.Unit = r.Unit
		m.Values[r.Item] = r.Value
		from := r.From
		from.At = from.At.UTC()
		m.From[r.Item] = from
	}
	data, err := json.Marshal(f)
	return append(data, '\n'), err
}

// FileGuards are the stage's guards as KIPP_METRICS says them, with the
// defaults filled in.
func FileGuards(guards []config.Guard) []FileGuard {
	out := make([]FileGuard, 0, len(guards))
	for _, g := range guards {
		out = append(out, FileGuard{Metric: g.Metric, Item: g.Item, NoLower: g.NoLower, Tolerance: g.Tolerance, OnFall: g.OnFall, Required: g.IsRequired()})
	}
	return out
}
