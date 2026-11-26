package mcp

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/artifacts"
	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/store"
)

const (
	defaultMatches      = 50
	maxMatches          = 200
	maxPattern          = 500
	searchBuffer        = 64 << 10
	maxLine             = 1 << 20
	maxChanges          = 50
	defaultHistoryBuild = 20
	maxHistory          = 50
)

type readArtifactIn struct {
	jobRef
	Stage      string `json:"stage" jsonschema:"the stage that kept the file, as list_artifacts gives it"`
	Path       string `json:"path" jsonschema:"the file's path, as list_artifacts gives it"`
	Offset     int64  `json:"offset,omitempty" jsonschema:"where to start reading, in bytes; default 0"`
	LimitBytes int    `json:"limit_bytes,omitempty" jsonschema:"the most bytes to return, default 65536, at most 262144"`
}

type readArtifactOut struct {
	Text      string `json:"text"`
	Offset    int64  `json:"offset"`
	Next      int64  `json:"next_offset"`
	Size      int64  `json:"total_size_bytes"`
	Truncated bool   `json:"truncated"`
	URL       string `json:"url,omitempty" jsonschema:"where the file is downloaded, with an API token"`
}

func (s *Server) readArtifact(ctx context.Context, c *Caller, in readArtifactIn) (readArtifactOut, error) {
	var out readArtifactOut
	switch {
	case in.LimitBytes < 0 || in.LimitBytes > maxBytes:
		return out, badInput("limit_bytes is 1 to %d", maxBytes)
	case in.Offset < 0:
		return out, badInput("offset is 0 or more")
	}
	b, err := s.build(ctx, c, in.jobRef)
	if err != nil {
		return out, err
	}
	if !artifacts.ValidPath(in.Path) {
		return out, api.ErrNotFound
	}
	_, dir, rows, err := s.cfg.Service.StageArtifacts(ctx, c.User, b.ID, in.Stage)
	if err != nil {
		return out, err
	}
	if !slices.ContainsFunc(rows, func(r store.Artifact) bool { return r.Path == in.Path }) {
		return out, api.ErrNotFound
	}
	f, err := artifacts.Open(dir, in.Path)
	if err != nil {
		return out, api.ErrNotFound
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return out, err
	}
	if s.cfg.BaseURL != "" {
		out.URL = strings.TrimSuffix(s.cfg.BaseURL, "/") + api.ArtifactURL(b.ID, in.Stage, in.Path)
	}
	text, start := readAt(f, info.Size(), in.Offset, or(in.LimitBytes, defaultBytes))
	if bytes.IndexByte(text, 0) >= 0 {
		msg := fmt.Sprintf("refused: %s is not text", in.Path)
		if out.URL != "" {
			msg += "; download it from " + out.URL
		}
		return readArtifactOut{}, &toldError{msg: msg}
	}
	out.Text, out.Offset, out.Next, out.Size = strings.ToValidUTF8(string(text), "\uFFFD"), start, start+int64(len(text)), info.Size()
	out.Truncated = start > 0 || out.Next < out.Size
	return out, nil
}

func readAt(f io.ReaderAt, size, from int64, limit int) ([]byte, int64) {
	start := min(from, size)
	end := min(start+int64(limit), size)
	buf := make([]byte, end-start)
	n, _ := f.ReadAt(buf, start)
	return buf[:n], start
}

type searchLogIn struct {
	jobRef
	Stage      string `json:"stage" jsonschema:"the stage's name, as get_build lists it; clone, prepare and hooks are the server's own logs"`
	Pattern    string `json:"pattern" jsonschema:"an RE2 regular expression matched against each line; a plain word matches the lines that hold it"`
	IgnoreCase bool   `json:"ignore_case,omitempty" jsonschema:"match without regard to case"`
	MaxMatches int    `json:"max_matches,omitempty" jsonschema:"how many matching lines to return, default 50, at most 200; total_matches counts them all"`
}

type matchOut struct {
	Line   int    `json:"line" jsonschema:"the line's number in the log, from 1"`
	Offset int64  `json:"offset" jsonschema:"where the line begins; read_log with this offset reads on from it"`
	Text   string `json:"text" jsonschema:"the line, cut at 500 bytes"`
}

type searchLogOut struct {
	Matches []matchOut `json:"matches"`
	Total   int        `json:"total_matches"`
	Cut     bool       `json:"cut"`
	Size    int64      `json:"total_size_bytes"`
	More    bool       `json:"build_still_writing"`
}

