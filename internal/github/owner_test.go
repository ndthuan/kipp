package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ownerAPI answers the calls that tell an owner's kind and list its repos,
// and records every path asked.
func ownerAPI(t *testing.T, kinds map[string]string, repos map[string]string) (*Client, *[]string) {
	t.Helper()
	var asked []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path+"?"+r.URL.Query().Encode())
		if login, ok := strings.CutPrefix(r.URL.Path, "/users/"); ok && !strings.Contains(login, "/") {
			if kind, ok := kinds[login]; ok {
				_ = json.NewEncoder(w).Encode(map[string]string{"login": login, "type": kind})
				return
			}
		}
		if r.URL.Path == "/user" {
			_ = json.NewEncoder(w).Encode(map[string]string{"login": "me"})
			return
		}
		body, ok := repos[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("page") == "" && strings.Contains(body, "|") {
			first, _, _ := strings.Cut(body, "|")
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?per_page=100&page=2>; rel="next"`, srv.URL, r.URL.Path))
			_, _ = fmt.Fprint(w, first)
			return
		}
		_, rest, _ := strings.Cut(body, "|")
		if rest == "" {
			rest = body
		}
		_, _ = fmt.Fprint(w, rest)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, needle), &asked
}

func names(l *Listing) string {
	var out []string
	for _, r := range l.Repos {
		out = append(out, r.FullName)
	}
	return strings.Join(out, " ")
}

func TestAnOrganizationListsItsReposThroughItsOwnListingAndAllItsPages(t *testing.T) {
	c, asked := ownerAPI(t, map[string]string{"acme": "Organization"}, map[string]string{
		"/orgs/acme/repos": `[{"full_name":"acme/a","name":"a","default_branch":"main","owner":{"login":"acme"}}]|` +
			`[{"full_name":"acme/b","name":"b","default_branch":"trunk","archived":true,"fork":true,"private":true,"owner":{"login":"acme"}}]`,
	})
	l, err := c.OwnerRepos(context.Background(), "acme")
	if err != nil || names(l) != "acme/a acme/b" || l.PublicOnly {
		t.Fatalf("listing %v, %v", l, err)
	}
	if b := l.Repos[1]; b.DefaultBranch != "trunk" || !b.Archived || !b.Fork || !b.Private || b.Name != "b" {
		t.Fatalf("repo %+v", b)
	}
	if !strings.Contains(strings.Join(*asked, " "), "/orgs/acme/repos?per_page=100") {
		t.Fatalf("asked %v", *asked)
	}
}

func TestTheTokensOwnUserListsWhatItOwnsPrivateReposIncluded(t *testing.T) {
	c, asked := ownerAPI(t, map[string]string{"Me": "User"}, map[string]string{
		"/user/repos": `[{"full_name":"Me/x","name":"x","default_branch":"main","private":true,"owner":{"login":"Me"}},` +
			`{"full_name":"acme/not-mine","name":"not-mine","default_branch":"main","owner":{"login":"acme"}}]`,
	})
	l, err := c.OwnerRepos(context.Background(), "Me")
	if err != nil || names(l) != "Me/x" || l.PublicOnly {
		t.Fatalf("listing %v, %v: only what the user owns is theirs", l, err)
	}
	if got := strings.Join(*asked, " "); !strings.Contains(got, "/user/repos?affiliation=owner&per_page=100") {
		t.Fatalf("asked %v", *asked)
	}
}

func TestAnotherUserListsOnlyPublicReposAndTheListingSaysSo(t *testing.T) {
	c, asked := ownerAPI(t, map[string]string{"them": "User"}, map[string]string{
		"/users/them/repos": `[{"full_name":"them/y","name":"y","default_branch":"main","owner":{"login":"them"}}]`,
	})
	l, err := c.OwnerRepos(context.Background(), "them")
	if err != nil || names(l) != "them/y" || !l.PublicOnly {
		t.Fatalf("listing %v, %v", l, err)
	}
	if got := strings.Join(*asked, " "); !strings.Contains(got, "/users/them/repos?per_page=100") || strings.Contains(got, "/user/repos") {
		t.Fatalf("asked %v", *asked)
	}
}

func TestAnUnknownOwnerIsAnErrorThatNamesTheCall(t *testing.T) {
	c, _ := ownerAPI(t, nil, nil)
	_, err := c.OwnerRepos(context.Background(), "nobody")
	var ge *Error
	if err == nil || !errors.As(err, &ge) || ge.Status != http.StatusNotFound || !strings.Contains(err.Error(), "GET /users/nobody") || strings.Contains(err.Error(), needle) {
		t.Fatalf("error %v", err)
	}
}

func TestHooksAreListedCreatedAndUpdatedWithTheSecretOnlyGoingOut(t *testing.T) {
	var calls []string
	var bodies []hookBody
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch r.Method {
		case http.MethodGet:
			_, _ = fmt.Fprint(w, `[{"id":7,"active":true,"events":["push"],"config":{"url":"https://ci.example/api/v1/hooks/github/acme","content_type":"json","secret":"********"}}]`)
		default:
			var b hookBody
			_ = json.NewDecoder(r.Body).Decode(&b)
			bodies = append(bodies, b)
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusCreated)
			}
			_, _ = fmt.Fprint(w, `{}`)
		}
	}))
	hs, err := c.Hooks(context.Background(), "acme/site")
	if err != nil || len(hs) != 1 || hs[0].ID != 7 || hs[0].URL != "https://ci.example/api/v1/hooks/github/acme" || !hs[0].JSON || !hs[0].Active || hs[0].Events[0] != "push" {
		t.Fatalf("hooks %+v, %v", hs, err)
	}
	spec := HookSpec{URL: "https://ci.example/api/v1/hooks/github/acme", Secret: "s3cret", Events: []string{"push", "pull_request"}}
	if err := c.CreateHook(context.Background(), "acme/site", spec); err != nil {
		t.Fatal(err)
	}
	if err := c.UpdateHook(context.Background(), "acme/site", 7, spec); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(calls, ","); got != "GET /repos/acme/site/hooks,POST /repos/acme/site/hooks,PATCH /repos/acme/site/hooks/7" {
		t.Fatalf("calls %s", got)
	}
	for i, b := range bodies {
		checkHookBody(t, i, b, spec)
	}
	if bodies[0].Name != "web" || bodies[1].Name != "" {
		t.Errorf("only a new hook is named web: %q %q", bodies[0].Name, bodies[1].Name)
	}
}

func checkHookBody(t *testing.T, i int, b hookBody, spec HookSpec) {
	t.Helper()
	if !b.Active || b.Config.Secret != "s3cret" || b.Config.ContentType != "json" || b.Config.URL != spec.URL || len(b.Events) != 2 {
		t.Errorf("body %d = %+v", i, b)
	}
}

func TestAHookCallNeverPutsTheTokenInAnError(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found: `+needle+`"}`, http.StatusNotFound)
	}))
	err := c.CreateHook(context.Background(), "acme/site", HookSpec{URL: "u", Secret: "s3cret", Events: []string{"push"}})
	if err == nil || strings.Contains(err.Error(), needle) {
		t.Fatalf("error %v", err)
	}
}
