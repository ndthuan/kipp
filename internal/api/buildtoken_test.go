package api

import (
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/store"
)

// buildTokens is an env whose server accepts the tokens of builds, and the
// set a test mints them in.
func buildTokens(t *testing.T) (*env, *auth.BuildTokens) {
	t.Helper()
	bt := auth.NewBuildTokens()
	return newEnv(t, func(a *API) { a.deps.Auth.UseBuildTokens(bt) }), bt
}

var viewSmoke = map[string]store.Perm{"smoke": {View: true}}

func TestABuildTokenViewsItsOwnJobAndIsRefusedBuildAndCancelAnywhere(t *testing.T) {
	e, bt := buildTokens(t)
	tok := bt.Mint(900, "smoke", 1, "", viewSmoke)
	e.call(t, 200, tok, "GET", "/api/v1/jobs/smoke", "")
	e.call(t, 200, tok, "GET", "/api/v1/jobs/smoke/builds", "")
	// A job it may not view does not exist for it, as for a user without the grant.
	e.asProblem(t, 404, "not_found", tok, "GET", "/api/v1/jobs/gc", "")
	e.asProblem(t, 403, "forbidden", tok, "POST", "/api/v1/jobs/smoke/builds", "")
	id := e.started(t, "slow")
	e.asProblem(t, 404, "not_found", tok, "POST", "/api/v1/builds/"+id+"/cancel", "")
	id = e.trigger(t, "smoke", "")
	e.finished(t, id)
	e.asProblem(t, 403, "forbidden", tok, "POST", "/api/v1/builds/"+id+"/cancel", "")
	e.asProblem(t, 403, "forbidden", tok, "POST", "/api/v1/builds/"+id+"/again", "")
	names := func(rows []map[string]any) (out []string) {
		for _, r := range rows {
			out = append(out, r["name"].(string))
		}
		return out
	}
	if got := names(e.list(t, tok, "/api/v1/jobs")); len(got) != 1 || got[0] != "smoke" {
		t.Fatalf("the token sees %v", got)
	}
}

func TestABuildTokenCannotStartABuildOfItsOwnJobButMayOfOneItHasBuildOn(t *testing.T) {
	e, bt := buildTokens(t)
	tok := bt.Mint(900, "smoke", 4, "", map[string]store.Perm{"smoke": {View: true, Build: true}, "gc": {View: true, Build: true}})
	d, _ := e.asProblem(t, 403, "forbidden", tok, "POST", "/api/v1/jobs/smoke/builds", "")
	if !strings.Contains(d.Detail, "its own job") {
		t.Fatalf("the refusal says %q", d.Detail)
	}
	id := e.trigger(t, "smoke", "")
	e.finished(t, id)
	e.asProblem(t, 403, "forbidden", tok, "POST", "/api/v1/builds/"+id+"/again", "")

	b := e.object(t, 201, tok, "POST", "/api/v1/jobs/gc/builds", "")
	eq(t, "who started it", b["started_by"].(string), "build smoke#4")
	eq(t, "how", b["trigger"].(string), "api")
}

func TestABuildTokenCannotStartTheMultibranchJobItIsAChildOf(t *testing.T) {
	e, bt := buildTokens(t)
	// smoke stands in for the parent: starting it builds a child, the token's own.
	tok := bt.Mint(900, "smoke.main", 4, "smoke", map[string]store.Perm{"smoke": {View: true, Build: true}})
	e.asProblem(t, 403, "forbidden", tok, "POST", "/api/v1/jobs/smoke/builds", "")
}

func TestABuildTokenIsRefusedOnEveryAdminRoute(t *testing.T) {
	e, bt := buildTokens(t)
	all := map[string]store.Perm{}
	for _, j := range []string{"smoke", "gc", "slow", "once", "cr"} {
		all[j] = store.Perm{View: true, Build: true, Cancel: true}
	}
	tok := bt.Mint(900, "other", 1, "", all)
	seen := 0
	for _, r := range e.api.Routes() {
		if !strings.HasPrefix(r.Path, "/admin/") {
			continue
		}
		seen++
		e.asProblem(t, 403, "forbidden", tok, r.Method, PrefixV1+pathValues.Replace(r.Path), "")
	}
	if seen == 0 {
		t.Fatal("no admin route was tried")
	}
	// What is not an admin's is a viewer's, within the grant.
	e.call(t, 200, tok, "GET", "/api/v1/overview", "")
	e.call(t, 200, tok, "GET", "/api/v1/queue", "")
}

func TestABuildTokenIsRefusedOnceTheBuildEndsAndAForgedOneNever(t *testing.T) {
	e, bt := buildTokens(t)
	tok := bt.Mint(900, "smoke", 1, "", viewSmoke)
	e.call(t, 200, tok, "GET", "/api/v1/jobs/smoke", "")
	bt.Revoke(900)
	e.asProblem(t, 401, "unauthorized", tok, "GET", "/api/v1/jobs/smoke", "")
	e.asProblem(t, 401, "unauthorized", auth.BuildTokenPrefix+"forged", "GET", "/api/v1/jobs/smoke", "")
}

func TestABuildTokenIsNoTokenOnAServerThatMakesNone(t *testing.T) {
	e := newEnv(t)
	e.asProblem(t, 401, "unauthorized", auth.BuildTokenPrefix+"anything", "GET", "/api/v1/jobs/smoke", "")
}
