package index

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/github"
	"github.com/ndthuan/kipp/internal/store"
)

// hookEvents are what a repo's webhook delivers to a job.
var hookEvents = []string{"pull_request", "push"} // sorted, to be compared with a listing

// HookPlan is the webhook kipp keeps on every repo of an owner job. A URL
// that is empty, or a secret that is not set, is the reason in Off: no
// hook is touched and the repos are left to the scan.
type HookPlan struct {
	URL, Secret string
	Off         string
}

// HookChange is how a repo's webhook stands after kipp looked at it.
type HookChange struct {
	Repo, Hook, Note string
	// Changed says the state or the reason is not what the last look found,
	// so that a line about it is new.
	Changed bool
}

// ManageHooks makes the webhook the plan describes exist on every live
// repo of the owner job: it is created where it is missing and updated
// where its URL, events or secret differ. A hook is recognised by its URL.
// kipp never deletes one. A repo whose hook cannot be managed is not an
// error: its row says why, and the scan builds what a delivery would have.
func (x *Index) ManageHooks(ctx context.Context, owner string, gh *github.Client, plan HookPlan) ([]HookChange, error) {
	var out []HookChange
	var errs []error
	for _, row := range x.Repos(owner) {
		if row.State != config.RepoLive {
			continue
		}
		hook, note, fp := plan.manage(ctx, gh, row)
		change := HookChange{Repo: row.Repo, Hook: hook, Note: note, Changed: hook != row.Hook || note != row.HookNote}
		if change.Changed || fp != row.HookFP {
			row.Hook, row.HookNote, row.HookFP = hook, note, fp
			errs = append(errs, x.setHook(ctx, row))
		}
		out = append(out, change)
	}
	return out, errors.Join(errs...)
}

func (x *Index) setHook(ctx context.Context, row store.OwnerRepo) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if !slices.ContainsFunc(x.repos[row.Owner], func(r store.OwnerRepo) bool { return r.Key == row.Key }) {
		return nil // a reload took it away meanwhile
	}
	return x.putRepoLocked(ctx, row)
}

// manage looks at the repo's hooks and acts: the state it leaves, the
// reason when it is off, and the fingerprint of what was written.
func (p HookPlan) manage(ctx context.Context, gh *github.Client, row store.OwnerRepo) (hook, note, fp string) {
	if p.Off != "" {
		return store.HookOff, p.Off, ""
	}
	hooks, err := gh.Hooks(ctx, row.Repo)
	if err != nil {
		return store.HookOff, hookFailure(err), row.HookFP
	}
	spec := github.HookSpec{URL: p.URL, Secret: p.Secret, Events: hookEvents}
	fp = fingerprint(spec)
	i := indexOfURL(hooks, p.URL)
	switch {
	case i < 0:
		err = gh.CreateHook(ctx, row.Repo, spec)
	case needsUpdate(hooks[i], row.HookFP, fp):
		err = gh.UpdateHook(ctx, row.Repo, hooks[i].ID, spec)
	}
	if err != nil {
		return store.HookOff, hookFailure(err), row.HookFP
	}
	return store.HookOK, "", fp
}

func indexOfURL(hooks []github.Hook, u string) int {
	return slices.IndexFunc(hooks, func(h github.Hook) bool { return strings.TrimRight(h.URL, "/") == strings.TrimRight(u, "/") })
}

func misshapen(h github.Hook) bool {
	return !h.Active || !h.JSON || !slices.Equal(slices.Sorted(slices.Values(h.Events)), hookEvents)
}

// needsUpdate: GitHub never hands a secret back, so the one kipp wrote is
// known only by the fingerprint it kept.
func needsUpdate(h github.Hook, have, want string) bool {
	return misshapen(h) || have != want
}

func fingerprint(s github.HookSpec) string {
	sum := sha256.Sum256([]byte(s.URL + "\x00" + s.Secret + "\x00" + strings.Join(s.Events, ",")))
	return hex.EncodeToString(sum[:])
}

// hookFailure says why a hook could not be kept, in words for the owner
// page: GitHub answers 404 or 403 to a token that may not read or write
// a repo's webhooks.
func hookFailure(err error) string {
	var ge *github.Error
	if errors.As(err, &ge) && (ge.Status == http.StatusNotFound || ge.Status == http.StatusForbidden) {
		return fmt.Sprintf("the token cannot manage this repo's webhooks (%s): it needs Webhooks, read and write", ge.Error())
	}
	return err.Error()
}

// OldHooks are the repos that carry a webhook kipp made and no longer
// keeps: the ones that left the set, and, once the owner job signs in as a
// GitHub App, every one the job made before it did, since the App's own
// webhook delivers for them now. kipp never deletes one, so the page lists
// them for the operator to remove.
func (x *Index) OldHooks(owner string) []string {
	app := false
	if o := x.Owner(owner); o != nil {
		app = o.App != nil
	}
	var out []string
	for _, r := range x.Repos(owner) {
		if r.Hook == store.HookOK && (app || r.State == config.RepoGone) {
			out = append(out, r.Repo)
		}
	}
	return out
}
