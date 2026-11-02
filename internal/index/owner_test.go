package index

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/store"
)

const ownerJob = `name: acme
secrets: [DEPLOY_KEY]
owner:
  login: acme
  token: GH_TOKEN
  exclude: ["*-archive"]
  repos:
    api:
      secrets: [API_KEY]
      env: {REGION: eu}
`

// ownerFixture is an index and a scanner over one fake account.
type ownerFixture struct {
	*fixture
	acct  *fakeOwner
	s     *Scanner
	logs  *syncBuffer
	mu    sync.Mutex
	plan  HookPlan
	queue []string
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newOwnerFixture(t *testing.T, files map[string]string, acct *fakeOwner) *ownerFixture {
	t.Helper()
	f := &ownerFixture{fixture: newFixture(t, files), acct: acct, logs: &syncBuffer{},
		plan: HookPlan{URL: "https://ci.example.org/api/v1/hooks/github/acme", Secret: "s1"}}
	srv := httptest.NewServer(http.HandlerFunc(acct.serve))
	t.Cleanup(srv.Close)
	f.s = NewScanner(context.Background(), f.x, ScannerOptions{
		API:   srv.URL,
		Token: func(context.Context, string) (string, error) { return "owner-token", nil },
		Trigger: func(_ context.Context, req dispatch.TriggerRequest) (*store.Build, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.queue = append(f.queue, req.Job+":"+req.Kind)
			return &store.Build{Job: req.Job}, nil
		},
		Hook: func(context.Context, string) HookPlan { f.mu.Lock(); defer f.mu.Unlock(); return f.plan },
		Log:  slog.New(slog.NewTextHandler(f.logs, nil)),
	})
	f.s.now = func() time.Time { return f.clock }
	t.Cleanup(f.s.Wait)
	return f
}

func (f *ownerFixture) scanNow() {
	f.t.Helper()
	must(f.t, f.s.Scan(ctx, "acme"))
	f.s.Wait()
}

func (f *ownerFixture) queued() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := slices.Clone(f.queue)
	slices.Sort(out)
	return out
}

func (f *ownerFixture) states() map[string]string {
	out := map[string]string{}
	for _, r := range f.x.Repos("acme") {
		out[r.Repo] = r.State
	}
	return out
}

func account() *fakeOwner {
	archived := repo("old")
	archived.archived = true
	fork := repo("forked")
	fork.fork = true
	bare := repo("bare")
	bare.file = false
	return newFakeOwner("acme", "Organization", repo("site"), repo("api"), archived, fork, bare, repo("docs-archive"), repo("Web.App"))
}

func TestDiscoveryKeepsOnlyRepoThatQualify(t *testing.T) {
	f := newOwnerFixture(t, map[string]string{"acme": ownerJob}, account())
	d, err := f.x.DiscoverRepos(ctx, "acme", f.acct.client(t))
	must(t, err)
	want := map[string]string{"acme/site": "live", "acme/api": "live", "acme/Web.App": "live"}
	if got := f.states(); len(got) != len(want) || got["acme/site"] != "live" || got["acme/api"] != "live" || got["acme/Web.App"] != "live" {
		t.Fatalf("repos = %v: archived, forks, excluded and file-less repos stay out", got)
	}
	if d.Added != 3 || d.Gone != 0 || d.PublicOnly || len(d.Nodes) != 3 {
		t.Fatalf("discovery = %+v", d)
	}
	if f.acct.called("GET /orgs/acme/repos") != 4 {
		t.Fatalf("an organization is listed through /orgs, in pages: %v", f.acct.calls)
	}
	// A key holds no dot, and a name that was changed gets a hash.
	var keys []string
	for _, r := range f.x.Repos("acme") {
		keys = append(keys, r.Key)
	}
	if len(keys) != 3 || keys[0] != "api" || keys[1] != "site" || !strings.HasPrefix(keys[2], "web-app-") {
		t.Fatalf("keys = %v", keys)
	}
	if _, ok := f.jobSet().Jobs["acme.api"]; !ok {
		t.Fatal("the snapshot does not hold the repo's node")
	}
}

