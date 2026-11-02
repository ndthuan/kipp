package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

func TestAParentPageListsItsChildrenApartAndAChildLinksUp(t *testing.T) {
	e := site(t)
	ctx := context.Background()
	e.add(t, map[string]string{"site.yml": "name: site\ngroup: web\nmultibranch: {repo: acme/site, clone: /nonexistent/site.git}\n"})
	parent := e.d.Snapshot().Jobs["site"]
	now := time.Now()
	rows := []store.Child{
		{Parent: "site", Key: "main", Kind: "branch", Branch: "main", HeadSHA: "abcdef0123456789", State: "live", LastSeenAt: now},
		{Parent: "site", Key: "pr-12", Kind: "pr", Branch: "feature/login", PR: 12, Title: "Log in", Author: "ann", BaseBranch: "main", State: "live", LastSeenAt: now},
		{Parent: "site", Key: "pr-12-head", Kind: "pr-head", Branch: "feature/login", PR: 12, Title: "Log in", BaseBranch: "main", State: "live", LastSeenAt: now},
		{Parent: "site", Key: "pr-3", Kind: "pr", Branch: "old-work", PR: 3, Title: "Old work", State: "gone", GoneAt: &now, LastSeenAt: now},
		{Parent: "site", Key: "pr-9", Kind: "pr", Branch: "patch", PR: 9, Title: "From afar", FromFork: true, State: "live", LastSeenAt: now},
	}
	var kids []*config.Job
	for _, c := range rows {
		must(t, e.st.PutChild(ctx, c))
		if !c.FromFork {
			kids = append(kids, parent.Child(config.ChildRecord{Parent: "site", Key: c.Key, Kind: c.Kind, Branch: c.Branch, PR: c.PR, Title: c.Title, State: c.State}))
		}
	}
	e.d.SetSnapshot(e.d.Snapshot().WithJobs(kids...))
	c := e.login(t, "admin")
	_, page := get(t, c, e.srv.URL+"/")
	contains(t, page, "3 children, none built yet")
	must(t, e.st.CreateBuild(ctx, &store.Build{Job: "site.pr-12", Parent: "site", Status: store.Failed, Trigger: "webhook", PR: 12}))
	code, page := get(t, c, e.srv.URL+"/jobs/site")
	eq(t, "the parent's page", code, http.StatusOK)
	contains(t, page, "Branches and pull requests", "acme/site", `href="/jobs/site.main"`, "abcdef0", `href="/jobs/site.pr-12"`, "PR #12 Log in", "PR #12 (head alone) Log in",
		"by ann, feature/login", "feature/login into main", "<h3>Gone</h3>", "PR #3 Old work", "Not built: from a fork", "PR #9 From afar")
	lacks(t, page, `href="/jobs/site.pr-9"`)
	code, page = get(t, c, e.srv.URL+"/jobs/site.pr-12")
	eq(t, "a child's page", code, http.StatusOK)
	contains(t, page, `multibranch job <a class="link" href="/jobs/site">site</a>`)
	lacks(t, page, "Branches and pull requests")
	code, page = get(t, c, e.srv.URL+"/")
	eq(t, "the jobs list", code, http.StatusOK)
	// A parent builds nothing itself: its children's worst status stands
	// in the Last build column, where every other job has its own.
	_, site, _ := strings.Cut(page, `<td><a class="link" href="/jobs/site"`)
	site, _, _ = strings.Cut(site, "</tr>")
	_, last, _ := strings.Cut(site, "</td>")
	contains(t, last, `<span class="status failed">failed</span>`, "worst of 3 children")
	lacks(t, last, "never built")
	lacks(t, page, `href="/jobs/site.main"`, `href="/jobs/site.pr-12"`)
}
