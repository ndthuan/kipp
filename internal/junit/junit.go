// Package junit reads the JUnit XML a stage leaves behind.
package junit

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxReportBytes  = 16 << 20
	maxMessageBytes = 4 << 10
)

// Case is one test's outcome.
type Case struct {
	Suite, Name, Status string
	DurationMs          int
	Message             string
}

type suites struct {
	Suites []suite `xml:"testsuite"`
}

type suite struct {
	Name   string     `xml:"name,attr"`
	Cases  []testcase `xml:"testcase"`
	Suites []suite    `xml:"testsuite"`
}

type testcase struct {
	Name      string   `xml:"name,attr"`
	ClassName string   `xml:"classname,attr"`
	Time      string   `xml:"time,attr"`
	Failure   *outcome `xml:"failure"`
	Error     *outcome `xml:"error"`
	Skipped   *outcome `xml:"skipped"`
}

type outcome struct {
	Message string `xml:"message,attr"`
	Body    string `xml:",chardata"`
}

// Parse reads a <testsuites> or a bare <testsuite> document of at most 16 MiB.
func Parse(r io.Reader) ([]Case, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxReportBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxReportBytes {
		return nil, fmt.Errorf("report larger than %d bytes", maxReportBytes)
	}
	top, err := topSuites(xml.NewDecoder(bytes.NewReader(data)))
	if err != nil {
		return nil, err
	}
	var out []Case
	for _, s := range top {
		out = appendSuite(out, s)
	}
	return out, nil
}

// topSuites is the document's suites: those of a <testsuites> root, or
// the one a <testsuite> root is.
func topSuites(dec *xml.Decoder) ([]suite, error) {
	root, err := rootElement(dec)
	if err != nil {
		return nil, err
	}
	switch root.Name.Local {
	case "testsuites":
		var doc suites
		err := dec.DecodeElement(&doc, &root)
		return doc.Suites, err
	case "testsuite":
		var one suite
		err := dec.DecodeElement(&one, &root)
		return []suite{one}, err
	}
	return nil, fmt.Errorf("unexpected root element <%s>", root.Name.Local)
}

func rootElement(dec *xml.Decoder) (xml.StartElement, error) {
	for {
		tok, err := dec.Token()
		if err != nil {
			return xml.StartElement{}, err
		}
		if se, ok := tok.(xml.StartElement); ok {
			return se, nil
		}
	}
}

func appendSuite(out []Case, s suite) []Case {
	for _, tc := range s.Cases {
		out = append(out, toCase(s, tc))
	}
	for _, child := range s.Suites {
		out = appendSuite(out, child)
	}
	return out
}

func toCase(s suite, tc testcase) Case {
	c := Case{Suite: s.Name, Name: tc.Name, Status: "passed", DurationMs: millis(tc.Time)}
	if c.Suite == "" {
		c.Suite = tc.ClassName
	}
	switch {
	case tc.Failure != nil:
		c.Status, c.Message = "failed", tc.Failure.text()
	case tc.Error != nil:
		c.Status, c.Message = "failed", tc.Error.text()
	case tc.Skipped != nil:
		c.Status, c.Message = "skipped", tc.Skipped.text()
	}
	return c
}

// millis turns a time attribute into milliseconds; anything unparseable,
// non-finite or negative is 0, and the top is clamped so it cannot overflow.
func millis(attr string) int {
	t, err := strconv.ParseFloat(strings.TrimSpace(attr), 64)
	if err != nil || math.IsNaN(t) || math.IsInf(t, 0) || t < 0 {
		return 0
	}
	return int(math.Round(min(t*1000, math.MaxInt32)))
}

func (o *outcome) text() string {
	msg := o.Message
	if msg == "" {
		msg = o.Body
	}
	msg = strings.TrimSpace(msg)
	if len(msg) <= maxMessageBytes {
		return msg
	}
	cut := maxMessageBytes
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut]
}

// ParseGlob reads every regular file matching pattern under dir. The pattern
// and any symlink are confined to dir. A file that does not parse is an error
// in the second result; the rest still count.
func ParseGlob(dir, pattern string) ([]Case, []error) {
	if !fs.ValidPath(pattern) {
		return nil, []error{fmt.Errorf("pattern %q leaves the workspace", pattern)}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, []error{err}
	}
	defer func() { _ = root.Close() }()
	paths, err := fs.Glob(root.FS(), pattern)
	if err != nil {
		return nil, []error{err}
	}
	var cases []Case
	var errs []error
	for _, p := range paths {
		got, err := parseInRoot(root, p)
		if errors.Is(err, errNotRegular) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
			continue
		}
		cases = append(cases, got...)
	}
	return cases, errs
}

var errNotRegular = errors.New("not a regular file")

func parseInRoot(root *os.Root, name string) ([]Case, error) {
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
	return Parse(f)
}
