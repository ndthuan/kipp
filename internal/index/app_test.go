package index

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/github"
	"github.com/ndthuan/kipp/internal/store"
)

const appOwnerJob = `name: acme
owner:
  login: acme
  app: {id: 4242, key_file: /keys/ci.pem}
`

// withApp gives the fixture's scanner a way to sign in as an App: an
// installation client of the fake account, and the calls it was asked for.
func withApp(t *testing.T, f *ownerFixture) *[]string {
	t.Helper()
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(f.acct.serve))
	t.Cleanup(srv.Close)
	f.s.opts.App = func(_ context.Context, a *config.App, login string) (*github.Client, error) {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, a.Name+"/"+login)
		return github.New(srv.URL, "installation-token"), nil
	}
	return &asked
}

func TestAnOwnerJobWithAnAppReadsTheInstallationsReposAndKeepsNoHook(t *testing.T) {
	f := newOwnerFixture(t, map[string]string{"acme": appOwnerJob}, account())
	asked := withApp(t, f)
	f.scanNow()
	if !slices.Equal(f.queued(), []string{"acme.api.main:scan", "acme.docs-archive.main:scan", "acme.site.main:scan", "acme.web-app-" + keySuffix(f) + ".main:scan"}) {
		t.Fatalf("queued %v", f.queued())
	}
	if !slices.Contains(*asked, "ci/acme") {
		t.Fatalf("asked %v", *asked)
	}
	if f.acct.called("GET /installation/repositories") == 0 {
		t.Fatalf("the installation's repos were not listed: %v", f.acct.calls)
	}
	for _, c := range f.acct.calls {
		if c == "GET /user" || strings.HasPrefix(c, "GET /users/") || strings.Contains(c, "/hooks") {
			t.Fatalf("an installation has no user and kipp keeps no hook for an App: %s", c)
		}
	}
	if o := f.s.Owner("acme"); o.App != "ci" || len(o.OldHooks) != 0 {
		t.Fatalf("owner status %+v", o)
	}
}

func TestAnAppTheServerCannotSignInAsIsAScanError(t *testing.T) {
	f := newOwnerFixture(t, map[string]string{"acme": appOwnerJob}, account())
	err := f.s.Scan(ctx, "acme")
	if err == nil || !strings.Contains(err.Error(), "GitHub App ci") {
		t.Fatalf("error %v", err)
	}
}

func TestMovingAnOwnerJobFromATokenToAnAppListsTheHooksItLeaves(t *testing.T) {
	f := newOwnerFixture(t, map[string]string{"acme": ownerJob}, account())
	f.scanNow()
	if len(f.acct.find("site").hooks) != 1 || len(f.x.OldHooks("acme")) != 0 {
		t.Fatal("the token path keeps a hook on a live repo and lists none as old")
	}
	f.load(map[string]string{"acme": appOwnerJob})
	withApp(t, f)
	posts := f.acct.called("POST ")
	f.scanNow()
	if f.acct.called("POST ") != posts || f.acct.called("PATCH ") != 0 || f.acct.called("DELETE ") != 0 {
		t.Fatalf("kipp touched a hook after the move: %v", f.acct.calls)
	}
	old := f.x.OldHooks("acme")
	slices.Sort(old)
	if !slices.Equal(old, []string{"acme/Web.App", "acme/api", "acme/site"}) {
		t.Fatalf("old hooks %v", old)
	}
	if got := f.s.Owner("acme"); got.App != "ci" || len(got.OldHooks) != 3 {
		t.Fatalf("owner status %+v", got)
	}
	// The statuses and the context stay what they were: the node is the same job.
	if n, ok := f.x.RepoNode("acme", "acme/site"); !ok || n != "acme.site" {
		t.Fatalf("node %q", n)
	}
}

func TestARescanAskedWhileAScanRunsRunsAnotherAfterIt(t *testing.T) {
	f := newOwnerFixture(t, map[string]string{"acme": appOwnerJob}, account())
	withApp(t, f)
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	t.Cleanup(release)
	var mu sync.Mutex
	calls := 0
	trigger := f.s.opts.Trigger
	f.s.opts.Trigger = func(c context.Context, req dispatch.TriggerRequest) (*store.Build, error) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			<-gate // the first scan holds here, running
		}
		return trigger(c, req)
	}
	must(t, f.s.Start("acme"))
	deadline := time.Now().Add(5 * time.Second)
	for !f.s.Status("acme").Running && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	must(t, f.s.Rescan("acme"))
	must(t, f.s.Rescan("acme")) // asked twice, run once
	scansBefore := f.acct.called("GET /installation/repositories")
	release()
	f.s.Wait()
	if got := f.acct.called("GET /installation/repositories") - scansBefore; got == 0 {
		t.Fatalf("no scan followed: %v", f.acct.calls)
	}
	if f.s.Status("acme").Running {
		t.Fatal("still running")
	}
	if err := f.s.Rescan("nope"); err == nil {
		t.Fatal("a name that is no job was rescanned")
	}
}

func TestARescanOfAnIdleJobScansAtOnce(t *testing.T) {
	f := newOwnerFixture(t, map[string]string{"acme": appOwnerJob}, account())
	withApp(t, f)
	must(t, f.s.Rescan("acme"))
	f.s.Wait()
	if f.acct.called("GET /installation/repositories") == 0 || f.s.Status("acme").At.IsZero() {
		t.Fatalf("not scanned: %v", f.acct.calls)
	}
}

func TestPostStatusForSignsInAsTheAppOfAnOwnersRepo(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	app := func(_ context.Context, a *config.App, login string) (*github.Client, error) {
		return github.New(srv.URL, "inst-"+a.Name+"-"+login), nil
	}
	token := func(context.Context, string) (string, error) {
		t.Error("a token was opened for an App")
		return "", nil
	}
	post := PostStatusFor(srv.URL, token, app)
	job := &config.Job{Multibranch: &config.Multibranch{Repo: "acme/site", App: &config.App{ID: 1, Name: "ci"}}}
	err := post(context.Background(), job, dispatch.CommitStatus{SHA: head, State: "success", Context: "kipp/acme"})
	must(t, err)
	if len(got) != 1 || got[0] != "POST /repos/acme/site/statuses/"+head+" Bearer inst-ci-acme" {
		t.Fatalf("calls %v", got)
	}
	// A server without Apps says so, and a job with neither says nothing.
	err = PostStatusFor(srv.URL, token, nil)(context.Background(), job, dispatch.CommitStatus{SHA: head, State: "success", Context: "c"})
	if err == nil || !strings.Contains(err.Error(), "GitHub App ci") {
		t.Fatalf("error %v", err)
	}
	if err := post(context.Background(), &config.Job{Multibranch: &config.Multibranch{Repo: "acme/site"}}, dispatch.CommitStatus{}); err != nil {
		t.Fatal(err)
	}
}
