package auth

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

func buildService(t *testing.T) (*Service, *store.Store, *BuildTokens) {
	t.Helper()
	svc, st := service(t)
	bt := NewBuildTokens()
	svc.UseBuildTokens(bt)
	return svc, st, bt
}

func TestABuildTokenIsTheBuildAndNeverAnAdmin(t *testing.T) {
	ctx := context.Background()
	svc, _, bt := buildService(t)
	tok := bt.Mint(7, "ci", 3, "", map[string]store.Perm{"ci": {View: true}})
	if !strings.HasPrefix(tok, BuildTokenPrefix) {
		t.Fatalf("a build token says what it is: %q", tok)
	}
	u, err := svc.UserFromToken(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	if u.Admin || u.Build == nil || u.Name != "build ci#3" || u.Build.Job != "ci" || u.ID >= 0 {
		t.Fatalf("principal %+v", u)
	}
}

func TestABuildTokenHoldsItsGrantAndNothingElse(t *testing.T) {
	ctx := context.Background()
	svc, st, bt := buildService(t)
	// A user named like the build's job, with grants of their own, is no
	// way to more than the token was made with.
	other := addUser(t, st, "ci", "h", false)
	grant(t, st, other.ID, "secret-job", []string{"view", "build", "cancel"})
	tok := bt.Mint(7, "ci", 1, "", map[string]store.Perm{"ci": {View: true}, "peer": {View: true, Build: true}})
	u, err := svc.UserFromToken(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	for job, want := range map[string]map[string]bool{
		"ci":         {"view": true, "build": false, "cancel": false},
		"peer":       {"view": true, "build": true, "cancel": false},
		"secret-job": {"view": false, "build": false, "cancel": false},
	} {
		for action, ok := range want {
			if got, err := svc.Can(ctx, u, job, action); err != nil || got != ok {
				t.Errorf("%s on %s: %v %v, want %v", action, job, got, err, ok)
			}
		}
	}
	vis, err := svc.Visible(ctx, u, []string{"ci", "peer", "secret-job"})
	if err != nil || !slices.Equal(vis, []string{"ci", "peer"}) {
		t.Fatalf("visible %v %v", vis, err)
	}
}

func TestViewOnAJobCoversItsChildrenByTheRulesOfCan(t *testing.T) {
	ctx := context.Background()
	svc, st, bt := buildService(t)
	if err := st.PutChild(ctx, store.Child{Parent: "site", Key: "pr-12", Kind: "pr", PR: 12, State: "live", LastSeenAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	u, err := svc.UserFromToken(ctx, bt.Mint(7, "site", 1, "", map[string]store.Perm{"site": {View: true}}))
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := svc.Can(ctx, u, "site.pr-12", "view"); err != nil || !ok {
		t.Fatalf("view on a child: %v %v", ok, err)
	}
	if ok, err := svc.Can(ctx, u, "site.pr-12", "build"); err != nil || ok {
		t.Fatalf("build on a child: %v %v", ok, err)
	}
}

func TestARevokedOrUnknownBuildTokenIsNotFoundAndTheStoreIsNotAsked(t *testing.T) {
	ctx := context.Background()
	svc, _, bt := buildService(t)
	tok := bt.Mint(7, "ci", 1, "", nil)
	bt.Revoke(7)
	for _, plain := range []string{tok, BuildTokenPrefix + "nobody", ""} {
		if _, err := svc.UserFromToken(ctx, plain); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%q: %v", plain, err)
		}
	}
	// A server that serves none refuses the shape alone.
	bare, _ := service(t)
	if _, err := bare.UserFromToken(ctx, tok); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no build tokens served: %v", err)
	}
}

func TestOneTokenPerBuildAndRegrantChangesWhatItHolds(t *testing.T) {
	ctx := context.Background()
	svc, _, bt := buildService(t)
	first := bt.Mint(7, "ci", 1, "", map[string]store.Perm{"ci": {View: true}})
	second := bt.Mint(7, "ci", 1, "", map[string]store.Perm{"ci": {View: true}})
	if _, err := svc.UserFromToken(ctx, first); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the first token was replaced: %v", err)
	}
	bt.Regrant(7, map[string]store.Perm{"ci": {View: true}, "peer": {Cancel: true}})
	u, err := svc.UserFromToken(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := svc.Can(ctx, u, "peer", "cancel"); !ok {
		t.Fatal("regrant must reach the token")
	}
	bt.Revoke(8) // a build with none: nothing happens
}

func TestARestartedServerKnowsNoBuildToken(t *testing.T) {
	svc, _, bt := buildService(t)
	tok := bt.Mint(7, "ci", 1, "", nil)
	svc.UseBuildTokens(NewBuildTokens()) // what a new process starts with
	if _, err := svc.UserFromToken(context.Background(), tok); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("%v", err)
	}
}
