// Package comments holds the test that keeps the repository free of the
// comments no style requires, as AGENTS.md says under Style.
package comments

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var skipped = []string{"examples/", "internal/jobstore/seed/", "docs/", "testdata/", "internal/web/static/htmx.min.js"}

var goDirectives = []string{"//go:", "//line ", "// +build", "//export ", "// Code generated"}

func isGoDirective(text string) bool {
	for _, d := range goDirectives {
		if strings.HasPrefix(text, d) {
			return true
		}
	}
	return false
}

func exportedDocs(f *ast.File) map[*ast.CommentGroup]bool {
	docs := map[*ast.CommentGroup]bool{}
	if f.Doc != nil {
		docs[f.Doc] = true
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Doc != nil && d.Name.IsExported() {
				docs[d.Doc] = true
			}
		case *ast.GenDecl:
			genDeclDocs(d, docs)
		}
	}
	return docs
}

func genDeclDocs(d *ast.GenDecl, docs map[*ast.CommentGroup]bool) {
	exported := false
	for _, s := range d.Specs {
		doc, ok := specDoc(s)
		if !ok {
			continue
		}
		exported = true
		if doc != nil {
			docs[doc] = true
		}
	}
	if exported && d.Doc != nil {
		docs[d.Doc] = true
	}
}

func specDoc(s ast.Spec) (*ast.CommentGroup, bool) {
	switch s := s.(type) {
	case *ast.TypeSpec:
		return s.Doc, s.Name.IsExported()
	case *ast.ValueSpec:
		for _, n := range s.Names {
			if n.IsExported() {
				return s.Doc, true
			}
		}
	}
	return nil, false
}

func strayGo(path string, src []byte) []string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return []string{path + ": " + err.Error()}
	}
	docs := exportedDocs(f)
	var out []string
	for _, g := range f.Comments {
		if docs[g] {
			continue
		}
		for _, c := range g.List {
			if !isGoDirective(c.Text) {
				out = append(out, path+":"+strconv.Itoa(fset.Position(c.Pos()).Line)+": a comment no style requires")
			}
		}
	}
	return out
}

var (
	heredoc   = regexp.MustCompile(`(?:^|[^<])<<-?\s*['"]?([A-Za-z_]\w*)['"]?`)
	yamlTrail = regexp.MustCompile(`^\s*-?\s*[\w-]+:\s*\S.*?\s#\s`)
	pinned    = regexp.MustCompile(`uses:\s*\S+@[0-9a-f]{40}\s+#\s+v\d`)
	jsTrail   = regexp.MustCompile(`[;{,]\s+//\s`)
)

func hashStyle(path string) bool {
	base := filepath.Base(path)
	switch {
	case base == "Makefile", base == "Dockerfile", base == "CODEOWNERS", base == ".gitignore", base == ".dockerignore":
		return true
	case strings.HasSuffix(path, ".sh"), strings.HasSuffix(path, ".yml"), strings.HasSuffix(path, ".yaml"):
		return true
	}
	return false
}

func slashStyle(path string) bool {
	return strings.HasSuffix(path, ".js") || strings.HasSuffix(path, ".mjs") || strings.HasSuffix(path, ".cjs") || strings.HasSuffix(path, ".css")
}

type hashScan struct {
	path, tag  string
	yaml, keep bool
	keepIndent int
}

func strayHash(path string, text []byte) []string {
	h := &hashScan{path: path, yaml: strings.HasSuffix(path, ".yml") || strings.HasSuffix(path, ".yaml")}
	var out []string
	for i, line := range strings.Split(string(text), "\n") {
		if h.stray(i, line) {
			out = append(out, path+":"+strconv.Itoa(i+1)+": a comment no style requires")
		}
	}
	return out
}

func (h *hashScan) stray(i int, line string) bool {
	s := strings.TrimSpace(line)
	if h.tag != "" {
		if s == h.tag {
			h.tag = ""
		}
		return false
	}
	h.exclusions(line, s)
	if h.keep {
		return false
	}
	if strings.HasPrefix(s, "#") {
		return (i != 0 || !strings.HasPrefix(s, "#!")) && !strings.HasPrefix(s, "# shellcheck")
	}
	if h.yaml && !pinned.MatchString(line) && yamlTrail.MatchString(line) {
		return true
	}
	if m := heredoc.FindStringSubmatch(line); m != nil {
		h.tag = m[1]
	}
	return false
}