func TestDiscoveryReadsAUserOwnerAndSaysWhenOnlyPublicReposShow(t *testing.T) {
	acct := newFakeOwner("dev", "User", repo("tool"))
	acct.me = "someone-else"
	priv := repo("secret")
	priv.private = true
	acct.repos = append(acct.repos, priv)
	acct.repos[1].owner = "dev"
	f := newOwnerFixture(t, map[string]string{"acme": strings.Replace(ownerJob, "login: acme", "login: dev", 1)}, acct)
	d, err := f.x.DiscoverRepos(ctx, "acme", acct.client(t))
	must(t, err)
	if !d.PublicOnly || len(d.Nodes) != 1 || acct.called("GET /users/dev/repos") == 0 {
		t.Fatalf("discovery %+v calls %v", d, acct.calls)
	}
	acct.me = "dev"
	d, err = f.x.DiscoverRepos(ctx, "acme", acct.client(t))
	must(t, err)
	if d.PublicOnly || acct.called("GET /user/repos") == 0 {
		t.Fatalf("the token's own account is read through /user/repos: %+v %v", d, acct.calls)
	}
}

func TestARepoThatLeavesTheSetIsGoneThenSweptWithItsChildren(t *testing.T) {
	f := newOwnerFixture(t, map[string]string{"acme": ownerJob}, account())
	f.scanNow()
	if len(f.x.Children("acme.site")) == 0 {
		t.Fatal("no child of the node")
	}
	f.acct.find("site").archived = true
	f.clock = f.clock.Add(time.Hour)
	f.scanNow()
	if f.states()["acme/site"] != "gone" {
		t.Fatalf("states = %v", f.states())
	}
	if f.jobSet().Jobs["acme.site"] == nil || !f.jobSet().Jobs["acme.site"].Disabled || !f.jobSet().Jobs["acme.site.main"].Disabled {
		t.Fatal("a gone repo's node and children stay, disabled, for orphans.days")
	}
	f.clock = f.clock.Add(8 * 24 * time.Hour)
	must(t, f.x.Sweep(ctx))
	if _, ok := f.states()["acme/site"]; ok || f.jobSet().Jobs["acme.site"] != nil {
		t.Fatal("not swept after orphans.days")
	}
	// It comes back as the same node.
	f.acct.find("site").archived = false
	f.scanNow()
	if f.states()["acme/site"] != "live" {
		t.Fatalf("states = %v", f.states())
	}
}

func TestAFileLookupThatFailsKeepsTheStateAndTheOthersAreApplied(t *testing.T) {
	f := newOwnerFixture(t, map[string]string{"acme": ownerJob}, account())
	f.scanNow()
	f.acct.fail["/repos/acme/api/contents"] = http.StatusBadGateway
	f.acct.find("site").archived = true
	_ = f.s.Scan(ctx, "acme") // it reports the repo it could not look at
	f.s.Wait()

	st := f.states()
	if st["acme/api"] != "live" || st["acme/site"] != "gone" {
		t.Fatalf("states = %v: a repo whose file cannot be looked for stays as it was", st)
	}
	if f.s.Status("acme").Err == "" {
		t.Fatal("the scan does not say it failed on one repo")
	}
}

