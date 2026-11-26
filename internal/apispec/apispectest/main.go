package apispectest

import (
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

// Main is a package's TestMain: it runs the tests, and when they pass
// reports, as a failure, the operations of the spec whose x-kipp-package is
// pkg that no test got a good answer from. It says nothing when the run was
// narrowed with -run, which cannot reach them all.
func Main(m *testing.M, pkg string) {
	code := m.Run()
	if code == 0 && !narrowed() {
		if missing := Uncovered(pkg); len(missing) > 0 {
			fmt.Fprintf(os.Stderr, "FAIL: %d operation(s) of the %s package have no test with a good response checked against the spec:\n  %s\n",
				len(missing), pkg, strings.Join(missing, "\n  "))
			code = 1
		}
	}
	os.Exit(code)
}

func narrowed() bool {
	f := flag.Lookup("test.run")
	return f != nil && f.Value.String() != ""
}

// Uncovered are the operations of pkg without a good response checked.
func Uncovered(pkg string) []string {
	s, err := Load()
	if err != nil {
		return []string{"the spec does not load: " + err.Error()}
	}
	return s.Uncovered(pkg)
}

// Uncovered are the operations of pkg without a good response checked.
func (s *Spec) Uncovered(pkg string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, op := range s.ops {
		if op.Package == pkg && !s.covered[op.Key()] {
			out = append(out, op.Key())
		}
	}
	slices.Sort(out)
	return out
}
