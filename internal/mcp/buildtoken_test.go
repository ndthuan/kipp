package mcp

import (
	"errors"
	"testing"

	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/store"
)

func TestABuildTokenIsServedWithinItsGrantAndNeverStartsItsOwnJob(t *testing.T) {
	e := newEnv(t, nil)
	bt := auth.NewBuildTokens()
	e.au.UseBuildTokens(bt)
	tok := bt.Mint(900, "smoke", 5, "", map[string]store.Perm{"smoke": {View: true, Build: true}, "other": {View: true, Build: true}})
	s := e.session(t, tok)

	var jobs listJobsOut
	ok(t, s, "list_jobs", nil, &jobs)
	names := map[string]bool{}
	for _, j := range jobs.Jobs {
		names[j.Name] = true
	}
	if len(names) != 2 || !names["smoke"] || !names["other"] {
		t.Fatalf("the token lists %v", names)
	}
	refused(t, s, "get_job", map[string]any{"job": "slow"}, "not found")
	refused(t, s, "run_build", map[string]any{"job": "smoke"}, "a build cannot start its own job")
	var started buildOut
	ok(t, s, "run_build", map[string]any{"job": "other"}, &started)
	if started.StartedBy != "build smoke#5" {
		t.Fatalf("a build started by a build is recorded under %q", started.StartedBy)
	}
	bt.Revoke(900)
	if _, err := e.au.UserFromToken(t.Context(), tok); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a revoked token resolves: %v", err)
	}
}