func (s *Server) searchLog(ctx context.Context, c *Caller, in searchLogIn) (searchLogOut, error) {
	out := searchLogOut{Matches: []matchOut{}}
	switch {
	case in.Pattern == "":
		return out, badInput("pattern is required")
	case len(in.Pattern) > maxPattern:
		return out, badInput("pattern is at most %d bytes", maxPattern)
	case in.MaxMatches < 0 || in.MaxMatches > maxMatches:
		return out, badInput("max_matches is 1 to %d", maxMatches)
	}
	expr := in.Pattern
	if in.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return out, badInput("pattern: %v", err)
	}
	path, complete, err := s.stageLog(ctx, c, in.jobRef, in.Stage)
	if err != nil {
		return out, err
	}
	out.More = !complete
	f, err := dispatch.OpenLog(path, os.O_RDONLY)
	if err != nil {
		return out, nil
	}
	defer func() { _ = f.Close() }()
	if info, err := f.Stat(); err == nil {
		out.Size = info.Size()
	}
	found, total, err := searchLines(ctx, io.LimitReader(f, out.Size), re.Match, or(in.MaxMatches, defaultMatches))
	if err != nil {
		return out, err
	}
	out.Matches, out.Total, out.Cut = append(out.Matches, found...), total, total > len(found)
	return out, nil
}

func (s *Server) stageLog(ctx context.Context, c *Caller, ref jobRef, stage string) (string, bool, error) {
	b, err := s.build(ctx, c, ref)
	if err != nil {
		return "", false, err
	}
	if stage == "" {
		return "", false, badInput("stage is required: one of the stages get_build lists")
	}
	svc := s.cfg.Service
	if err := svc.HasStage(ctx, c.User, b.ID, stage); err != nil {
		return "", false, err
	}
	path, _, err := svc.LogPath(ctx, c.User, b.ID, stage)
	if err != nil {
		return "", false, err
	}
	v, err := svc.Build(ctx, c.User, b.ID)
	if err != nil {
		return "", false, err
	}
	return path, v.Complete, nil
}

func searchLines(ctx context.Context, r io.Reader, match func([]byte) bool, max int) ([]matchOut, int, error) {
	lr := &lineReader{br: bufio.NewReaderSize(r, searchBuffer)}
	var found []matchOut
	var start int64
	total := 0
	for number := 1; ; number++ {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		text, size, ok := lr.next()
		if !ok {
			return found, total, nil
		}
		if match(text) {
			total++
			if len(found) < max {
				found = append(found, matchOut{Line: number, Offset: start, Text: clip(strings.ToValidUTF8(string(text), "\uFFFD"), maxMessage)})
			}
		}
		start += int64(size)
	}
}

type lineReader struct {
	br   *bufio.Reader
	line []byte
}

func (l *lineReader) next() (text []byte, size int, ok bool) {
	l.line = l.line[:0]
	for {
		chunk, err := l.br.ReadSlice('\n')
		if room := maxLine - len(l.line); room > 0 {
			l.line = append(l.line, chunk[:min(len(chunk), room)]...)
		}
		size += len(chunk)
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case size == 0:
			return nil, 0, false
		}
		return bytes.TrimSuffix(bytes.TrimSuffix(l.line, []byte("\n")), []byte("\r")), size, true
	}
}

type compareIn struct {
	jobRef
	Base int `json:"base,omitempty" jsonschema:"the number of the build to compare with; default the job's build before number"`
}

type briefOut struct {
	Number int               `json:"number"`
	Status string            `json:"status"`
	Commit string            `json:"commit,omitempty"`
	Params map[string]string `json:"params,omitempty"`
	URL    string            `json:"url,omitempty"`
}

type paramChange struct {
	Name  string `json:"name"`
	Base  string `json:"base"`
	Build string `json:"build"`
}

type stageChange struct {
	Name  string `json:"name"`
	Base  string `json:"base" jsonschema:"its status in base, empty when base had no such stage"`
	Build string `json:"build" jsonschema:"its status in build, empty when build has no such stage"`
}

type caseChange struct {
	Stage   string `json:"stage,omitempty"`
	Suite   string `json:"suite,omitempty"`
	Name    string `json:"name"`
	Message string `json:"message,omitempty"`
}

