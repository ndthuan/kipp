package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/problem"
	"github.com/ndthuan/kipp/internal/secretbox"
	"github.com/ndthuan/kipp/internal/store"
)

var adminReads = []string{"/api/v1/admin/users", "/api/v1/admin/users/aide", "/api/v1/admin/secrets", "/api/v1/admin/server", "/api/v1/admin/audit", "/api/v1/admin/engines", "/api/v1/admin/runners"}

func TestTheAdminReadsAreRefusedToANonAdminWith403(t *testing.T) {
	e := newEnv(t)
	for _, path := range adminReads {
		for _, tok := range []string{e.aide, e.viewer} {
			e.asProblem(t, 403, "forbidden", tok, "GET", path, "")
		}
		e.call(t, 401, "", "GET", path, "")
	}
}

// decode is call with the answer decoded into out: a list's rows when out
// is a slice.
func (e *env) decode(t *testing.T, path string, out any) string {
	t.Helper()
	text, _ := e.call(t, 200, e.admin, "GET", path, "")
	data := []byte(text)
	if reflect.TypeOf(out).Elem().Kind() == reflect.Slice {
		var list struct{ Items json.RawMessage }
		must(t, json.Unmarshal(data, &list))
		data = list.Items
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatalf("%s: %v: %q", path, err, text)
	}
	return text
}

// utc fails unless every time in an answer is RFC 3339 in UTC with a Z.
func utc(t *testing.T, text string, fields ...string) {
	t.Helper()
	var v any
	must(t, json.Unmarshal([]byte(text), &v))
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, f := range x {
				if s, ok := f.(string); ok && (k == "at" || strings.HasSuffix(k, "_at")) && !strings.HasSuffix(s, "Z") {
					t.Fatalf("%s is %q, not UTC with a Z", k, s)
				}
				walk(f)
			}
		case []any:
			for _, f := range x {
				walk(f)
			}
		}
	}
	walk(v)
	for _, f := range fields {
		contains(t, text, `"`+f+`":"`)
	}
}

func TestUsersAnswerGrantsTokensAndSessionsAndNeverAHash(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.person(t, "alice", false)
	must(t, e.st.Grant(ctx, alice.u.ID, "smoke", []string{"view", "build"}))
	must(t, e.st.Grant(ctx, alice.u.ID, "gone", []string{"view"}))
	hash := "an obvious test token hash"
	_, err := e.st.CreateTokenUntil(ctx, alice.u.ID, "ci", hash, new(time.Now().Add(time.Hour)))
	must(t, err)

	var all []struct{ Name string }
	text := e.decode(t, "/api/v1/admin/users", &all)
	if strings.Contains(text, hash) || strings.Contains(text, "hash") || strings.Contains(text, alice.pw) {
		t.Fatalf("a hash or a password is in %s", text)
	}
	names := []string{}
	for _, u := range all {
		names = append(names, u.Name)
	}
	eq(t, "users", strings.Join(names, ","), "admin,aide,alice,viewer")

	var one map[string]any
	text = e.decode(t, "/api/v1/admin/users/alice", &one)
	utc(t, text, "last_login_at", "created_at", "expires_at")
	want := `{"name":"alice","admin":false,"grants":{"gone":["view"],"smoke":["view","build"]},"gone_grants":["gone"],"sessions":1}`
	var got map[string]any
	must(t, json.Unmarshal([]byte(want), &got))
	for k, v := range got {
		if fmt.Sprint(one[k]) != fmt.Sprint(v) {
			t.Fatalf("%s: got %v, want %v", k, one[k], v)
		}
	}
	tokens := one["tokens"].([]any)
	tok := tokens[0].(map[string]any)
	if len(tokens) != 1 || tok["label"] != "ci" || tok["id"] == nil || tok["last_used_at"] != nil || tok["expires_at"] == nil {
		t.Fatalf("tokens: %v", tokens)
	}
	e.call(t, 404, e.admin, "GET", "/api/v1/admin/users/nobody", "")
}

func TestSecretsAnswerNamesTimesAndJobsAndNeverAValue(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.withJobs(t, map[string]string{
		"deploy.yml": "name: deploy\nsecrets: [TOKEN, SSH_KEY]\ntriggers: {webhook: {repo: o/deploy}}\nstages:\n  s: {run: echo}\n",
	})
	const value = "an obvious test value"
	sealed, err := secretbox.Seal(testSecretKey, "TOKEN", []byte(value))
	must(t, err)
	must(t, e.st.PutSecret(ctx, "TOKEN", sealed))
	var got struct {
		Set, Missing []map[string]any
	}
	text := e.decode(t, "/api/v1/admin/secrets", &got)
	utc(t, text, "updated_at")
	if strings.Contains(text, value) || strings.Contains(text, string(sealed)) {
		t.Fatalf("a value is in %s", text)
	}
	if len(got.Set) != 1 || got.Set[0]["name"] != "TOKEN" || fmt.Sprint(got.Set[0]["jobs"]) != "[deploy]" || got.Set[0]["webhook"] != false {
		t.Fatalf("set: %v", got.Set)
	}
	if len(got.Missing) != 2 || got.Missing[0]["name"] != "GITHUB_WEBHOOK_DEPLOY" || got.Missing[0]["webhook"] != true ||
		got.Missing[0]["updated_at"] != nil || got.Missing[1]["name"] != "SSH_KEY" {
		t.Fatalf("missing: %v", got.Missing)
	}
}

