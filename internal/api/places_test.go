package api

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/cachesrv"
	"github.com/ndthuan/kipp/internal/enginepool"
	"github.com/ndthuan/kipp/internal/runnerhub"
	"github.com/ndthuan/kipp/internal/store"
)

func TestTheEnginesAndRunnersAreReadWithWhatTheServerSeesOfThem(t *testing.T) {
	since := time.Date(2026, 10, 24, 14, 2, 0, 0, time.UTC)
	e := newEnv(t, func(a *API) {
		a.deps.Engines = func() []enginepool.EngineStatus {
			return []enginepool.EngineStatus{{Name: "mac", Kind: "docker", URL: "ssh://me@host", Labels: []string{"linux"}, Max: 2, Enabled: true, Since: since, LastError: "dial tcp: refused", Running: []string{"mac/1"}}}
		}
		a.deps.Runners = func() []runnerhub.Status {
			return []runnerhub.Status{{Name: "box", Online: true, Since: since, Version: "test", OS: "linux", Arch: "amd64", Stages: []string{"smoke#3/s"}}}
		}
	})
	ctx := context.Background()
	must(t, e.st.PutEngine(ctx, store.Engine{Name: "mac", Kind: "docker", URL: "ssh://me@host", Labels: []string{"linux"}, Max: 2, Enabled: true}))
	must(t, e.st.PutEngine(ctx, store.Engine{Name: "new", Kind: "docker", URL: "unix:///d.sock", Labels: []string{"linux"}, Max: 1}))
	must(t, e.st.PutRunner(ctx, store.Runner{Name: "box", Labels: []string{"linux"}, Executors: 2, Weight: 10, Enabled: true, TokenHash: "h1"}))
	must(t, e.st.PutRunner(ctx, store.Runner{Name: "idle", Labels: []string{"arm64"}, Executors: 1, TokenHash: "h2"}))
	var engines []map[string]any
	text := e.decode(t, "/api/v1/admin/engines", &engines)
	utc(t, text)
	if len(engines) != 2 || engines[1]["name"] != "new" || engines[1]["since"] != nil || engines[0]["url"] != "ssh://me@host" || engines[0]["reachable"] != false || engines[0]["last_error"] != "dial tcp: refused" ||
		engines[0]["since"] != "2026-10-24T14:02:00.000Z" || engines[0]["executors"] != 2.0 {
		t.Fatalf("%s", text)
	}
	var runners []map[string]any
	text = e.decode(t, "/api/v1/admin/runners", &runners)
	if len(runners) != 2 || runners[0]["name"] != "box" || runners[0]["online"] != true || runners[0]["version"] != "test" || runners[0]["os"] != "linux" || runners[0]["weight"] != 10.0 || runners[1]["weight"] != 0.0 ||
		runners[1]["online"] != false || runners[1]["enabled"] != false || runners[1]["since"] != nil {
		t.Fatalf("%s", text)
	}
	if strings.Contains(text, "h1") || strings.Contains(text, "token") {
		t.Fatalf("a runner's token hash is in %s", text)
	}
}