type compareOut struct {
	Base          briefOut      `json:"base"`
	Build         briefOut      `json:"build"`
	CommitChanged bool          `json:"commit_changed"`
	Params        []paramChange `json:"params_changed"`
	Stages        []stageChange `json:"stages_changed"`
	NewlyFailing  []caseChange  `json:"newly_failing" jsonschema:"tests that fail in build and did not fail in base"`
	Fixed         []caseChange  `json:"fixed" jsonschema:"tests that fail in base and pass in build"`
	StillFailing  int           `json:"still_failing"`
	Cut           bool          `json:"cut"`
}

func (s *Server) compareBuilds(ctx context.Context, c *Caller, in compareIn) (compareOut, error) {
	out := compareOut{Params: []paramChange{}, Stages: []stageChange{}, NewlyFailing: []caseChange{}, Fixed: []caseChange{}}
	if in.Base < 0 || in.Base != 0 && in.Base == in.Number {
		return out, badInput("base is the number of another build of the job")
	}
	b, err := s.build(ctx, c, in.jobRef)
	if err != nil {
		return out, err
	}
	base, err := s.baseOf(ctx, c, b, in.Base)
	if err != nil {
		return out, err
	}
	svc := s.cfg.Service
	bv, err := svc.Build(ctx, c.User, b.ID)
	if err != nil {
		return out, err
	}
	basev, err := svc.Build(ctx, c.User, base.ID)
	if err != nil {
		return out, err
	}
	out.Base, out.Build = s.briefOf(base), s.briefOf(b)
	out.CommitChanged = base.BuiltCommit != b.BuiltCommit
	out.Params = paramChanges(base.Params, b.Params)
	out.Stages = stageChanges(basev, bv)
	baseTests, err := svc.Tests(ctx, c.User, base.ID)
	if err != nil {
		return out, err
	}
	tests, err := svc.Tests(ctx, c.User, b.ID)
	if err != nil {
		return out, err
	}
	s.testChanges(&out, baseTests, tests)
	return out, nil
}

func (s *Server) baseOf(ctx context.Context, c *Caller, b *store.Build, base int) (*store.Build, error) {
	if base != 0 {
		return s.build(ctx, c, jobRef{Job: b.Job, Number: base})
	}
	page, _, err := s.cfg.Service.Builds(ctx, c.User, b.Job, b.Number, 1)
	if err != nil {
		return nil, err
	}
	if len(page) == 0 {
		return nil, badInput("build #%d has no earlier build to compare with; give base", b.Number)
	}
	return page[0], nil
}

func (s *Server) briefOf(b *store.Build) briefOut {
	return briefOut{Number: b.Number, Status: string(b.Status), Commit: b.BuiltCommit, Params: b.Params, URL: s.buildURL(b.Job, b.Number)}
}

func paramChanges(base, build map[string]string) []paramChange {
	out := []paramChange{}
	names := slices.Sorted(maps.Keys(base))
	for name := range build {
		if _, ok := base[name]; !ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		if base[name] != build[name] {
			out = append(out, paramChange{name, base[name], build[name]})
		}
	}
	return out
}

func stageChanges(base, build *api.BuildView) []stageChange {
	out := []stageChange{}
	was := map[string]string{}
	for _, st := range base.Stages {
		was[st.Name] = string(st.Status)
	}
	seen := map[string]bool{}
	for _, st := range build.Stages {
		seen[st.Name] = true
		if now := string(st.Status); was[st.Name] != now {
			out = append(out, stageChange{st.Name, was[st.Name], now})
		}
	}
	for _, st := range base.Stages {
		if !seen[st.Name] {
			out = append(out, stageChange{st.Name, string(st.Status), ""})
		}
	}
	return out
}

func caseKey(t store.Test) string { return t.Stage + "\x00" + t.Suite + "\x00" + t.Name }

func worst(tests []store.Test) map[string]string {
	out := map[string]string{}
	for _, t := range tests {
		if k := caseKey(t); out[k] != "failed" {
			out[k] = t.Status
		}
	}
	return out
}

func (s *Server) testChanges(out *compareOut, base, build []store.Test) {
	was, now := worst(base), worst(build)
	add := func(list *[]caseChange, t store.Test) {
		if len(*list) == maxChanges {
			out.Cut = true
			return
		}
		*list = append(*list, caseChange{t.Stage, t.Suite, t.Name, clip(t.Message, maxMessage)})
	}
	seen := map[string]bool{}
	for _, t := range build {
		k := caseKey(t)
		if seen[k] || now[k] != t.Status {
			continue
		}
		seen[k] = true
		switch {
		case t.Status == "failed" && was[k] == "failed":
			out.StillFailing++
		case t.Status == "failed":
			add(&out.NewlyFailing, t)
		case t.Status == "passed" && was[k] == "failed":
			add(&out.Fixed, t)
		}
	}
}

