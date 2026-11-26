package problem_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/problem"
)

func decode(t *testing.T, rec *httptest.ResponseRecorder) problem.Doc {
	t.Helper()
	var d problem.Doc
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("the body is not a problem: %v\n%s", err, rec.Body)
	}
	return d
}

func TestAProblemHasTheFiveMembersOfRFC9457AndItsMediaType(t *testing.T) {
	rec := httptest.NewRecorder()
	problem.Write(rec, problem.Conflict, "the job is disabled")
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("Content-Type = %q", got)
	}
	d := decode(t, rec)
	want := problem.Doc{
		Type: "https://github.com/ndthuan/kipp/blob/main/api/problems.md#conflict", Title: problem.Conflict.Title,
		Status: 409, Detail: "the job is disabled", Code: "conflict",
	}
	if d != want || rec.Code != 409 {
		t.Errorf("got %d %+v, want %+v", rec.Code, d, want)
	}
}

func fallbackMux() *http.ServeMux {
	mux := http.NewServeMux()
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.Handle("GET /api/v1/things/{id}", ok)
	mux.Handle("DELETE /api/v1/things/{id}", ok)
	mux.Handle("/api/v1/", problem.Fallback(mux, "/api/v1/"))
	return mux
}

func TestAPathNobodyServesIsA404Problem(t *testing.T) {
	rec := httptest.NewRecorder()
	fallbackMux().ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/nothing", nil))
	if rec.Code != 404 || decode(t, rec).Code != "not_found" {
		t.Errorf("got %d %s", rec.Code, rec.Body)
	}
}

func TestAWrongMethodIsA405ProblemThatNamesTheMethodsServed(t *testing.T) {
	rec := httptest.NewRecorder()
	fallbackMux().ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/things/3", nil))
	if rec.Code != 405 || decode(t, rec).Code != "method_not_allowed" {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD, DELETE" {
		t.Errorf("Allow = %q", got)
	}
}

func TestANetHTTPErrorIsCaughtAsAProblem(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(problem.Catch(w), "invalid range", http.StatusRequestedRangeNotSatisfiable)
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/x", nil))
	d := decode(t, rec)
	if rec.Code != 416 || d.Code != "range_not_satisfiable" || rec.Header().Get("Content-Type") != problem.MediaType {
		t.Errorf("got %d %s %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
}

func TestASuccessPassesThroughTheCatch(t *testing.T) {
	rec := httptest.NewRecorder()
	w := problem.Catch(rec)
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write([]byte("abc"))
	if rec.Code != 206 || rec.Body.String() != "abc" {
		t.Errorf("got %d %q", rec.Code, rec.Body)
	}
}

func TestEveryCodeIsListedInProblemsMdWithItsStatusAndTitle(t *testing.T) {
	doc, err := os.ReadFile("../../api/problems.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(doc)
	seen := map[string]bool{}
	for _, c := range problem.All() {
		if seen[c.Name] {
			t.Errorf("code %s is defined twice", c.Name)
		}
		seen[c.Name] = true
		if !strings.Contains(text, "\n## "+c.Name+"\n") {
			t.Errorf("api/problems.md has no heading for %s", c.Name)
			continue
		}
		_, section, _ := strings.Cut(text, "\n## "+c.Name+"\n")
		section, _, _ = strings.Cut(section, "\n## ")
		for _, want := range []string{"Status: " + strconv.Itoa(c.Status), "Title: " + c.Title} {
			if !strings.Contains(section, want) {
				t.Errorf("api/problems.md, %s: want a line %q", c.Name, want)
			}
		}
	}
	for line := range strings.SplitSeq(text, "\n") {
		if name, ok := strings.CutPrefix(line, "## "); ok && !seen[name] {
			t.Errorf("api/problems.md lists %s, which no code defines", name)
		}
	}
}