// Adding an engine takes an ssh key, and adding a runner or giving it a new
// token shows a token: those keep sudo mode.
func TestAnEngineOrARunnerIsAddedOrGivenATokenOnlyByAnAdminInSudoMode(t *testing.T) {
	var changed atomic.Int32
	e := newEnv(t, func(a *API) { a.deps.PlacesChanged = func(context.Context) { changed.Add(1) } })
	ctx := context.Background()
	user := e.person(t, "user", false)
	admin := e.person(t, "boss", true)
	engine := store.Engine{Name: "mac", URL: "unix:///var/run/docker.sock", Labels: []string{"linux"}, Max: 1}
	is(t, "a user", e.api.svc.AddEngine(ctx, user.u, user.sess, engine), ErrForbidden)
	is(t, "an admin not in sudo mode", e.api.svc.AddEngine(ctx, admin.u, admin.sess, engine), ErrSudoRequired)
	_, err := e.api.svc.AddRunner(ctx, admin.u, admin.sess, "box", []string{"linux"}, 1)
	is(t, "a runner from an admin not in sudo mode", err, ErrSudoRequired)
	must(t, e.st.PutRunner(ctx, store.Runner{Name: "standing", Labels: []string{"linux"}, Executors: 1, Enabled: true, TokenHash: "h"}))
	_, err = e.api.svc.NewRunnerToken(ctx, admin.u, admin.sess, "standing")
	is(t, "a runner's new token from an admin not in sudo mode", err, ErrSudoRequired)
	must(t, e.st.DeleteRunner(ctx, "standing"))
	e.sudo(t, admin)
	must(t, e.api.svc.AddEngine(ctx, admin.u, admin.sess, engine))
	is(t, "the same engine again", e.api.svc.AddEngine(ctx, admin.u, admin.sess, engine), ErrConflict)
	is(t, "an engine kipp would not dial", e.api.svc.AddEngine(ctx, admin.u, admin.sess, store.Engine{Name: "tcp", URL: "tcp://host:2376", Labels: []string{"linux"}, Max: 1}), ErrBadInput)
	must(t, e.api.svc.SetEngineEnabled(ctx, admin.u, admin.sess, "mac", false))
	plain, err := e.api.svc.AddRunner(ctx, admin.u, admin.sess, "box", []string{"linux"}, 2)
	must(t, err)
	if !strings.HasPrefix(plain, "kippa_") {
		t.Fatal(plain)
	}
	again, err := e.api.svc.NewRunnerToken(ctx, admin.u, admin.sess, "box")
	must(t, err)
	if again == plain {
		t.Fatal("the same token again")
	}
	must(t, e.api.svc.SetRunnerEnabled(ctx, admin.u, admin.sess, "box", false))
	must(t, e.api.svc.DeleteRunner(ctx, admin.u, admin.sess, "box"))
	must(t, e.api.svc.DeleteEngine(ctx, admin.u, admin.sess, "mac"))
	is(t, "an engine that is gone", e.api.svc.DeleteEngine(ctx, admin.u, admin.sess, "mac"), ErrNotFound)
	var got []string
	for _, a := range e.audit(t) {
		if strings.HasPrefix(a.Action, "engine.") || strings.HasPrefix(a.Action, "runner.") {
			got = append(got, a.Actor+" "+a.Action+" "+a.Target+": "+a.Detail)
		}
	}
	want := []string{
		"boss engine.add mac: unix:///var/run/docker.sock, labels linux, max 1, enabled",
		"boss engine.set mac: unix:///var/run/docker.sock, labels linux, max 1, disabled",
		"boss runner.add box: labels linux, executors 2, enabled",
		"boss runner.token box: the old token no longer works",
		"boss runner.set box: labels linux, executors 2, disabled",
		"boss runner.rm box: ",
		"boss engine.rm mac: ",
	}
	eq(t, "the audit", strings.Join(got, "\n"), strings.Join(want, "\n"))
	for _, a := range got {
		if strings.Contains(a, plain) || strings.Contains(a, again) {
			t.Fatal("a token is in the audit")
		}
	}
	if changed.Load() < 6 {
		t.Fatalf("the server was told of %d changes", changed.Load())
	}
}

func TestAStageTellsWhereItRanAndAWaitingOneWhatItWaitsFor(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := &store.Build{Job: "smoke", Status: store.Success, Trigger: "api"}
	must(t, e.st.CreateBuild(ctx, b))
	must(t, e.st.PutStages(ctx, b.ID, []string{"s"}, nil))
	must(t, e.st.SetStageRanOn(ctx, b.ID, "s", "mac/1"))
	var got struct {
		Stages []struct {
			RanOn string `json:"ran_on"`
		} `json:"stages"`
	}
	text, _ := e.call(t, 200, e.admin, "GET", "/api/v1/builds/"+itoa(b.ID), "")
	must(t, json.Unmarshal([]byte(text), &got))
	if len(got.Stages) != 1 || got.Stages[0].RanOn != "mac/1" {
		t.Fatalf("%s", text)
	}
}

func TestOnlyAnAdminIsToldWhatAStageWaitsForByName(t *testing.T) {
	v := &QueueView{Running: []RunningView{{Build: &store.Build{ID: 1}, Waiting: map[string]string{"s": "waiting for engine mac (unreachable since 14:02: dial tcp 192.0.2.1:22)"}}}}
	hideCapacity(v)
	if got := v.Running[0].Waiting["s"]; got != WaitPlace {
		t.Fatal(got)
	}
}