type testHistoryIn struct {
	Job    string `json:"job" jsonschema:"the job's name"`
	Builds int    `json:"builds,omitempty" jsonschema:"how many of the job's last builds to look at, default 20, at most 50"`
	Name   string `json:"name,omitempty" jsonschema:"only the tests whose suite or name holds this, in any case"`
	Limit  int    `json:"limit,omitempty" jsonschema:"how many tests to list, default 50, at most 200"`
}

type historyCase struct {
	Suite    string   `json:"suite,omitempty"`
	Name     string   `json:"name"`
	Statuses []string `json:"statuses" jsonschema:"one per build, in the order builds lists them: passed, failed, skipped, or absent where the build did not run it"`
	Failed   int      `json:"failed" jsonschema:"in how many of the builds it failed"`
	Flips    int      `json:"flips" jsonschema:"how often it went from passed to failed or back, oldest to newest"`
}

type testHistoryOut struct {
	Builds []int         `json:"builds" jsonschema:"the numbers of the builds that reported tests, newest first"`
	Tests  []historyCase `json:"tests" jsonschema:"the tests that flip the most first, then those that fail the most"`
	Cut    bool          `json:"cut"`
}

func (s *Server) testHistory(ctx context.Context, c *Caller, in testHistoryIn) (testHistoryOut, error) {
	out := testHistoryOut{Builds: []int{}, Tests: []historyCase{}}
	switch {
	case in.Builds < 0 || in.Builds > maxHistory:
		return out, badInput("builds is 1 to %d", maxHistory)
	case in.Limit < 0 || in.Limit > maxTests:
		return out, badInput("limit is 1 to %d", maxTests)
	}
	if err := s.guard(ctx, c, in.Job, "view"); err != nil {
		return out, err
	}
	rows, err := s.cfg.Service.TestHistory(ctx, c.User, in.Job, or(in.Builds, defaultHistoryBuild))
	if err != nil {
		return out, err
	}
	cases := historyOf(rows, &out.Builds, strings.ToLower(in.Name))
	slices.SortFunc(cases, func(a, b historyCase) int {
		return cmp.Or(cmp.Compare(b.Flips, a.Flips), cmp.Compare(b.Failed, a.Failed), cmp.Compare(a.Suite, b.Suite), cmp.Compare(a.Name, b.Name))
	})
	limit := or(in.Limit, defaultTests)
	out.Cut = len(cases) > limit
	out.Tests = append(out.Tests, cases[:min(len(cases), limit)]...)
	return out, nil
}

func historyOf(rows []store.TestRow, builds *[]int, filter string) []historyCase {
	at, order := gather(rows, builds, filter)
	out := make([]historyCase, 0, len(order))
	for _, k := range order {
		out = append(out, caseHistory(k, at[k], *builds))
	}
	return out
}

type testName struct{ suite, name string }

func gather(rows []store.TestRow, builds *[]int, filter string) (map[testName]map[int]string, []testName) {
	at := map[testName]map[int]string{}
	var order []testName
	for _, r := range rows {
		if n := len(*builds); n == 0 || (*builds)[n-1] != r.BuildNumber {
			*builds = append(*builds, r.BuildNumber)
		}
		if filter != "" && !strings.Contains(strings.ToLower(r.Suite), filter) && !strings.Contains(strings.ToLower(r.Name), filter) {
			continue
		}
		k := testName{r.Suite, r.Name}
		if at[k] == nil {
			at[k] = map[int]string{}
			order = append(order, k)
		}
		if at[k][r.BuildNumber] != "failed" {
			at[k][r.BuildNumber] = r.Status
		}
	}
	return at, order
}

func caseHistory(k testName, by map[int]string, builds []int) historyCase {
	h := historyCase{Suite: k.suite, Name: k.name, Statuses: make([]string, len(builds))}
	last := ""
	for i := len(builds) - 1; i >= 0; i-- {
		st, ok := by[builds[i]]
		if !ok {
			st = "absent"
		}
		h.Statuses[i] = st
		if st == "failed" {
			h.Failed++
		}
		if st != "passed" && st != "failed" {
			continue
		}
		if last != "" && last != st {
			h.Flips++
		}
		last = st
	}
	return h
}
