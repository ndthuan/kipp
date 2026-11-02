package auth

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

func TestAChildIsAuthorizedByItsParentsGrant(t *testing.T) {
	ctx := context.Background()
	svc, st := service(t)
	u := addUser(t, st, "ann", "h", false)
	grant(t, st, u.ID, "site", []string{"build"})
	if err := st.PutChild(ctx, store.Child{Parent: "site", Key: "pr-12", Kind: "pr", PR: 12, State: "live", LastSeenAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for job, want := range map[string]map[string]bool{
		"site.pr-12": {"view": true, "build": true, "cancel": false},
		// A name that only looks like a child's is no child: no row says so.
		"site.main": {"view": false, "build": false, "cancel": false},
		"other":     {"view": false, "build": false, "cancel": false},
	} {
		for action, ok := range want {
			got, err := svc.Can(ctx, u, job, action)
			if err != nil || got != ok {
				t.Errorf("%s on %s: %v %v, want %v", action, job, got, err, ok)
			}
		}
	}
	vis, err := svc.Visible(ctx, u, []string{"other", "site", "site.main", "site.pr-12"})
	if err != nil || !slices.Equal(vis, []string{"site", "site.pr-12"}) {
		t.Fatalf("visible %v %v", vis, err)
	}
	// A grant on the child itself adds to the parent's.
	grant(t, st, u.ID, "site.pr-12", []string{"cancel"})
	if ok, err := svc.Can(ctx, u, "site.pr-12", "cancel"); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
}

func TestARepoOfAnOwnerJobAndItsChildrenAreAuthorizedByTheOwnerJobsGrant(t *testing.T) {
	ctx := context.Background()
	svc, st := service(t)
	u := addUser(t, st, "ann", "h", false)
	grant(t, st, u.ID, "acme", []string{"build"})
	seen := time.Now()
	if err := st.PutOwnerRepo(ctx, store.OwnerRepo{Owner: "acme", Key: "site", Repo: "acme/site", State: "live", LastSeenAt: seen}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutChild(ctx, store.Child{Parent: "acme.site", Key: "main", Kind: "branch", Branch: "main", State: "live", LastSeenAt: seen}); err != nil {
		t.Fatal(err)
	}
	for job, want := range map[string]map[string]bool{
		"acme.site":      {"view": true, "build": true, "cancel": false},
		"acme.site.main": {"view": true, "build": true, "cancel": false},
		// No row says these belong to the owner job.
		"acme.other":      {"view": false, "build": false, "cancel": false},
		"acme.site.other": {"view": false, "build": false, "cancel": false},
	} {
		for action, ok := range want {
			got, err := svc.Can(ctx, u, job, action)
			if err != nil || got != ok {
				t.Errorf("%s on %s: %v %v, want %v", action, job, got, err, ok)
			}
		}
	}
	vis, err := svc.Visible(ctx, u, []string{"acme", "acme.other", "acme.site", "acme.site.main"})
	if err != nil || !slices.Equal(vis, []string{"acme", "acme.site", "acme.site.main"}) {
		t.Fatalf("visible %v %v", vis, err)
	}
	// Another user with no grant on the owner job sees none of it.
	v := addUser(t, st, "bob", "h", false)
	if ok, err := svc.Can(ctx, v, "acme.site.main", "view"); err != nil || ok {
		t.Fatalf("a user with no grant: %v %v", ok, err)
	}
}