func TestTheCacheIsReadAndClearedOnlyByAnAdminInTheirOwnSessionWithoutSudo(t *testing.T) {
	e := newEnv(t, func(a *API) {
		a.deps.Cache = cachesrv.New(cachesrv.Options{Dir: t.TempDir(), Store: a.deps.Store})
	})
	ctx := context.Background()
	for _, k := range []struct{ scope, key string }{{"site", "a"}, {"site", "b"}, {"api", "c"}} {
		_, err := e.st.PutCacheEntry(ctx, &store.CacheEntry{Scope: k.scope, Namespace: "default", Key: k.key, Size: 5, SavedAt: time.Now()})
		must(t, err)
	}
	user := e.person(t, "user", false)
	admin := e.person(t, "boss", true)
	_, err := e.api.svc.Caches(ctx, user.u)
	is(t, "a user reading", err, ErrForbidden)
	is(t, "a user removing", e.api.svc.RemoveCacheEntry(ctx, user.u, user.sess, 1), ErrForbidden)
	is(t, "an admin's token", e.api.svc.RemoveCacheEntry(ctx, admin.u, nil, 1), ErrForbidden)
	var list []map[string]any
	text := e.decode(t, "/api/v1/admin/cache", &list)
	if len(list) != 3 || list[0]["scope"] != "site" || list[0]["key"] != "a" || list[0]["size"] != 5.0 || list[0]["last_restored_at"] != nil {
		t.Fatalf("%s", text)
	}
	must(t, e.api.svc.RemoveCacheEntry(ctx, admin.u, admin.sess, 1))
	is(t, "an entry that is gone", e.api.svc.RemoveCacheEntry(ctx, admin.u, admin.sess, 1), ErrNotFound)
	n, err := e.api.svc.RemoveCacheScope(ctx, admin.u, admin.sess, "site")
	must(t, err)
	eq(t, "removed", n, 1)
	_, err = e.api.svc.RemoveCacheScope(ctx, admin.u, admin.sess, "site")
	is(t, "an empty scope", err, ErrNotFound)
	var got []string
	for _, a := range e.audit(t) {
		if a.Action == "cache.remove" {
			got = append(got, a.Actor+" "+a.Target)
		}
	}
	eq(t, "the audit", strings.Join(got, "|"), "boss site/default/a|boss site/default/b")
}

func TestAStageTellsHowManyTimesItsCommandRan(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := &store.Build{Job: "smoke", Status: store.Success, Trigger: "api"}
	must(t, e.st.CreateBuild(ctx, b))
	must(t, e.st.PutStages(ctx, b.ID, []string{"a", "b"}, nil))
	must(t, e.st.SetStageTries(ctx, b.ID, "b", 3))
	var got struct {
		Stages []struct {
			Tries int `json:"tries"`
		} `json:"stages"`
	}
	text, _ := e.call(t, 200, e.admin, "GET", "/api/v1/builds/"+itoa(b.ID), "")
	must(t, json.Unmarshal([]byte(text), &got))
	if len(got.Stages) != 2 || got.Stages[0].Tries != 1 || got.Stages[1].Tries != 3 {
		t.Fatalf("%s", text)
	}
}

