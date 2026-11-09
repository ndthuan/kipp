package dispatch

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/cachesrv"
	"github.com/ndthuan/kipp/internal/runnerhub"
	"github.com/ndthuan/kipp/internal/store"
)

// withCache gives the dispatcher a cache served at an address of its own,
// the way a server does for the stages on it.
func withCache(t *testing.T) (func(*Options), *httptest.Server) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "cache.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := cachesrv.New(cachesrv.Options{Dir: filepath.Join(t.TempDir(), "cache"), Store: st})
	srv := httptest.NewServer(svc.Handler())
	t.Cleanup(srv.Close)
	return func(o *Options) { o.Cache, o.CacheURL = svc, srv.URL }, srv
}

func lookup(t *testing.T, srv *httptest.Server, token, scope string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/cache/"+scope+"/lookup?key=k&restore=p", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

const cacheJob = "name: a\nstages:\n  s:\n    cache_scopes: [site-smoke, acme]\n    run: 'echo \"[$KIPP_CACHE_URL][$KIPP_CACHE_TOKEN][$KIPP_CACHE_SCOPE]\"'\n  t: {needs: [s], run: 'echo \"[$KIPP_CACHE_URL][$KIPP_CACHE_TOKEN]\"'}\n"

func TestAStageWithCacheScopesIsGivenTheCacheAndATokenThatDiesWithTheBuild(t *testing.T) {
	opt, srv := withCache(t)
	h := newHarness(t, map[string]string{"a": cacheJob}, opt)
	b := h.wait(t, h.trigger(t, "a").ID)
	log := h.log(t, b, "s")
	if b.Status != store.Success {
		t.Fatalf("%s: %s", b.Status, log)
	}
	rest, ok := strings.CutPrefix(log, "["+srv.URL+"][")
	token, scope, _ := strings.Cut(rest, "][")
	if !ok || !strings.HasPrefix(token, "kippc_") || strings.TrimSpace(scope) != "site-smoke]" {
		t.Fatalf("the stage was given %q", log)
	}
	if strings.Contains(h.log(t, b, "t"), "kippc_") {
		t.Fatalf("a stage with no cache_scopes was given a token: %q", h.log(t, b, "t"))
	}
	if got := lookup(t, srv, token, "site-smoke"); got != http.StatusUnauthorized {
		t.Fatalf("the token of an ended build answered %d, want 401", got)
	}
}

func TestAStageWithCacheScopesOnAServerWithNoCacheFailsSayingSo(t *testing.T) {
	h := newHarness(t, map[string]string{"a": cacheJob})
	b := h.wait(t, h.trigger(t, "a").ID)
	if b.Status != store.Failed || !strings.Contains(h.log(t, b, "s"), "keeps no cache") {
		t.Fatalf("%s: %q", b.Status, h.log(t, b, "s"))
	}
}

func TestARemoteCacheStageWithoutAnExplicitRunnerURLFailsSayingSo(t *testing.T) {
	ag := newRunners(t, runnerhub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	ag.runBox(t.TempDir())
	opt, _ := withCache(t)
	job := "name: a\nstages:\n  s: {runs_on: linux, cache_scopes: [site-smoke], run: 'echo ran'}\n"
	h := newHarness(t, map[string]string{"a": job}, ag.opts, opt)
	b := h.wait(t, h.trigger(t, "a").ID)
	if log := h.log(t, b, "s"); b.Status != store.Failed || !strings.Contains(log, "--runner-url") {
		t.Fatalf("%s: %q", b.Status, log)
	}
}

func TestAnRunnerReachesTheCacheThroughRunnerURL(t *testing.T) {
	ag := newRunners(t, runnerhub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	ag.runBox(t.TempDir())
	opt, srv := withCache(t)
	job := "name: a\nstages:\n  s: {runs_on: linux, cache_scopes: [site-smoke], run: 'echo \"[$KIPP_CACHE_URL][$KIPP_CACHE_SCOPE]\"'}\n"
	h := newHarness(t, map[string]string{"a": job}, ag.opts, opt, func(o *Options) { o.CacheURL, o.RunnerURL = "http://127.0.0.1:1", srv.URL })
	b := h.wait(t, h.trigger(t, "a").ID)
	if log := h.log(t, b, "s"); b.Status != store.Success || !strings.Contains(log, "["+srv.URL+"][site-smoke]") {
		t.Fatalf("%s: %q", b.Status, log)
	}
}

// declaredJob keeps out/x between builds: the first build makes it, a later
// one finds what the first saved. The command says nothing of the cache.
func declaredJob(extra string) string {
	return "name: a\nstages:\n  s:\n    cache: [{key: k1, paths: [out], scope: site-smoke}]\n" + extra +
		"    run: |\n      if [ -f out/x ]; then echo \"found:$(cat out/x)\"; else mkdir -p out; echo first > out/x; echo made; fi\n"
}

func TestADeclaredCacheIsRestoredBeforeAndSavedAfterASuccess(t *testing.T) {
	opt, _ := withCache(t)
	h := newHarness(t, map[string]string{"a": declaredJob("")}, opt)
	first := h.wait(t, h.trigger(t, "a").ID)
	log := h.log(t, first, "s")
	if first.Status != store.Success || !strings.Contains(log, "miss: no entry for key k1") || !strings.Contains(log, "made") || !strings.Contains(log, "saved with key k1") {
		t.Fatalf("%s: %s", first.Status, log)
	}
	if strings.Index(log, "miss: no entry") > strings.Index(log, "made") || strings.Index(log, "made") > strings.Index(log, "saved with key") {
		t.Fatalf("the cache was not restored before the command and saved after it: %s", log)
	}
	second := h.wait(t, h.trigger(t, "a").ID)
	log = h.log(t, second, "s")
	if second.Status != store.Success || !strings.Contains(log, "restored from key k1") || !strings.Contains(log, "found:first") {
		t.Fatalf("%s: %s", second.Status, log)
	}
}

func TestAnExactHitIsNotSavedAgain(t *testing.T) {
	opt, _ := withCache(t)
	h := newHarness(t, map[string]string{"a": declaredJob("")}, opt)
	h.wait(t, h.trigger(t, "a").ID)
	second := h.wait(t, h.trigger(t, "a").ID)
	log := h.log(t, second, "s")
	if strings.Contains(log, "saved with key") || !strings.Contains(log, "exact hit; not saved again") {
		t.Fatalf("an exact hit was saved again, or not said to be left: %s", log)
	}
}

func TestAFailedStageSavesNoCache(t *testing.T) {
	opt, _ := withCache(t)
	job := "name: a\nstages:\n  s:\n    cache: [{key: k1, paths: [out], scope: site-smoke}]\n    run: 'mkdir -p out; echo bad > out/x; exit 1'\n"
	h := newHarness(t, map[string]string{"a": job}, opt)
	first := h.wait(t, h.trigger(t, "a").ID)
	if log := h.log(t, first, "s"); first.Status != store.Failed || strings.Contains(log, "saved with key") {
		t.Fatalf("%s: a failed stage saved a cache: %s", first.Status, log)
	}
	second := h.wait(t, h.trigger(t, "a").ID)
	if log := h.log(t, second, "s"); !strings.Contains(log, "miss: no entry for key k1") {
		t.Fatalf("the next build found what a failed stage left: %s", log)
	}
}

func TestAnUnstableStageSavesItsCache(t *testing.T) {
	opt, _ := withCache(t)
	job := "name: a\nstages:\n  s:\n    unstable_exit: [3]\n    cache: [{key: k1, paths: [out], scope: site-smoke}]\n    run: 'mkdir -p out; echo ok > out/x; exit 3'\n"
	h := newHarness(t, map[string]string{"a": job}, opt)
	first := h.wait(t, h.trigger(t, "a").ID)
	if log := h.log(t, first, "s"); first.Status != store.Unstable || !strings.Contains(log, "saved with key k1") {
		t.Fatalf("%s: %s", first.Status, log)
	}
}

func TestADeclaredCacheReachesItsScopeWithoutCacheScopes(t *testing.T) {
	opt, _ := withCache(t)
	job := "name: a\nstages:\n  s:\n    cache: [{key: k1, paths: [out]}]\n    run: 'mkdir -p out; echo ok > out/x; echo \"[$KIPP_CACHE_SCOPE]\"'\n"
	h := newHarness(t, map[string]string{"a": job}, opt)
	b := h.wait(t, h.trigger(t, "a").ID)
	if log := h.log(t, b, "s"); b.Status != store.Success || !strings.Contains(log, "[a]") || !strings.Contains(log, "saved with key k1") {
		t.Fatalf("a cache with no scope is the job's: %s: %s", b.Status, log)
	}
}

func TestADeclaredKeyWithAVariableOfTheStageIsExpanded(t *testing.T) {
	opt, _ := withCache(t)
	job := "name: a\nparams:\n  flavor: {type: string, default: plain}\nstages:\n  s:\n    cache: [{key: 'k-${flavor}-${KIPP_JOB}', paths: [out]}]\n    run: 'mkdir -p out; echo ok > out/x'\n"
	h := newHarness(t, map[string]string{"a": job}, opt)
	b := h.wait(t, h.trigger(t, "a").ID)
	if log := h.log(t, b, "s"); b.Status != store.Success || !strings.Contains(log, "saved with key k-plain-a") {
		t.Fatalf("%s: %s", b.Status, log)
	}
}

func TestACacheWhoseKeyIsNoKeyOnceExpandedIsSkippedAndNoFailure(t *testing.T) {
	opt, _ := withCache(t)
	job := "name: a\nparams:\n  flavor: {type: string, default: 'a/b'}\nstages:\n  s:\n    cache: [{key: 'k-${flavor}', paths: [out]}]\n    run: 'echo ran'\n"
	h := newHarness(t, map[string]string{"a": job}, opt)
	b := h.wait(t, h.trigger(t, "a").ID)
	log := h.log(t, b, "s")
	if b.Status != store.Success || !strings.Contains(log, "ran") || !strings.Contains(log, "not a key") || !strings.Contains(log, "skipped") {
		t.Fatalf("%s: %s", b.Status, log)
	}
}

func TestADeclaredCacheOnAStageWithNoCacheServerSaysSoAndFails(t *testing.T) {
	h := newHarness(t, map[string]string{"a": declaredJob("")})
	b := h.wait(t, h.trigger(t, "a").ID)
	if b.Status != store.Failed || !strings.Contains(h.log(t, b, "s"), "keeps no cache") {
		t.Fatalf("%s: %q", b.Status, h.log(t, b, "s"))
	}
}

func TestADeclaredCacheOnAnRunnerIsRestoredAndSavedByTheRunner(t *testing.T) {
	ag := newRunners(t, runnerhub.Identity{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true})
	ag.runBox(t.TempDir())
	opt, srv := withCache(t)
	job := declaredJob("    runs_on: linux\n    unstable_exit: [3]\n")
	h := newHarness(t, map[string]string{"a": job}, ag.opts, opt, func(o *Options) { o.RunnerURL = srv.URL })
	first := h.wait(t, h.trigger(t, "a").ID)
	if log := h.log(t, first, "s"); first.Status != store.Success || !strings.Contains(log, "saved with key k1") {
		t.Fatalf("%s: %s", first.Status, log)
	}
	second := h.wait(t, h.trigger(t, "a").ID)
	if log := h.log(t, second, "s"); !strings.Contains(log, "restored from key k1") || !strings.Contains(log, "found:first") || strings.Contains(log, "saved with key") {
		t.Fatalf("%s: %s", second.Status, log)
	}
}
