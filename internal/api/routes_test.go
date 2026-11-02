package api

import (
	"encoding/json"
	"strings"
	"testing"
)

var pathValues = strings.NewReplacer("{name}", "smoke", "{id}", "999999", "{stage}", "s", "{path...}", "a/b.txt", "{store}", "none", "{number}", "1")

// Every route is served under /api/v1 alone: the same path without the
// version is a not_found problem.
func TestEveryRouteIsServedUnderV1AndNowhereElse(t *testing.T) {
	e := newEnv(t)
	for _, r := range e.api.Routes() {
		if r.Path == "/queue/events" { // a stream that does not end
			continue
		}
		path := pathValues.Replace(r.Path)
		if code, body, _ := e.raw(t, e.admin, r.Method, PrefixV1+path, ""); code == 404 && strings.Contains(body, "no such path") {
			t.Errorf("%s %s is not served under /api/v1", r.Method, r.Path)
		}
		e.asProblem(t, 404, "not_found", e.admin, r.Method, "/api"+path, "")
	}
}

// canonical is a JSON text with its keys in order and no whitespace, so that
// two answers compare by what they say.
func canonical(t *testing.T, text string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatalf("not JSON: %q", text)
	}
	out, err := json.Marshal(v)
	must(t, err)
	return string(out)
}