func TestANodeCarriesTheOwnersOverridesAndGrantsFollowTheOwnerJob(t *testing.T) {
	f := newOwnerFixture(t, map[string]string{"acme": ownerJob}, account())
	f.scanNow()
	api, site := f.jobSet().Jobs["acme.api"], f.jobSet().Jobs["acme.site"]
	if !slices.Equal(api.Secrets, []string{"API_KEY"}) || api.Env["REGION"] != "eu" || !slices.Equal(site.Secrets, []string{"DEPLOY_KEY"}) {
		t.Fatalf("secrets api=%v site=%v env=%v", api.Secrets, site.Secrets, api.Env)
	}
	if api.Folder != "acme" || f.jobSet().Jobs["acme.api.main"].Folder != "acme" {
		t.Fatal("a node and its children belong to the owner job's folder")
	}
	if got := f.queued(); !slices.Equal(got, []string{"acme.api.main:scan", "acme.site.main:scan", "acme.web-app-" + keySuffix(f) + ".main:scan"}) {
		t.Fatalf("queued = %v", got)
	}
	if got, ok := f.x.RepoNode("acme", "ACME/site"); !ok || got != "acme.site" {
		t.Fatalf("RepoNode = %q %v", got, ok)
	}
	if _, ok := f.x.RepoNode("acme", "acme/nothing"); ok {
		t.Fatal("an unknown repo is routed")
	}
	if folder, err := f.st.FolderOf(ctx, "acme.site.main"); err != nil || folder != "acme" {
		t.Fatalf("FolderOf = %q %v", folder, err)
	}
}

func keySuffix(f *ownerFixture) string {
	for _, r := range f.x.Repos("acme") {
		if suffix, ok := strings.CutPrefix(r.Key, "web-app-"); ok {
			return suffix
		}
	}
	return ""
}