// A standing runner's executors and labels, an engine's limit and labels,
// and the built-in runner's limit are changed by an admin in their own
// session without sudo mode, through the checks kipp runner set makes,
// audited, and in effect at once.
func TestAnAdminChangesEachRunnersLimitAndLabels(t *testing.T) {
	var changed atomic.Int32
	e := newEnv(t, func(a *API) { a.deps.PlacesChanged = func(context.Context) { changed.Add(1) } })
	ctx := context.Background()
	user := e.person(t, "user", false)
	admin := e.person(t, "boss", true)
	must(t, e.st.PutRunner(ctx, store.Runner{Name: "box", Labels: []string{"linux"}, Executors: 2, Enabled: true, TokenHash: "h1"}))
	must(t, e.st.PutEngine(ctx, store.Engine{Name: "mac", Kind: "docker", URL: "unix:///var/run/docker.sock", Labels: []string{"linux"}, Max: 1, Enabled: true}))
	three, none, minus := 3, 0, -1
	is(t, "a user", e.api.svc.SetRunner(ctx, user.u, user.sess, "box", RunnerEdit{Executors: &three}), ErrForbidden)
	is(t, "the built-in limit from a user", e.api.svc.SetBuiltInExecutors(ctx, user.u, user.sess, 3), ErrForbidden)
	must(t, e.api.svc.SetRunner(ctx, admin.u, admin.sess, "box", RunnerEdit{Executors: &three, Labels: []string{"linux", "arm64"}}))
	must(t, e.api.svc.SetRunner(ctx, admin.u, admin.sess, "box", RunnerEdit{Executors: &none}))
	ten := 10
	must(t, e.api.svc.SetRunner(ctx, admin.u, admin.sess, "box", RunnerEdit{Weight: &ten}))
	must(t, e.api.svc.SetRunner(ctx, admin.u, admin.sess, "box", RunnerEdit{Weight: new(int)}))
	is(t, "executors below zero", e.api.svc.SetRunner(ctx, admin.u, admin.sess, "box", RunnerEdit{Executors: &minus}), ErrBadInput)
	is(t, "no label", e.api.svc.SetRunner(ctx, admin.u, admin.sess, "box", RunnerEdit{Labels: []string{}}), ErrBadInput)
	is(t, "a runner that is not there", e.api.svc.SetRunner(ctx, admin.u, admin.sess, "gone", RunnerEdit{Executors: &three}), ErrNotFound)
	a, err := e.st.Runner(ctx, "box")
	must(t, err)
	if a.Executors != 0 || a.Weight != 0 || strings.Join(a.Labels, ",") != "linux,arm64" || !a.Enabled {
		t.Fatalf("kept %+v", a)
	}
	must(t, e.api.svc.SetEngine(ctx, admin.u, admin.sess, "mac", EngineEdit{Max: &three, Labels: []string{"linux", "amd64"}}))
	is(t, "an engine of no container", e.api.svc.SetEngine(ctx, admin.u, admin.sess, "mac", EngineEdit{Max: &none}), ErrBadInput)
	must(t, e.api.svc.SetBuiltInExecutors(ctx, admin.u, admin.sess, 6))
	is(t, "a built-in runner of no executor", e.api.svc.SetBuiltInExecutors(ctx, admin.u, admin.sess, 0), ErrBadInput)
	if n, ok, err := e.st.BuiltInExecutors(ctx); err != nil || !ok || n != 6 {
		t.Fatalf("the built-in limit kept: %d %v %v", n, ok, err)
	}
	if n := e.api.deps.Dispatch.Executors(); n != 6 {
		t.Fatalf("the built-in runner's limit is %d", n)
	}
	var got []string
	for _, a := range e.audit(t) {
		if strings.HasSuffix(a.Action, ".set") {
			got = append(got, a.Actor+" "+a.Action+" "+a.Target+": "+a.Detail)
		}
	}
	want := []string{
		"boss runner.set box: labels linux,arm64, executors 3, enabled",
		"boss runner.set box: labels linux,arm64, executors 0, enabled",
		"boss runner.set box: labels linux,arm64, executors 0, enabled, weight 10",
		"boss runner.set box: labels linux,arm64, executors 0, enabled",
		"boss engine.set mac: unix:///var/run/docker.sock, labels linux,amd64, max 3, enabled",
		"boss runner.set built-in: executors 6",
	}
	eq(t, "the audit", strings.Join(got, "\n"), strings.Join(want, "\n"))
	if changed.Load() != 5 {
		t.Fatalf("the server was told of %d changes", changed.Load())
	}
}

func placeEditCalls(e *env) map[string]func(u *store.User, sess *store.Session) error {
	ctx := context.Background()
	svc := e.api.svc
	two := 2
	return map[string]func(u *store.User, sess *store.Session) error{
		"SetEngine": func(u *store.User, sess *store.Session) error {
			return svc.SetEngine(ctx, u, sess, "mac", EngineEdit{Labels: []string{"linux", "amd64"}, Max: &two})
		},
		"DisableEngine": func(u *store.User, sess *store.Session) error {
			return svc.SetEngineEnabled(ctx, u, sess, "mac", false)
		},
		"EnableEngine": func(u *store.User, sess *store.Session) error { return svc.SetEngineEnabled(ctx, u, sess, "mac", true) },
		"SetRunner": func(u *store.User, sess *store.Session) error {
			return svc.SetRunner(ctx, u, sess, "box", RunnerEdit{Labels: []string{"linux", "arm64"}, Executors: &two, Weight: &two})
		},
		"DisableRunner": func(u *store.User, sess *store.Session) error {
			return svc.SetRunnerEnabled(ctx, u, sess, "box", false)
		},
		"EnableRunner":        func(u *store.User, sess *store.Session) error { return svc.SetRunnerEnabled(ctx, u, sess, "box", true) },
		"SetBuiltInExecutors": func(u *store.User, sess *store.Session) error { return svc.SetBuiltInExecutors(ctx, u, sess, 3) },
		"RemoveCacheEntry":    func(u *store.User, sess *store.Session) error { return svc.RemoveCacheEntry(ctx, u, sess, 1) },
		"RemoveCacheScope": func(u *store.User, sess *store.Session) error {
			_, err := svc.RemoveCacheScope(ctx, u, sess, "api")
			return err
		},
		"DeleteEngine": func(u *store.User, sess *store.Session) error { return svc.DeleteEngine(ctx, u, sess, "mac") },
		"DeleteRunner": func(u *store.User, sess *store.Session) error { return svc.DeleteRunner(ctx, u, sess, "box") },
	}
}

