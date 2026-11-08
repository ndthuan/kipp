package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/apispec/apispectest"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/runner"
	"github.com/ndthuan/kipp/internal/store"
)

const (
	sha  = "0123456789abcdef0123456789abcdef01234567"
	push = `{"ref":"refs/heads/main","after":"` + sha + `","repository":{"full_name":"example/app"}}`
)

func server(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	return serverWith(t, func(_ context.Context, name string) (string, error) {
		if name == "GITHUB_WEBHOOK_SMOKE" || name == "GITHUB_WEBHOOK_OFF" || name == "GITHUB_WEBHOOK_BRANCHES" || name == "GITHUB_WEBHOOK_RELEASE" {
			return "topsecret", nil
		}
		return "", store.ErrNotFound
	})
}

func serverWith(t *testing.T, secret func(context.Context, string) (string, error)) (*httptest.Server, *store.Store) {
	t.Helper()
	return serverOpts(t, Options{Secret: secret})
}

// serverOpts is the test server with o; a nil Record keeps deliveries in
// the server's store.
func serverOpts(t *testing.T, o Options) (*httptest.Server, *store.Store) {
	t.Helper()
	srv, st, _ := serverDispatch(t, o)
	return srv, st
}

// serverDispatch is serverOpts that also gives the dispatcher behind it.
func serverDispatch(t *testing.T, o Options) (*httptest.Server, *store.Store, *dispatch.Dispatcher) {
	t.Helper()
	return serverWrapped(t, o, func(h http.Handler) http.Handler { return h })
}