func TestAPushToARepoOfTheOwnerBuildsItsBranch(t *testing.T) {
	f := newOwnerFixture(t, map[string]string{"acme": ownerJob}, account())
	f.scanNow()
	node, ok := f.x.RepoNode("acme", "acme/site")
	if !ok {
		t.Fatal("no node")
	}
	plan, err := f.x.Push(ctx, node, PushEvent{Repo: "acme/site", Ref: "refs/heads/main", After: head2, DefaultBranch: "main"})
	must(t, err)
	if plan == nil || len(plan.Builds) != 1 || plan.Builds[0].Job != "acme.site.main" {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestHooksAreCreatedRepairedAndNeverDeleted(t *testing.T) {
	f := newOwnerFixture(t, map[string]string{"acme": ownerJob}, account())
	f.scanNow()
	site := f.acct.find("site")
	if len(site.hooks) != 1 || site.hooks[0].URL != f.plan.URL || site.hooks[0].Secret != "s1" || !slices.Equal(site.hooks[0].Events, []string{"pull_request", "push"}) {
		t.Fatalf("hooks = %+v", site.hooks)
	}
	// Left alone: nothing is written twice.
	posts := f.acct.called("POST ")
	f.scanNow()
	if f.acct.called("POST ") != posts || f.acct.called("PATCH ") != 0 {
		t.Fatalf("a correct hook was written again: %v", f.acct.calls)
	}
	// A rotated secret is written to the hook GitHub cannot show.
	f.mu.Lock()
	f.plan.Secret = "s2"
	f.mu.Unlock()
	f.scanNow()
	if site.hooks[0].Secret != "s2" || f.acct.called("PATCH ") != 3 {
		t.Fatalf("secret %q, calls %v", site.hooks[0].Secret, f.acct.calls)
	}
	// Someone turned it off, or changed its events: repaired in place.
	site.hooks[0].Active, site.hooks[0].Events = false, []string{"push"}
	f.scanNow()
	if !site.hooks[0].Active || len(site.hooks[0].Events) != 2 || len(site.hooks) != 1 {
		t.Fatalf("hooks = %+v", site.hooks)
	}
	// A repo that leaves keeps its hook, and the page lists it.
	site.archived = true
	f.scanNow()
	if len(site.hooks) != 1 || !slices.Equal(f.x.OldHooks("acme"), []string{"acme/site"}) {
		t.Fatalf("hooks %+v old %v", site.hooks, f.x.OldHooks("acme"))
	}
	for _, c := range f.acct.calls {
		if strings.HasPrefix(c, "DELETE ") {
			t.Fatalf("kipp deleted: %s", c)
		}
	}
}

func TestAHookThatCannotBeKeptFallsBackToTheScanAndSaysItOnce(t *testing.T) {
	f := newOwnerFixture(t, map[string]string{"acme": ownerJob}, account())
	f.acct.find("api").hookStatus = http.StatusNotFound
	f.scanNow()
	f.scanNow()
	var api store.OwnerRepo
	for _, r := range f.x.Repos("acme") {
		if r.Repo == "acme/api" {
			api = r
		}
	}
	if api.Hook != store.HookOff || !strings.Contains(api.HookNote, "Webhooks, read and write") {
		t.Fatalf("row = %+v", api)
	}
	if n := strings.Count(f.logs.String(), "no webhook kept"); n != 1 {
		t.Fatalf("%d lines about it, want 1:\n%s", n, f.logs.String())
	}
	if !slices.Contains(f.queued(), "acme.api.main:scan") {
		t.Fatal("the repo is still scanned")
	}
	// The token is given the right and the hook appears.
	f.acct.find("api").hookStatus = 0
	f.scanNow()
	if len(f.acct.find("api").hooks) != 1 {
		t.Fatal("the hook was not made once the token could")
	}
}

func TestAHookPlanThatIsOffTouchesNothing(t *testing.T) {
	f := newOwnerFixture(t, map[string]string{"acme": ownerJob}, account())
	f.plan = HookPlan{Off: "no --base-url"}
	f.scanNow()
	f.scanNow()
	if f.acct.called("GET /repos/acme/site/hooks")+f.acct.called("POST ") != 0 {
		t.Fatalf("calls = %v", f.acct.calls)
	}
	if st := f.s.Owner("acme"); st == nil || st.Repos[0].Hook != store.HookOff || st.Repos[0].Note != "no --base-url" {
		t.Fatalf("status = %+v", st)
	}
	if n := strings.Count(f.logs.String(), "no --base-url"); n != 3 {
		t.Fatalf("said %d times for 3 repos:\n%s", n, f.logs.String())
	}
}

func TestOverlapsAreFoundAndSaidOnceUntilTheyReturn(t *testing.T) {
	files := map[string]string{"acme": ownerJob, "site": scanned}
	f := newOwnerFixture(t, files, account())
	f.scanNow()
	ov := f.x.Overlaps()
	if len(ov) != 1 || ov[0].Repo != "acme/site" || !slices.Equal(ov[0].Jobs, []string{"acme", "site"}) {
		t.Fatalf("overlaps = %+v", ov)
	}
	f.scanNow()
	if n := strings.Count(f.logs.String(), "two jobs index one repo"); n != 1 {
		t.Fatalf("said %d times:\n%s", n, f.logs.String())
	}
	f.load(map[string]string{"acme": ownerJob})
	f.s.warnOverlaps()
	f.load(files)
	f.s.warnOverlaps()
	if n := strings.Count(f.logs.String(), "two jobs index one repo"); n != 2 {
		t.Fatalf("not said again after it returned: %d", n)
	}
	if st := f.s.Owner("acme"); len(st.Overlaps) != 1 {
		t.Fatalf("status = %+v", st)
	}
}

func TestAnOwnerJobIsNotTriggeredAndHasNoBuildOfItsOwn(t *testing.T) {
	f := newOwnerFixture(t, map[string]string{"acme": ownerJob}, account())
	f.scanNow()
	if f.jobSet().Jobs["acme"] == nil || !f.jobSet().Jobs["acme"].IsOwner() {
		t.Fatal("owner job missing from the job set")
	}
	if got := f.s.Owner("nobody"); got != nil {
		t.Fatalf("Owner(nobody) = %+v", got)
	}
	if o := f.x.Owner("acme"); o == nil || o.Login != "acme" || !slices.Equal(f.x.Owners(), []string{"acme"}) {
		t.Fatalf("owner = %+v", o)
	}
}
