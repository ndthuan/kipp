package index

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ndthuan/kipp/internal/github"
)

// fakeOwner is a GitHub account and its repos as GitHub answers for them:
// the kind of owner, who the token is, the repos in pages of two, each
// with its default branch, its file and its webhooks.
type fakeOwner struct {
	mu    sync.Mutex
	login string
	kind  string // User or Organization
	me    string // the token's own login
	repos []*fakeRepo
	calls []string
	// fail is a path prefix and the status it answers.
	fail map[string]int
	next int64
}

type fakeRepo struct {
	owner, name, def, sha   string
	archived, fork, private bool
	file                    bool
	hooks                   []*fakeHook
	pulls                   []fakePull
	hookStatus              int // the status the hook calls answer, when not 0
}

type fakeHook struct {
	ID     int64
	URL    string
	Events []string
	Active bool
	Secret string
}

func newFakeOwner(login, kind string, repos ...*fakeRepo) *fakeOwner {
	o := &fakeOwner{login: login, kind: kind, me: login, repos: repos, fail: map[string]int{}, next: 100}
	for _, r := range repos {
		r.owner = login
		if r.def == "" {
			r.def = "main"
		}
		if r.sha == "" {
			r.sha = head
		}
	}
	return o
}

func repo(name string) *fakeRepo { return &fakeRepo{name: name, file: true} }

func (o *fakeOwner) client(t *testing.T) *github.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(o.serve))
	t.Cleanup(srv.Close)
	return github.New(srv.URL, "owner-token")
}

func (o *fakeOwner) called(prefix string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, c := range o.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (o *fakeOwner) find(name string) *fakeRepo {
	for _, r := range o.repos {
		if r.name == name {
			return r
		}
	}
	return nil
}

func (o *fakeOwner) serve(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, r.Method+" "+r.URL.Path)
	for prefix, status := range o.fail {
		if strings.HasPrefix(r.URL.Path, prefix) {
			http.Error(w, `{"message":"refused"}`, status)
			return
		}
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case len(parts) == 2 && parts[0] == "users":
		if !strings.EqualFold(parts[1], o.login) {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"login": o.login, "type": o.kind})
	case r.URL.Path == "/user":
		_ = json.NewEncoder(w).Encode(map[string]string{"login": o.me})
	case r.URL.Path == "/installation/repositories" ||
		r.URL.Path == "/user/repos" || r.URL.Path == "/users/"+o.login+"/repos" || r.URL.Path == "/orgs/"+o.login+"/repos":
		o.list(w, r)
	case len(parts) >= 3 && parts[0] == "repos":
		o.repoCall(w, r, parts[2:])
	default:
		http.NotFound(w, r)
	}
}

// list answers a listing in pages of two.
func (o *fakeOwner) list(w http.ResponseWriter, r *http.Request) {
	var visible []*fakeRepo
	for _, rp := range o.repos {
		if rp.private && r.URL.Path == "/users/"+o.login+"/repos" {
			continue
		}
		visible = append(visible, rp)
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	lo, hi := (page-1)*2, min(page*2, len(visible))
	if hi < len(visible) {
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?per_page=100&page=%d>; rel="next"`, r.Host, r.URL.Path, page+1))
	}
	out := []map[string]any{}
	if lo < hi {
		for _, rp := range visible[lo:hi] {
			out = append(out, map[string]any{"full_name": rp.owner + "/" + rp.name, "name": rp.name, "default_branch": rp.def,
				"archived": rp.archived, "fork": rp.fork, "private": rp.private, "owner": map[string]string{"login": rp.owner}})
		}
	}
	if r.URL.Path == "/installation/repositories" { // an installation's listing is an object
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(visible), "repositories": out})
		return
	}
	_ = json.NewEncoder(w).Encode(out)
}

// repoCall answers a call under /repos/<owner>/<name>/.
func (o *fakeOwner) repoCall(w http.ResponseWriter, r *http.Request, rest []string) {
	rp := o.find(rest[0])
	if rp == nil {
		http.NotFound(w, r)
		return
	}
	sub := strings.Join(rest[1:], "/")
	switch {
	case sub == "":
		_ = json.NewEncoder(w).Encode(map[string]string{"default_branch": rp.def})
	case sub == "branches":
		_ = json.NewEncoder(w).Encode([]map[string]any{{"name": rp.def, "commit": map[string]string{"sha": rp.sha}}})
	case strings.HasPrefix(sub, "branches/"):
		_ = json.NewEncoder(w).Encode(map[string]any{"name": rp.def, "commit": map[string]string{"sha": rp.sha}})
	case sub == "pulls":
		out := []map[string]any{}
		for _, pr := range rp.pulls {
			out = append(out, map[string]any{"number": pr.n, "title": pr.title,
				"head": map[string]any{"ref": pr.ref, "sha": pr.sha, "repo": map[string]string{"full_name": pr.repo}},
				"base": map[string]string{"ref": pr.base}})
		}
		_ = json.NewEncoder(w).Encode(out)
	case strings.HasPrefix(sub, "contents/"):
		if rp.file && strings.TrimPrefix(sub, "contents/") == ".kipp.yml" {
			_, _ = w.Write([]byte(`{"type":"file"}`))
			return
		}
		http.NotFound(w, r)
	case sub == "hooks" || strings.HasPrefix(sub, "hooks/"):
		o.hookCall(w, r, rp, strings.TrimPrefix(sub, "hooks"))
	default:
		http.NotFound(w, r)
	}
}

type hookPayload struct {
	Active bool     `json:"active"`
	Events []string `json:"events"`
	Config struct {
		URL         string `json:"url"`
		ContentType string `json:"content_type"`
		Secret      string `json:"secret"`
	} `json:"config"`
}

func (o *fakeOwner) hookCall(w http.ResponseWriter, r *http.Request, rp *fakeRepo, tail string) {
	if rp.hookStatus != 0 {
		http.Error(w, `{"message":"Not Found"}`, rp.hookStatus)
		return
	}
	var in hookPayload
	if r.Method != http.MethodGet {
		_ = json.NewDecoder(r.Body).Decode(&in)
	}
	switch r.Method {
	case http.MethodGet:
		out := []map[string]any{}
		for _, h := range rp.hooks {
			out = append(out, map[string]any{"id": h.ID, "active": h.Active, "events": h.Events,
				"config": map[string]string{"url": h.URL, "content_type": "json", "secret": "********"}})
		}
		_ = json.NewEncoder(w).Encode(out)
	case http.MethodPost:
		o.next++
		rp.hooks = append(rp.hooks, &fakeHook{ID: o.next, URL: in.Config.URL, Events: in.Events, Active: in.Active, Secret: in.Config.Secret})
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	case http.MethodPatch:
		id, _ := strconv.ParseInt(strings.TrimPrefix(tail, "/"), 10, 64)
		i := slices.IndexFunc(rp.hooks, func(h *fakeHook) bool { return h.ID == id })
		if i < 0 {
			http.NotFound(w, r)
			return
		}
		rp.hooks[i].URL, rp.hooks[i].Events, rp.hooks[i].Active, rp.hooks[i].Secret = in.Config.URL, in.Events, in.Active, in.Config.Secret
		_, _ = w.Write([]byte(`{}`))
	default:
		http.Error(w, "no delete here", http.StatusMethodNotAllowed)
	}
}