func TestServerAnswersWhatServeSaysWithTheJobsLoaded(t *testing.T) {
	e := newEnv(t)
	var got map[string]any
	text := e.decode(t, "/api/v1/admin/server", &got)
	utc(t, text, "started_at", "last_reload_at", "checked_at")
	if got["origin"] != "Homebrew" || got["update_check"] != true || got["latest"] != "v9.9.9" || got["upgradable"] != true ||
		got["notes_url"] != "https://github.com/ndthuan/kipp/releases/tag/v9.9.9" || got["checked_at"] != "2026-01-02T03:04:05.000Z" {
		t.Fatalf("the update fields: %v", got)
	}
	if got["version"] != "test" || got["listen"] != "127.0.0.1:8085" || got["executors"] != float64(4) || got["jobs"] != float64(len(jobFiles)) ||
		got["free_disk_bytes"] != nil || got["last_reload_error"] != "" {
		t.Fatalf("%v", got)
	}
}

// auditRow is an audit row as a client reads it.
type auditRow struct {
	ID                                int64
	At, Actor, Action, Target, Detail string
}

func TestTheAuditIsAnsweredNewestFirstAPageAtATime(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for i := range 5 {
		must(t, e.st.AppendAudit(ctx, store.Audit{At: time.Now(), Actor: "cli", Action: "user.add", Target: "u" + itoa(int64(i)), Detail: "user"}))
	}
	var first []auditRow
	text := e.decode(t, "/api/v1/admin/audit?limit=2", &first)
	utc(t, text, "at")
	if len(first) != 2 || first[0].Target != "u4" || first[1].Target != "u3" || first[0].Actor != "cli" || first[0].Action != "user.add" || first[0].Detail != "user" {
		t.Fatalf("first page: %+v", first)
	}
	var next []auditRow
	e.decode(t, "/api/v1/admin/audit?cursor="+cursorFor("audit", first[1].ID), &next)
	eq(t, "next page", len(next), 3)
	var end []auditRow
	e.decode(t, "/api/v1/admin/audit?cursor="+cursorFor("audit", next[2].ID), &end)
	eq(t, "past the end", len(end), 0)
}

func TestTheAuditAnswersAHundredRowsUnlessAskedAndFiveHundredAtMost(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for range 501 {
		must(t, e.st.AppendAudit(ctx, store.Audit{At: time.Now(), Actor: "cli", Action: "reload"}))
	}
	for path, want := range map[string]int{"/api/v1/admin/audit": 100, "/api/v1/admin/audit?limit=7": 7, "/api/v1/admin/audit?limit=1000": 500} {
		var got []auditRow
		e.decode(t, path, &got)
		eq(t, path, len(got), want)
	}
}

func TestAPasswordRefusalIsAnsweredAsWhatItIs(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		err   error
		code  int
		retry string
	}{
		{ErrSudoRequired, 403, ""},
		{ErrWrongPassword, 403, ""},
		{fmt.Errorf("sudo: %w", &auth.ThrottledError{RetryAfter: 1500 * time.Millisecond}), 429, "2"},
		{auth.ErrBusy, 503, "2"},
	} {
		w := httptest.NewRecorder()
		e.api.fail(w, httptest.NewRequest("POST", "/api/v1/x", nil), c.err)
		var body problem.Doc
		must(t, json.Unmarshal(w.Body.Bytes(), &body))
		if w.Code != c.code || w.Header().Get("Retry-After") != c.retry || body.Detail == "" || body.Status != c.code {
			t.Fatalf("%v: %d %q %v", c.err, w.Code, w.Header().Get("Retry-After"), body)
		}
		if errors.Is(c.err, ErrWrongPassword) && body.Detail != "wrong password" {
			t.Fatalf("wrong password says %q", body.Detail)
		}
	}
}

func TestSecretsAnswerScopesTheHintAndWhatIsOutOfScope(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.withJobs(t, map[string]string{
		"deploy.yml": "name: deploy\nsecrets: [TOKEN]\nstages:\n  s: {run: echo}\n",
	})
	sealed, err := secretbox.Seal(testSecretKey, "TOKEN", []byte("an obvious test value"))
	must(t, err)
	must(t, e.st.PutSecret(ctx, "TOKEN", sealed))
	var got struct {
		Set        []map[string]any
		OutOfScope []map[string]any `json:"out_of_scope"`
	}
	e.decode(t, "/api/v1/admin/secrets", &got)
	if len(got.Set) != 1 || fmt.Sprint(got.Set[0]["scopes"]) != "[global]" || got.Set[0]["narrow"] != true || len(got.OutOfScope) != 0 {
		t.Fatalf("%v", got)
	}
	must(t, e.st.SetSecretScopes(ctx, "TOKEN", []string{"job:other"}))
	e.decode(t, "/api/v1/admin/secrets", &got)
	if len(got.OutOfScope) != 1 || got.OutOfScope[0]["name"] != "TOKEN" || fmt.Sprint(got.OutOfScope[0]["jobs"]) != "[deploy]" {
		t.Fatalf("%v", got.OutOfScope)
	}
}

func TestServerSaysNullForCheckedAtBeforeAnyAnswerAndEmptyForNoNewerRelease(t *testing.T) {
	out := toServerJSON(&ServerInfo{Version: "v1.0.0", Origin: "Docker image", UpdateCheck: true})
	b, err := json.Marshal(out)
	must(t, err)
	var got map[string]any
	must(t, json.Unmarshal(b, &got))
	if got["checked_at"] != nil || got["latest"] != "" || got["notes_url"] != "" || got["upgradable"] != false || got["origin"] != "Docker image" {
		t.Fatalf("%v", got)
	}
}