func (h *hashScan) exclusions(line, s string) {
	if h.path != ".golangci.yml" {
		return
	}
	indent := len(line) - len(strings.TrimLeft(line, " \t"))
	switch {
	case s == "exclusions:":
		h.keep, h.keepIndent = true, indent
	case h.keep && s != "" && !strings.HasPrefix(s, "#") && indent <= h.keepIndent:
		h.keep = false
	}
}

func straySlash(path string, text []byte) []string {
	var out []string
	for i, line := range strings.Split(string(text), "\n") {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "//") || strings.HasPrefix(s, "/*") || jsTrail.MatchString(line) {
			out = append(out, path+":"+strconv.Itoa(i+1)+": a comment no style requires")
		}
	}
	return out
}

func stray(path string, text []byte) []string {
	for _, s := range skipped {
		if strings.HasPrefix(path, s) {
			return nil
		}
	}
	switch {
	case strings.HasSuffix(path, ".go"):
		return strayGo(path, text)
	case hashStyle(path):
		return strayHash(path, text)
	case slashStyle(path):
		return straySlash(path, text)
	}
	return nil
}

func TestNoTrackedFileHasACommentNoStyleRequires(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "ls-files", "-z")
	cmd.Dir = root
	listed, err := cmd.Output()
	if err != nil {
		t.Skipf("not a git checkout, so there is no list of tracked files: %v", err)
	}
	repo, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repo.Close() }()
	for path := range strings.SplitSeq(strings.TrimSuffix(string(listed), "\x00"), "\x00") {
		text, err := repo.ReadFile(path)
		if err != nil {
			continue
		}
		for _, bad := range stray(path, text) {
			t.Error(bad)
		}
	}
}

func TestAGoCommentIsAllowedOnlyAsTheDocOfThePackageOrAnExportedName(t *testing.T) {
	src := `//go:build linux

// Package p is allowed.
package p

import "embed"

// T is allowed.
type T int

// t is not.
type t int

// Exported constants are allowed.
const (
	// A is allowed.
	A = 1
)

//go:embed x
var files embed.FS

// F is allowed.
func F() {
	// nor is this
	_ = 1 // nor this
}

// f is not.
func f() {}
`
	got := strayGo("p.go", []byte(src))
	want := []string{"p.go:11: a comment no style requires", "p.go:25: a comment no style requires", "p.go:26: a comment no style requires", "p.go:29: a comment no style requires"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestAHashCommentIsFoundAndWhatIsNoCommentIsNot(t *testing.T) {
	for _, c := range []struct {
		path, text string
		stray      int
	}{
		{"scripts/a.sh", "#!/bin/sh\n# shellcheck disable=SC2086\necho hi\n", 0},
		{"scripts/a.sh", "#!/bin/sh\n# why this\necho hi\n", 1},
		{"scripts/a.sh", "cat <<EOF\n# written into the file\nEOF\n", 0},
		{"scripts/a.sh", "cat <<'EOF'\nx\nEOF\n# after it\n", 1},
		{"Makefile", "lint:   ## go vet and golangci-lint\n\tgo vet ./...\n", 0},
		{"Makefile", "# the gate\nlint:\n", 1},
		{".github/workflows/x.yml", "      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1\n", 0},
		{".github/workflows/x.yml", "      actions: read # for the runs\n", 1},
		{".github/workflows/x.yml", "        run: echo \"#$N\"\n", 0},
		{".golangci.yml", "linters:\n  # why\n  exclusions:\n    rules:\n      # the reason\n      - path: x\nformatters:\n", 1},
	} {
		if got := len(strayHash(c.path, []byte(c.text))); got != c.stray {
			t.Errorf("%s %q: %d stray, want %d", c.path, c.text, got, c.stray)
		}
	}
}

func TestASlashCommentIsFoundAndAURLIsNot(t *testing.T) {
	for text, n := range map[string]int{
		"const a = 1; // why":                 1,
		"// why\nconst a = 1;":                1,
		"/* why */\n.a { color: red; }":       1,
		`fetch("https://example.com/x");`:     0,
		"const re = /a\\/\\/b/;":              0,
		".a { background: url(//x.example) }": 0,
	} {
		if got := len(straySlash("a.js", []byte(text))); got != n {
			t.Errorf("%q: %d stray, want %d", text, got, n)
		}
	}
}

func TestTheExamplesAndTheVendoredScriptAreLeftAlone(t *testing.T) {
	for _, path := range []string{"examples/jobs/acme.yml", "internal/jobstore/seed/hello.yml", "internal/web/static/htmx.min.js"} {
		if got := stray(path, []byte("# a comment for users\n// and this\n")); got != nil {
			t.Errorf("%s: %v", path, got)
		}
	}
}
