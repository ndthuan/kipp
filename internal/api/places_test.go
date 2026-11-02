package api

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/agenthub"
	"github.com/ndthuan/kipp/internal/cachesrv"
	"github.com/ndthuan/kipp/internal/enginepool"
	"github.com/ndthuan/kipp/internal/store"
)

func TestTheEnginesAndAgentsAreReadWithWhatTheServerSeesOfThem(t *testing.T) {
	since := time.Date(2026, 10, 24, 14, 2, 0, 0, time.UTC)
	e := newEnv(t, func(a *API) {
		a.deps.Engines = func() []enginepool.EngineStatus {
			return []enginepool.EngineStatus{{Name: "mac", Kind: "docker", URL: "ssh://me@host", Labels: []string{"linux"}, Max: 2, Enabled: true, Since: since, LastError: "dial tcp: refused", Running: []string{"mac/1"}}}
		}
		a.deps.Agents = func() []agenthub.Status {
			return []agenthub.Status{{Name: "box", Online: true, Since: since, Version: "test", OS: "linux", Arch: "amd64", Stages: []string{"smoke#3/s"}}}
		}
	})
	ctx := context.Background()
	must(t, e.st.PutEngine(ctx, store.Engine{Name: "mac", Kind: "docker", URL: "ssh://me@host", Labels: []string{"linux"}, Max: 2, Enabled: true}))
	must(t, e.st.PutEngine(ctx, store.Engine{Name: "new", Kind: "docker", URL: "unix:///d.sock", Labels: []string{"linux"}, Max: 1}))
	must(t, e.st.PutAgent(ctx, store.Agent{Name: "box", Labels: []string{"linux"}, Executors: 2, Enabled: true, TokenHash: "h1"}))
	must(t, e.st.PutAgent(ctx, store.Agent{Name: "idle", Labels: []string{"arm64"}, Executors: 1, TokenHash: "h2"}))
	var engines []map[string]any
	text := e.decode(t, "/api/v1/admin/engines", &engines)
	utc(t, text)
	if len(engines) != 2 || engines[1]["name"] != "new" || engines[1]["since"] != nil || engines[0]["url"] != "ssh://me@host" || engines[0]["reachable"] != false || engines[0]["last_error"] != "dial tcp: refused" ||
		engines[0]["since"] != "2026-10-24T14:02:00.000Z" || engines[0]["executors"] != 2.0 {
		t.Fatalf("%s", text)
	}
	var agents []map[string]any
	text = e.decode(t, "/api/v1/admin/agents", &agents)
	if len(agents) != 2 || agents[0]["name"] != "box" || agents[0]["online"] != true || agents[0]["version"] != "test" || agents[0]["os"] != "linux" ||
		agents[1]["online"] != false || agents[1]["enabled"] != false || agents[1]["since"] != nil {
		t.Fatalf("%s", text)
	}
	if strings.Contains(text, "h1") || strings.Contains(text, "token") {
		t.Fatalf("an agent's token hash is in %s", text)
	}
}

func TestAnEngineOrAnAgentIsChangedOnlyByAnAdminInSudoMode(t *testing.T) {
	var changed atomic.Int32
	e := newEnv(t, func(a *API) { a.deps.PlacesChanged = func(context.Context) { changed.Add(1) } })
	ctx := context.Background()
	user := e.person(t, "user", false)
	admin := e.person(t, "boss", true)
	engine := store.Engine{Name: "mac", URL: "unix:///var/run/docker.sock", Labels: []string{"linux"}, Max: 1}
	is(t, "a user", e.api.svc.AddEngine(ctx, user.u, user.sess, engine), ErrForbidden)
	is(t, "an admin not in sudo mode", e.api.svc.AddEngine(ctx, admin.u, admin.sess, engine), ErrSudoRequired)
	_, err := e.api.svc.AddAgent(ctx, admin.u, admin.sess, "box", []string{"linux"}, 1)
	is(t, "an agent from an admin not in sudo mode", err, ErrSudoRequired)
	e.sudo(t, admin)
	must(t, e.api.svc.AddEngine(ctx, admin.u, admin.sess, engine))
	is(t, "the same engine again", e.api.svc.AddEngine(ctx, admin.u, admin.sess, engine), ErrConflict)
	is(t, "an engine kipp would not dial", e.api.svc.AddEngine(ctx, admin.u, admin.sess, store.Engine{Name: "tcp", URL: "tcp://host:2376", Labels: []string{"linux"}, Max: 1}), ErrBadInput)
	must(t, e.api.svc.SetEngineEnabled(ctx, admin.u, admin.sess, "mac", false))
	plain, err := e.api.svc.AddAgent(ctx, admin.u, admin.sess, "box", []string{"linux"}, 2)
	must(t, err)
	if !strings.HasPrefix(plain, "kippa_") {
		t.Fatal(plain)
	}
	again, err := e.api.svc.NewAgentToken(ctx, admin.u, admin.sess, "box")
	must(t, err)
	if again == plain {
		t.Fatal("the same token again")
	}
	must(t, e.api.svc.SetAgentEnabled(ctx, admin.u, admin.sess, "box", false))
	must(t, e.api.svc.DeleteAgent(ctx, admin.u, admin.sess, "box"))
	must(t, e.api.svc.DeleteEngine(ctx, admin.u, admin.sess, "mac"))
	is(t, "an engine that is gone", e.api.svc.DeleteEngine(ctx, admin.u, admin.sess, "mac"), ErrNotFound)
	var got []string
	for _, a := range e.audit(t) {
		if strings.HasPrefix(a.Action, "engine.") || strings.HasPrefix(a.Action, "agent.") {
			got = append(got, a.Actor+" "+a.Action+" "+a.Target+": "+a.Detail)
		}
	}
	want := []string{
		"boss engine.add mac: unix:///var/run/docker.sock, labels linux, max 1, enabled",
		"boss engine.set mac: unix:///var/run/docker.sock, labels linux, max 1, disabled",
		"boss agent.add box: labels linux, executors 2, enabled",
		"boss agent.token box: the old token no longer works",
		"boss agent.set box: labels linux, executors 2, disabled",
		"boss agent.rm box: ",
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

func TestTheCacheIsReadAndClearedOnlyByAnAdminInSudoMode(t *testing.T) {
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
	is(t, "an admin not in sudo mode", e.api.svc.RemoveCacheEntry(ctx, admin.u, admin.sess, 1), ErrSudoRequired)
	var list []map[string]any
	text := e.decode(t, "/api/v1/admin/cache", &list)
	if len(list) != 3 || list[0]["scope"] != "site" || list[0]["key"] != "a" || list[0]["size"] != 5.0 || list[0]["last_restored_at"] != nil {
		t.Fatalf("%s", text)
	}
	e.sudo(t, admin)
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
