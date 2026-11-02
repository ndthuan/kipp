package dispatch

import (
	"context"
	"errors"
	"testing"

	"github.com/ndthuan/kipp/internal/config"
)

func TestAnOwnerJobBuildsNothingItself(t *testing.T) {
	h := newHarness(t, map[string]string{"acme": "name: acme\nowner: {login: acme, token: GH_TOKEN}\n"})
	_, err := h.d.Trigger(context.Background(), TriggerRequest{Job: "acme", Kind: "manual"})
	if !errors.Is(err, ErrOwnerJob) {
		t.Fatalf("Trigger(owner job) = %v", err)
	}
}

func TestAStatusOfARepoOfAnOwnerJobIsSaidUnderTheOwnerJob(t *testing.T) {
	multi := &config.Multibranch{PullRequests: config.PRBoth}
	cases := map[string]*config.Job{
		"a parent's child":      {Parent: "site", Multibranch: multi, Origin: &config.ChildRecord{Kind: config.KindBranch}},
		"a repo of an owner":    {Parent: "acme.site", Folder: "acme", Multibranch: multi, Origin: &config.ChildRecord{Kind: config.KindBranch}},
		"its pull request head": {Parent: "acme.site", Folder: "acme", Multibranch: multi, Origin: &config.ChildRecord{Kind: config.KindPRHead}},
	}
	want := map[string]string{"a parent's child": "kipp/site", "a repo of an owner": "kipp/acme", "its pull request head": "kipp/acme/head"}
	for name, job := range cases {
		if got := statusContext(job); got != want[name] {
			t.Errorf("%s: %q, want %q", name, got, want[name])
		}
	}
}