// serverWrapped is serverDispatch with the webhook handler wrapped by wrap.
func serverWrapped(t *testing.T, o Options, wrap func(http.Handler) http.Handler) (*httptest.Server, *store.Store, *dispatch.Dispatcher) {
	t.Helper()
	dir := t.TempDir()
	jobs := filepath.Join(dir, "jobs")
	must(t, os.MkdirAll(jobs, 0o750))
	must(t, os.WriteFile(filepath.Join(jobs, "smoke.yml"), []byte("name: smoke\ntriggers: {webhook: {repo: example/app, branches: [main]}}\nstages:\n  s: {run: echo $KIPP_COMMIT}\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(jobs, "branches.yml"), []byte("name: branches\ntriggers: {webhook: {repo: example/app, branches: [\"APP-*\", \"epic/APP-*\"]}}\nconcurrency: parallel\nstages:\n  s: {run: echo $KIPP_BRANCH}\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(jobs, "release.yml"), []byte("name: release\ntriggers: {webhook: {repo: example/app, tags: [\"v*\"]}}\nconcurrency: parallel\nstages:\n  s: {run: echo $KIPP_TAG}\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(jobs, "plain.yml"), []byte("name: plain\nstages:\n  s: {run: \"true\"}\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(jobs, "off.yml"), []byte("name: off\ndisabled: true\ntriggers: {webhook: {repo: example/app}}\nstages:\n  s: {run: \"true\"}\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(jobs, "nosecret.yml"), []byte("name: nosecret\ntriggers: {webhook: {repo: example/app}}\nstages:\n  s: {run: \"true\"}\n"), 0o600))
	snap, err := config.Load(jobs)
	must(t, err)
	st, err := store.Open(filepath.Join(dir, "db"))
	must(t, err)
	d := dispatch.New(st, runner.Local{}, dispatch.Options{DataDir: dir})
	d.SetSnapshot(snap)
	if o.Record == nil {
		o.Record = func(ctx context.Context, id, job string) error { return st.RecordDelivery(ctx, id, job, time.Now()) }
	}
	h := GitHub(d, o)
	mux := http.NewServeMux()
	mux.Handle("POST /api/v1/hooks/github/{job}", wrap(h))
	srv := httptest.NewServer(apispectest.Wrap(t, mux))
	t.Cleanup(func() { srv.Close(); _ = st.Close() })
	return srv, st, d
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func sign(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// deliveries numbers the deliveries the tests send, each its own.
var deliveries atomic.Int64

func newDelivery() string { return fmt.Sprintf("delivery-%d", deliveries.Add(1)) }

func post(t *testing.T, url, event, sig, body string) (int, string) {
	t.Helper()
	code, out, _ := send(t, url, event, sig, newDelivery(), body)
	return code, out
}

// send posts a delivery; an empty sig or delivery sends no such header.
func send(t *testing.T, url, event, sig, delivery, body string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	must(t, err)
	req.Header.Set("X-GitHub-Event", event)
	if delivery != "" {
		req.Header.Set("X-GitHub-Delivery", delivery)
	}
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	res, err := http.DefaultClient.Do(req)
	must(t, err)
	out, err := io.ReadAll(res.Body)
	must(t, err)
	must(t, res.Body.Close())
	return res.StatusCode, string(out), res.Header
}

func TestAMatchingPushQueuesABuildWithTheCommit(t *testing.T) {
	srv, st := server(t)
	if code, body := post(t, srv.URL+"/api/v1/hooks/github/smoke", "push", sign("topsecret", push), push); code != 202 {
		t.Fatal(code, body)
	}
	bs, err := st.Builds(context.Background(), "smoke", 5)
	must(t, err)
	if len(bs) != 1 || bs[0].Trigger != "webhook" || bs[0].TriggerDetail != sha {
		t.Fatalf("%+v", bs)
	}
}

func TestOnlyASignedPushToAWatchedBranchWithAFullCommitQueuesABuild(t *testing.T) {
	srv, st := server(t)
	other := func(from, to string) string { return strings.Replace(push, from, to, 1) }
	zero := strings.Repeat("0", 40)
	cases := []struct {
		name, path, event, sig, body string
		want                         int
	}{
		{"no signature", "/api/v1/hooks/github/smoke", "push", "", push, 401},
		{"wrong secret", "/api/v1/hooks/github/smoke", "push", sign("nope", push), push, 401},
		{"sha1 header only", "/api/v1/hooks/github/smoke", "push", "sha1=" + strings.Repeat("a", 40), push, 401},
		{"signature of other body", "/api/v1/hooks/github/smoke", "push", sign("topsecret", other("main", "dev")), push, 401},
		{"malformed hex", "/api/v1/hooks/github/smoke", "push", "sha256=zz", push, 401},
		{"unknown job", "/api/v1/hooks/github/zzz", "push", sign("topsecret", push), push, 401},
		{"job without webhook", "/api/v1/hooks/github/plain", "push", sign("topsecret", push), push, 401},
		{"secret not set", "/api/v1/hooks/github/nosecret", "push", sign("topsecret", push), push, 401},
		{"ping event", "/api/v1/hooks/github/smoke", "ping", sign("topsecret", `{"zen":"x"}`), `{"zen":"x"}`, 200},
		{"other branch", "/api/v1/hooks/github/smoke", "push", sign("topsecret", other("main", "dev")), other("main", "dev"), 200},
		{"a tag", "/api/v1/hooks/github/smoke", "push", sign("topsecret", other("refs/heads/main", "refs/tags/v1")), other("refs/heads/main", "refs/tags/v1"), 200},
		{"other repo", "/api/v1/hooks/github/smoke", "push", sign("topsecret", other("example/app", "x/y")), other("example/app", "x/y"), 200},
		{"branch deleted by zeros", "/api/v1/hooks/github/smoke", "push", sign("topsecret", other(sha, zero)), other(sha, zero), 200},
		{"branch deleted by flag", "/api/v1/hooks/github/smoke", "push", sign("topsecret", other(`"after"`, `"deleted":true,"after"`)), other(`"after"`, `"deleted":true,"after"`), 200},
		{"short commit", "/api/v1/hooks/github/smoke", "push", sign("topsecret", other(sha, "abc123")), other(sha, "abc123"), 400},
		{"non-hex commit", "/api/v1/hooks/github/smoke", "push", sign("topsecret", other(sha, strings.Repeat("g", 40))), other(sha, strings.Repeat("g", 40)), 400},
		{"flag-like commit", "/api/v1/hooks/github/smoke", "push", sign("topsecret", other(sha, "--upload-pack=x")), other(sha, "--upload-pack=x"), 400},
		{"not json", "/api/v1/hooks/github/smoke", "push", sign("topsecret", "{"), "{", 400},
	}
	for _, c := range cases {
		if got, _ := post(t, srv.URL+c.path, c.event, c.sig, c.body); got != c.want {
			t.Errorf("%s: %d want %d", c.name, got, c.want)
		}
	}
	bs, err := st.Builds(context.Background(), "smoke", 5)
	must(t, err)
	if len(bs) != 0 {
		t.Fatalf("a mismatch queued a build: %+v", bs)
	}
}

func TestAPushToABranchAPatternWatchesQueuesABuildWithTheBranch(t *testing.T) {
	srv, st := server(t)
	at := func(branch string) string { return strings.Replace(push, "refs/heads/main", "refs/heads/"+branch, 1) }
	zero := strings.Repeat("0", 40)
	cases := []struct {
		name, body string
		want       int
	}{
		{"a ticket branch", at("APP-12-fix"), 202},
		{"an epic branch", at("epic/APP-3-big"), 202},
		{"main is not watched", at("main"), 200},
		{"the prefix alone", at("APP"), 200},
		{"a star does not cross a slash", at("feature/APP-1-x"), 200},
		{"a deleted ticket branch", strings.Replace(at("APP-13-gone"), sha, zero, 1), 200},
		{"a deleted ticket branch by flag", strings.Replace(at("APP-14-gone"), `"after"`, `"deleted":true,"after"`, 1), 200},
	}
	for _, c := range cases {
		if got, out := post(t, srv.URL+"/api/v1/hooks/github/branches", "push", sign("topsecret", c.body), c.body); got != c.want {
			t.Errorf("%s: %d want %d (%s)", c.name, got, c.want, out)
		}
	}
	bs, err := st.Builds(context.Background(), "branches", 10)
	must(t, err)
	var got []string
	for _, b := range bs {
		got = append(got, b.TriggerBranch+"@"+b.TriggerDetail)
	}
	slices.Sort(got)
	if want := []string{"APP-12-fix@" + sha, "epic/APP-3-big@" + sha}; !slices.Equal(got, want) {
		t.Fatalf("builds %v, want %v", got, want)
	}
}

func TestASha256CommitIsTakenAndARefusalEchoesNothing(t *testing.T) {
	srv, _ := server(t)
	long := strings.Repeat("ab", 32)
	body := strings.Replace(push, sha, long, 1)
	if code, _ := post(t, srv.URL+"/api/v1/hooks/github/smoke", "push", sign("topsecret", body), body); code != 202 {
		t.Fatal(code)
	}
	_, out := post(t, srv.URL+"/api/v1/hooks/github/smoke", "push", sign("nope", push), push)
	if strings.Contains(out, "topsecret") || strings.Contains(out, sha) {
		t.Fatalf("response echoes input: %q", out)
	}
}

// GitHub sends lower case; whatever arrives, the build records one form.
func TestACommitInUpperCaseIsRecordedInLowerCase(t *testing.T) {
	srv, st := server(t)
	body := strings.Replace(push, sha, strings.ToUpper(sha), 1)
	if code, out := post(t, srv.URL+"/api/v1/hooks/github/smoke", "push", sign("topsecret", body), body); code != 202 {
		t.Fatal(code, out)
	}
	bs, err := st.Builds(context.Background(), "smoke", 5)
	must(t, err)
	if len(bs) != 1 || bs[0].TriggerDetail != strings.ToLower(sha) {
		t.Fatalf("%+v", bs)
	}
}

func TestAPushThatIsNotThisJobsSaysWhyItWasIgnored(t *testing.T) {
	srv, _ := server(t)
	body := strings.Replace(push, "refs/heads/main", "refs/tags/v1", 1)
	code, out := post(t, srv.URL+"/api/v1/hooks/github/smoke", "push", sign("topsecret", body), body)
	if code != 200 || strings.TrimSpace(out) != `{"ignored":"tag v1 is not watched"}` {
		t.Fatal(code, out)
	}
}

func TestARefThatIsNeitherABranchNorATagIsIgnored(t *testing.T) {
	srv, _ := server(t)
	body := strings.Replace(push, "refs/heads/main", "refs/notes/commits", 1)
	code, out := post(t, srv.URL+"/api/v1/hooks/github/smoke", "push", sign("topsecret", body), body)
	if code != 200 || strings.TrimSpace(out) != `{"ignored":"refs/notes/commits is neither a branch nor a tag"}` {
		t.Fatal(code, out)
	}
}

// tagPush is a push of tag at after; head, when given, is the commit
// GitHub names as head_commit, which an annotated tag's after is not.
func tagPush(tag, after, head string) string {
	body := `{"ref":"refs/tags/` + tag + `","after":"` + after + `","repository":{"full_name":"example/app","default_branch":"main"}`
	if head != "" {
		body += `,"head_commit":{"id":"` + head + `"}`
	}
	return body + "}"
}

func TestAPushOfATagTheJobWatchesQueuesABuildOfTheCommitTheTagNames(t *testing.T) {
	srv, st := server(t)
	tagObject, zero := strings.Repeat("b", 40), strings.Repeat("0", 40)
	cases := []struct {
		name, body string
		want       int
	}{
		{"a lightweight tag", tagPush("v1.0.0", sha, sha), 202},
		{"an annotated tag builds its commit", tagPush("v1.1.0", tagObject, strings.Repeat("c", 40)), 202},
		{"no head_commit builds after", tagPush("v1.2.0", strings.Repeat("d", 40), ""), 202},
		{"a tag the job does not watch", tagPush("nightly", sha, sha), 200},
		{"a branch named like a watched tag", strings.Replace(push, "refs/heads/main", "refs/heads/v9", 1), 200},
		{"a deleted tag by zeros", tagPush("v1.3.0", zero, ""), 200},
		{"a deleted tag by flag", strings.Replace(tagPush("v1.4.0", sha, ""), `"after"`, `"deleted":true,"after"`, 1), 200},
		{"a head_commit that is no commit id", tagPush("v1.5.0", sha, "--upload-pack=x"), 400},
	}
	for _, c := range cases {
		if got, out := post(t, srv.URL+"/api/v1/hooks/github/release", "push", sign("topsecret", c.body), c.body); got != c.want {
			t.Errorf("%s: %d want %d (%s)", c.name, got, c.want, out)
		}
	}
	bs, err := st.Builds(context.Background(), "release", 10)
	must(t, err)
	var got []string
	for _, b := range bs {
		got = append(got, b.Trigger+" "+b.TriggerTag+"@"+b.TriggerDetail+" ["+b.TriggerBranch+"] "+b.TriggerDefaultBranch)
	}
	slices.Sort(got)
	want := []string{
		"webhook v1.0.0@" + sha + " [] main",
		"webhook v1.1.0@" + strings.Repeat("c", 40) + " [] main",
		"webhook v1.2.0@" + strings.Repeat("d", 40) + " [] main",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("builds %v, want %v", got, want)
	}
}

func TestAPushOfATagSaysWhyAJobIgnoredIt(t *testing.T) {
	srv, _ := server(t)
	for _, c := range []struct{ job, body, want string }{
		{"release", tagPush("nightly", sha, sha), `{"ignored":"tag nightly is not watched"}`},
		{"release", tagPush("v2.0.0", strings.Repeat("0", 40), ""), `{"ignored":"tag v2.0.0 was deleted"}`},
		{"release", push, `{"ignored":"branch main is not watched"}`},
	} {
		if code, out := post(t, srv.URL+"/api/v1/hooks/github/"+c.job, "push", sign("topsecret", c.body), c.body); code != 200 || strings.TrimSpace(out) != c.want {
			t.Errorf("%s: %d %s, want %s", c.job, code, out, c.want)
		}
	}
}

func TestSecondPushWhileQueuedIsADropWhenTheJobDrops(t *testing.T) {
	dir := t.TempDir()
	jobs := filepath.Join(dir, "jobs")
	must(t, os.MkdirAll(jobs, 0o750))
	must(t, os.WriteFile(filepath.Join(jobs, "d.yml"), []byte("name: d\nconcurrency: drop\ntriggers: {webhook: {repo: example/app}}\nstages:\n  s: {run: sleep 5}\n"), 0o600))
	snap, err := config.Load(jobs)
	must(t, err)
	st, err := store.Open(filepath.Join(dir, "db"))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	d := dispatch.New(st, runner.Local{}, dispatch.Options{DataDir: dir})
	d.SetSnapshot(snap)
	mux := http.NewServeMux()
	h := GitHub(d, Options{
		Secret: func(context.Context, string) (string, error) { return "k", nil },
		Record: func(ctx context.Context, id, job string) error { return st.RecordDelivery(ctx, id, job, time.Now()) },
	})
	mux.Handle("POST /api/v1/hooks/github/{job}", h)
	srv := httptest.NewServer(apispectest.Wrap(t, mux))
	defer srv.Close()
	if code, _ := post(t, srv.URL+"/api/v1/hooks/github/d", "push", sign("k", push), push); code != 202 {
		t.Fatal(code)
	}
	if code, _ := post(t, srv.URL+"/api/v1/hooks/github/d", "push", sign("k", push), push); code != 409 {
		t.Fatal(code)
	}
	// Another commit, so that it is not taken for the body sent before.
	other := strings.Replace(push, sha, strings.Repeat("a", 40), 1)
	code, body, hdr := send(t, srv.URL+"/api/v1/hooks/github/d", "push", sign("k", other), newDelivery(), other)
	asProblem(t, code, body, hdr, 409, "dropped")
}

func raw(t *testing.T, url, sig, body string) (int, string, http.Header) {
	t.Helper()
	code, out, h := send(t, url, "push", sig, newDelivery(), body)
	h = h.Clone()
	h.Del("Date")
	return code, out, h
}

func TestAnUnsignedCallerLearnsNothing(t *testing.T) {
	srv, _ := server(t)
	cases := []struct{ name, path, sig string }{
		{"unknown job", "/api/v1/hooks/github/zzz", sign("topsecret", push)},
		{"no webhook trigger", "/api/v1/hooks/github/plain", sign("topsecret", push)},
		{"secret not set", "/api/v1/hooks/github/nosecret", sign("topsecret", push)},
		{"missing signature", "/api/v1/hooks/github/smoke", ""},
		{"bad signature", "/api/v1/hooks/github/smoke", sign("nope", push)},
	}
	var code0 int
	var body0 string
	var hdr0 http.Header
	for i, c := range cases {
		code, body, hdr := raw(t, srv.URL+c.path, c.sig, push)
		if i == 0 {
			code0, body0, hdr0 = code, body, hdr
			if code != 401 {
				t.Fatalf("%s: %d", c.name, code)
			}
			continue
		}
		if code != code0 || body != body0 || !reflect.DeepEqual(hdr, hdr0) {
			t.Errorf("%s differs: %d %q %v vs %d %q %v", c.name, code, body, hdr, code0, body0, hdr0)
		}
	}
}

func TestAFailingSecretLookupIs500(t *testing.T) {
	srv, _ := serverWith(t, func(context.Context, string) (string, error) { return "", errors.New("store down: sekrit-detail") })
	code, body, _ := raw(t, srv.URL+"/api/v1/hooks/github/smoke", sign("topsecret", push), push)
	if code != 500 || strings.Contains(body, "sekrit-detail") {
		t.Fatal(code, body)
	}
}

func TestASignedPushToADisabledJobIsIgnoredAndAnUnsignedOneLearnsNothing(t *testing.T) {
	srv, st := server(t)
	code, body := post(t, srv.URL+"/api/v1/hooks/github/off", "push", sign("topsecret", push), push)
	if code != 200 || !strings.Contains(body, `"ignored":"the job is disabled"`) {
		t.Fatal(code, body)
	}
	if code, body := post(t, srv.URL+"/api/v1/hooks/github/off", "push", sign("wrong", push), push); code != 401 || strings.Contains(body, "disabled") {
		t.Fatal(code, body)
	}
	if bs, err := st.Builds(context.Background(), "off", 5); err != nil || len(bs) != 0 {
		t.Fatalf("%+v %v", bs, err)
	}
}

// head sends the head of a delivery to smoke over a connection of its own, saying a body
// of n bytes follows and sending none of it, and returns the connection.
func head(t *testing.T, srvURL, sig string, n int) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(srvURL, "http://"))
	must(t, err)
	t.Cleanup(func() { _ = c.Close() })
	req := fmt.Sprintf("POST /api/v1/hooks/github/smoke HTTP/1.1\r\nHost: x\r\nX-GitHub-Event: push\r\nX-GitHub-Delivery: %s\r\nContent-Length: %d\r\n", newDelivery(), n)
	if sig != "" {
		req += "X-Hub-Signature-256: " + sig + "\r\n"
	}
	_, err = io.WriteString(c, req+"\r\n")
	must(t, err)
	return c
}

// status reads the status line the server answers on c, within a time far
// longer than any answer should take.
func status(t *testing.T, c net.Conn) string {
	t.Helper()
	must(t, c.SetReadDeadline(time.Now().Add(10*time.Second)))
	line := make([]byte, len("HTTP/1.1 401"))
	if _, err := io.ReadFull(c, line); err != nil {
		t.Fatalf("no answer before the body was sent: %v", err)
	}
	return string(line)
}

func TestARequestWithoutASignatureIsRefusedBeforeItsBodyIsRead(t *testing.T) {
	srv, _ := server(t)
	for _, sig := range []string{"", "sha1=" + strings.Repeat("a", 40), "sha256=zz"} {
		c := head(t, srv.URL, sig, 1<<20)
		eq(t, "answer to "+sig, status(t, c), "HTTP/1.1 401")
	}
}

func TestABodyThatDoesNotArriveInTheReadTimeoutIs408(t *testing.T) {
	srv, _ := serverOpts(t, Options{Secret: func(context.Context, string) (string, error) { return "topsecret", nil }, ReadTimeout: 100 * time.Millisecond})
	c := head(t, srv.URL, sign("topsecret", push), len(push))
	eq(t, "answer", status(t, c), "HTTP/1.1 408")
}

func TestAtMostTwoBodiesAreReadAtOnceAndAThirdIs503WithRetryAfter(t *testing.T) {
	entered := make(chan struct{}, 2)
	srv, st, _ := serverWrapped(t, Options{Secret: func(context.Context, string) (string, error) { return "topsecret", nil }},
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case entered <- struct{}{}:
				default:
				}
				next.ServeHTTP(w, r)
			})
		})
	sig := sign("topsecret", push)
	// Two deliveries whose bodies never come hold both readers.
	head(t, srv.URL, sig, len(push))
	head(t, srv.URL, sig, len(push))
	// No third is sent until the server has taken up both: a third that
	// came first would be built, and the next one then be 409.
	for i := range 2 {
		select {
		case <-entered:
		case <-time.After(30 * time.Second):
			t.Fatalf("the server never took up held delivery %d", i+1)
		}
	}
	// Taking up a delivery and holding its reader are two steps; a bad
	// signature probes for the second without building or recording anything.
	url := srv.URL + "/api/v1/hooks/github/smoke"
	var code int
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end) && code != http.StatusServiceUnavailable; time.Sleep(5 * time.Millisecond) {
		code, _, _ = send(t, url, "push", sign("nope", push), newDelivery(), push)
	}
	eq(t, "probe", code, http.StatusServiceUnavailable)
	code, _, hdr := send(t, url, "push", sig, newDelivery(), push)
	eq(t, "third delivery", code, http.StatusServiceUnavailable)
	eq(t, "Retry-After", hdr.Get("Retry-After"), "10")
	if bs, err := st.Builds(context.Background(), "smoke", 5); err != nil || len(bs) != 0 {
		t.Fatalf("%+v %v", bs, err)
	}
}

func TestEveryAnswerOfTheWebhookIsNotToBeSniffed(t *testing.T) {
	srv, _ := server(t)
	for _, sig := range []string{"", sign("nope", push), sign("topsecret", push)} {
		code, _, hdr := send(t, srv.URL+"/api/v1/hooks/github/smoke", "push", sig, newDelivery(), push)
		eq(t, fmt.Sprintf("nosniff on %d", code), hdr.Get("X-Content-Type-Options"), "nosniff")
	}
}

func TestEveryNameLooksUpItsSecretSoTheTimeTellsNotWhichHaveAWebhook(t *testing.T) {
	var mu sync.Mutex
	looked := map[string]bool{}
	srv, _ := serverWith(t, func(_ context.Context, name string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		looked[name] = true
		return "", store.ErrNotFound
	})
	for _, job := range []string{"zzz", "plain", "nosecret", "smoke"} {
		if code, _ := post(t, srv.URL+"/api/v1/hooks/github/"+job, "push", sign("topsecret", push), push); code != 401 {
			t.Fatalf("%s: %d", job, code)
		}
		mu.Lock()
		eq(t, job+" looked up", looked[SecretName(job)], true)
		mu.Unlock()
	}
}

func eq[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

func TestADeliveryIsBuiltOnceAndItsIdSentAgainIs409(t *testing.T) {
	srv, st := server(t)
	sig := sign("topsecret", push)
	if code, _, _ := send(t, srv.URL+"/api/v1/hooks/github/smoke", "push", sig, "d-1", push); code != 202 {
		t.Fatal(code)
	}
	code, body, _ := send(t, srv.URL+"/api/v1/hooks/github/smoke", "push", sig, "d-1", push)
	if code != 409 || !strings.Contains(body, "received before") {
		t.Fatal(code, body)
	}
	if bs, err := st.Builds(context.Background(), "smoke", 5); err != nil || len(bs) != 1 {
		t.Fatalf("%+v %v", bs, err)
	}
}

// The signature covers the body alone, so whoever copied a delivery on
// its way could send it again under an id of their own making.
func TestTheSameSignedBodySentAgainUnderANewIdIs409(t *testing.T) {
	srv, st := server(t)
	sig := sign("topsecret", push)
	if code, _, _ := send(t, srv.URL+"/api/v1/hooks/github/smoke", "push", sig, "d-5", push); code != 202 {
		t.Fatal(code)
	}
	code, body, _ := send(t, srv.URL+"/api/v1/hooks/github/smoke", "push", sig, "d-6", push)
	if code != 409 || !strings.Contains(body, "received before") {
		t.Fatal(code, body)
	}
	if bs, err := st.Builds(context.Background(), "smoke", 5); err != nil || len(bs) != 1 {
		t.Fatalf("%+v %v", bs, err)
	}
}

// GitHub sends one event to every hook of a repo under the same delivery id,
// so the id is one job's to have seen, not every job's.
func TestOneDeliveryIdReachesEveryJobWhoseHookItWasSentTo(t *testing.T) {
	srv, st := server(t)
	branch := strings.Replace(push, "refs/heads/main", "refs/heads/APP-1-x", 1)
	sig := sign("topsecret", branch)
	if code, body, _ := send(t, srv.URL+"/api/v1/hooks/github/smoke", "push", sig, "d-7", branch); code != 200 || !strings.Contains(body, "not watched") {
		t.Fatal(code, body)
	}
	if code, body, _ := send(t, srv.URL+"/api/v1/hooks/github/branches", "push", sig, "d-7", branch); code != 202 {
		t.Fatal(code, body)
	}
	if code, _, _ := send(t, srv.URL+"/api/v1/hooks/github/branches", "push", sig, "d-7", branch); code != 409 {
		t.Fatal(code)
	}
	if bs, err := st.Builds(context.Background(), "branches", 5); err != nil || len(bs) != 1 {
		t.Fatalf("%+v %v", bs, err)
	}
}

func TestASignedDeliveryWithoutAnIdIs400AndBuildsNothing(t *testing.T) {
	srv, st := server(t)
	code, body, _ := send(t, srv.URL+"/api/v1/hooks/github/smoke", "push", sign("topsecret", push), "", push)
	if code != 400 || !strings.Contains(body, "X-GitHub-Delivery") {
		t.Fatal(code, body)
	}
	if bs, err := st.Builds(context.Background(), "smoke", 5); err != nil || len(bs) != 0 {
		t.Fatalf("%+v %v", bs, err)
	}
}

// Only a delivery that proved it holds the secret is kept: anybody could
// otherwise send the id of one GitHub is about to send, and have it refused.
func TestAnIdIsKeptOnlyOnceTheSignatureIsVerified(t *testing.T) {
	srv, _ := server(t)
	if code, _, _ := send(t, srv.URL+"/api/v1/hooks/github/smoke", "push", sign("nope", push), "d-2", push); code != 401 {
		t.Fatal(code)
	}
	if code, _, _ := send(t, srv.URL+"/api/v1/hooks/github/smoke", "push", sign("topsecret", push), "", push); code != 400 {
		t.Fatal(code)
	}
	if code, _, _ := send(t, srv.URL+"/api/v1/hooks/github/smoke", "push", sign("topsecret", push), "d-2", push); code != 202 {
		t.Fatal(code)
	}
}

func TestAnUnsignedCallerIsRefusedTheSameWithAnIdSeenBefore(t *testing.T) {
	srv, _ := server(t)
	if code, _, _ := send(t, srv.URL+"/api/v1/hooks/github/smoke", "push", sign("topsecret", push), "d-3", push); code != 202 {
		t.Fatal(code)
	}
	if code, _, _ := send(t, srv.URL+"/api/v1/hooks/github/smoke", "push", sign("nope", push), "d-3", push); code != 401 {
		t.Fatal(code)
	}
}

func TestAFailingDeliveryRecordIs500AndBuildsNothing(t *testing.T) {
	srv, st := serverOpts(t, Options{
		Secret: func(context.Context, string) (string, error) { return "topsecret", nil },
		Record: func(context.Context, string, string) error { return errors.New("store down: sekrit-detail") },
	})
	code, body, _ := send(t, srv.URL+"/api/v1/hooks/github/smoke", "push", sign("topsecret", push), "d-4", push)
	if code != 500 || strings.Contains(body, "sekrit-detail") {
		t.Fatal(code, body)
	}
	if bs, err := st.Builds(context.Background(), "smoke", 5); err != nil || len(bs) != 0 {
		t.Fatalf("%+v %v", bs, err)
	}
}

// The default branch a push names is kept with its build, for a stage's
// when.default_branch; a push that names none keeps none.
func TestAPushKeepsTheRepoDefaultBranchItNames(t *testing.T) {
	srv, st := server(t)
	named := strings.Replace(push, `"full_name":"example/app"`, `"full_name":"example/app","default_branch":"main"`, 1)
	for _, body := range []string{named, strings.Replace(push, sha, strings.Repeat("1", 40), 1)} {
		if code, out := post(t, srv.URL+"/api/v1/hooks/github/smoke", "push", sign("topsecret", body), body); code != 202 {
			t.Fatalf("%d %s", code, out)
		}
	}
	bs, err := st.Builds(context.Background(), "smoke", 10)
	must(t, err)
	got := map[string]string{}
	for _, b := range bs {
		got[b.TriggerDetail] = b.TriggerDefaultBranch
	}
	if got[sha] != "main" || got[strings.Repeat("1", 40)] != "" {
		t.Fatalf("%v", got)
	}
}

func TestAQuietServerAnswers503AndKeepsNoRecordSoTheDeliverySentAgainBuilds(t *testing.T) {
	srv, st, d := serverDispatch(t, Options{Secret: func(context.Context, string) (string, error) { return "topsecret", nil }})
	ctx := context.Background()
	_, err := d.SetQuiet(ctx, true)
	must(t, err)
	code, body, hdr := send(t, srv.URL+"/api/v1/hooks/github/smoke", "push", sign("topsecret", push), "d-q", push)
	if code != 503 || !strings.Contains(body, "kipp is quiet") || hdr.Get("Retry-After") != "60" {
		t.Fatal(code, body, hdr)
	}
	if bs, err := st.Builds(ctx, "smoke", 5); err != nil || len(bs) != 0 {
		t.Fatalf("%+v %v", bs, err)
	}
	// Unsigned, it learns nothing of the quiet.
	if code, _, _ := send(t, srv.URL+"/api/v1/hooks/github/smoke", "push", sign("nope", push), "d-q2", push); code != 401 {
		t.Fatal(code)
	}
	_, err = d.SetQuiet(ctx, false)
	must(t, err)
	if code, body, _ := send(t, srv.URL+"/api/v1/hooks/github/smoke", "push", sign("topsecret", push), "d-q", push); code != 202 {
		t.Fatalf("the same delivery sent again: %d %s", code, body)
	}
}

func TestTheHookRoutesAreUnderAPIV1(t *testing.T) {
	var got []string
	for _, r := range New(nil, Options{}).Routes() {
		got = append(got, r.Pattern())
	}
	want := []string{
		"POST /api/v1/hooks/github/{job}",
		"POST /api/v1/hooks/github-app/{app}",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("routes\n%s", strings.Join(got, "\n"))
	}
	if JobURL("https://ci.example/", "a") != "https://ci.example/api/v1/hooks/github/a" ||
		AppURL("https://ci.example", "ci") != "https://ci.example/api/v1/hooks/github-app/ci" {
		t.Fatal("a hook URL")
	}
}