var placeEditOrder = []string{"SetEngine", "DisableEngine", "EnableEngine", "SetRunner", "DisableRunner", "EnableRunner",
	"SetBuiltInExecutors", "RemoveCacheEntry", "RemoveCacheScope", "DeleteEngine", "DeleteRunner"}

func placesEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, func(a *API) {
		a.deps.Cache = cachesrv.New(cachesrv.Options{Dir: t.TempDir(), Store: a.deps.Store})
	})
	ctx := context.Background()
	must(t, e.st.PutEngine(ctx, store.Engine{Name: "mac", Kind: "docker", URL: "unix:///var/run/docker.sock", Labels: []string{"linux"}, Max: 1, Enabled: true}))
	must(t, e.st.PutRunner(ctx, store.Runner{Name: "box", Labels: []string{"linux"}, Executors: 1, Enabled: true, TokenHash: "h"}))
	for _, k := range []struct{ scope, key string }{{"site", "a"}, {"api", "b"}} {
		_, err := e.st.PutCacheEntry(ctx, &store.CacheEntry{Scope: k.scope, Namespace: "default", Key: k.key, Size: 5, SavedAt: time.Now()})
		must(t, err)
	}
	return e
}

func TestAnAdminInTheirOwnSessionChangesRunnersEnginesAndTheCacheWithoutSudo(t *testing.T) {
	e := placesEnv(t)
	admin := e.person(t, "boss", true)
	if admin.sess.SudoUntil != nil {
		t.Fatal("the admin's session is in sudo mode")
	}
	calls := placeEditCalls(e)
	eq(t, "every change is run", len(placeEditOrder), len(calls))
	for _, name := range placeEditOrder {
		must(t, calls[name](admin.u, admin.sess))
	}
	ctx := context.Background()
	if _, err := e.st.Engine(ctx, "mac"); err == nil {
		t.Fatal("the engine is still there")
	}
	if _, err := e.st.Runner(ctx, "box"); err == nil {
		t.Fatal("the runner is still there")
	}
	if left, err := e.st.CacheEntries(ctx); err != nil || len(left) != 0 {
		t.Fatalf("the cache kept %+v, %v", left, err)
	}
	var actions []string
	for _, a := range e.audit(t) {
		actions = append(actions, a.Actor+" "+a.Action+" "+a.Target)
	}
	eq(t, "the audit", strings.Join(actions, "|"), strings.Join([]string{
		"boss engine.set mac", "boss engine.set mac", "boss engine.set mac",
		"boss runner.set box", "boss runner.set box", "boss runner.set box",
		"boss runner.set built-in", "boss cache.remove site/default/a", "boss cache.remove api/default/b",
		"boss engine.rm mac", "boss runner.rm box",
	}, "|"))
}

func TestARunnerEngineOrCacheChangeIsRefusedToANonAdminATokenAndAnotherUsersSession(t *testing.T) {
	e := placesEnv(t)
	admin, bob := e.person(t, "boss", true), e.person(t, "bob", false)
	e.sudo(t, bob)
	for name, call := range placeEditCalls(e) {
		is(t, name+" from a non-admin in sudo mode", call(bob.u, bob.sess), ErrForbidden)
		is(t, name+" from an admin's token", call(admin.u, nil), ErrForbidden)
		is(t, name+" from another user's session", call(admin.u, bob.sess), ErrForbidden)
	}
	eq(t, "audit rows", len(e.audit(t)), 0)
}
