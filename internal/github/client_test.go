package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const needle = "ghp_sekrit_token_value"

// testSHA is a commit as GitHub names one.
const testSHA = "0123456789abcdef0123456789abcdef01234567"

func newClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL, needle)
}

func TestClientAsksWithTheTokenAndReadsTheRepoAndABranch(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+needle {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/repos/acme/site":
			_, _ = fmt.Fprint(w, `{"default_branch":"trunk"}`)
		case "/repos/acme/site/branches/feature/x":
			_, _ = fmt.Fprint(w, `{"name":"feature/x","commit":{"sha":"abc123"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	def, err := c.Repo(context.Background(), "acme/site")
	if err != nil || def != "trunk" {
		t.Fatalf("Repo = %q, %v", def, err)
	}
	sha, err := c.Branch(context.Background(), "acme/site", "feature/x")
	if err != nil || sha != "abc123" {
		t.Fatalf("Branch = %q, %v", sha, err)
	}
}

func TestClientFollowsLinkPagination(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "":
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/acme/site/branches?per_page=100&page=2>; rel="next", <%s/x>; rel="last"`, srv.URL, srv.URL))
			_, _ = fmt.Fprint(w, `[{"name":"main","commit":{"sha":"a1"}},{"name":"dev","commit":{"sha":"b2"}}]`)
		case "2":
			_, _ = fmt.Fprint(w, `[{"name":"topic","commit":{"sha":"c3"}}]`)
		default:
			t.Errorf("unexpected page %q", r.URL.RawQuery)
		}
	}))
	t.Cleanup(srv.Close)
	got, err := New(srv.URL, needle).Branches(context.Background(), "acme/site")
	if err != nil {
		t.Fatal(err)
	}
	want := []Branch{{"main", "a1"}, {"dev", "b2"}, {"topic", "c3"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Branches = %v, want %v", got, want)
	}
}

func TestClientDoesNotFollowALinkToAnotherHost(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the token was sent to another host: %s", r.Header.Get("Authorization"))
	}))
	t.Cleanup(other.Close)
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", fmt.Sprintf(`<%s/steal>; rel="next"`, other.URL))
		_, _ = fmt.Fprint(w, `[]`)
	}))
	if _, err := c.Branches(context.Background(), "acme/site"); err == nil {
		t.Fatal("a next link to another host was followed")
	}
}

func TestClientReadsOpenPullsAndKeepsAForkApart(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/site/pulls" || r.URL.Query().Get("state") != "open" {
			t.Errorf("asked for %s", r.URL)
		}
		_, _ = fmt.Fprint(w, `[
		 {"number":12,"title":"Log in","html_url":"https://github.example/acme/site/pull/12","user":{"login":"ann"},"head":{"ref":"feature/login","sha":"h12","repo":{"full_name":"acme/site"}},"base":{"ref":"main"}},
		 {"number":13,"title":"From afar","head":{"ref":"patch","sha":"h13","repo":{"full_name":"mallory/site"}},"base":{"ref":"main"}},
		 {"number":14,"title":"Fork deleted","head":{"ref":"x","sha":"h14","repo":null},"base":{"ref":"main"}}]`)
	}))
	got, err := c.OpenPulls(context.Background(), "acme/site")
	if err != nil || len(got) != 3 {
		t.Fatalf("OpenPulls = %v, %v", got, err)
	}
	if got[0] != (Pull{12, "Log in", "feature/login", "h12", "acme/site", "ann", "https://github.example/acme/site/pull/12", "main"}) {
		t.Fatalf("pull 12 = %+v", got[0])
	}
	if got[1].HeadRepo != "mallory/site" || got[2].HeadRepo != "" {
		t.Fatalf("head repos = %q, %q", got[1].HeadRepo, got[2].HeadRepo)
	}
}

func TestClientHasFileIsTheContentsAPIAnswer(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ref") != "sha1" {
			t.Errorf("ref = %q", r.URL.Query().Get("ref"))
		}
		switch r.URL.Path {
		case "/repos/acme/site/contents/.kipp.yml":
			_, _ = fmt.Fprint(w, `{"type":"file"}`)
		case "/repos/acme/site/contents/ci/build.yml":
			http.NotFound(w, r)
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	ctx := context.Background()
	if ok, err := c.HasFile(ctx, "acme/site", ".kipp.yml", "sha1"); !ok || err != nil {
		t.Fatalf("200: %v, %v", ok, err)
	}
	if ok, err := c.HasFile(ctx, "acme/site", "ci/build.yml", "sha1"); ok || err != nil {
		t.Fatalf("404: %v, %v", ok, err)
	}
	if _, err := c.HasFile(ctx, "acme/site", "other", "sha1"); err == nil {
		t.Fatal("a 500 is not an answer")
	}
}

func TestClientErrorNeverSaysTheToken(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"unauthorized": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprintf(w, `{"message":"Bad credentials for %s"}`, needle)
		},
		"forbidden": func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no "+needle, http.StatusForbidden) },
		"not found": func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
		"rate limit": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", "1893456000")
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := newClient(t, h).Repo(context.Background(), "acme/site")
			if err == nil {
				t.Fatal("no error")
			}
			if strings.Contains(err.Error(), needle) {
				t.Fatalf("the error says the token: %v", err)
			}
			if !strings.Contains(err.Error(), "GET /repos/acme/site") {
				t.Fatalf("the error does not name the call: %v", err)
			}
		})
	}
}

func TestClientRateLimitErrorSaysSoAndWhen(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "1893456000")
		w.WriteHeader(http.StatusForbidden)
	}))
	_, err := c.Repo(context.Background(), "acme/site")
	if err == nil || !strings.Contains(err.Error(), "rate limit") || !strings.Contains(err.Error(), "2030-01-01") {
		t.Fatalf("err = %v", err)
	}
}

func TestClientTransportErrorNeverSaysTheToken(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens any more
	_, err := New(url, needle).Repo(context.Background(), "acme/site")
	if err == nil || strings.Contains(err.Error(), needle) || !strings.Contains(err.Error(), "GET /repos/acme/site") {
		t.Fatalf("err = %v", err)
	}
}

func TestClientPostsAStatusToTheCommitWithTheTokenAndNothingElse(t *testing.T) {
	var got struct {
		State       string  `json:"state"`
		Context     string  `json:"context"`
		Description string  `json:"description"`
		TargetURL   *string `json:"target_url"`
	}
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/acme/site/statuses/"+testSHA {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+needle {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"id":1}`)
	}))
	err := c.PostStatus(context.Background(), "acme/site", testSHA, Status{State: "success", Context: "kipp/site", Description: "site #3 success"})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "success" || got.Context != "kipp/site" || got.Description != "site #3 success" || got.TargetURL != nil {
		t.Fatalf("the body was %+v, want the state, context and description and no target_url", got)
	}
}

func TestClientPostsATargetURLWhenThereIsOne(t *testing.T) {
	var body map[string]any
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
	}))
	if err := c.PostStatus(context.Background(), "acme/site", testSHA, Status{State: "pending", Context: "kipp/site", TargetURL: "https://ci.example/jobs/site/3"}); err != nil {
		t.Fatal(err)
	}
	if body["target_url"] != "https://ci.example/jobs/site/3" {
		t.Fatalf("the body was %v", body)
	}
}

func TestClientRefusesAStatusItCouldNotAddressBeforeCallingAnything(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a call was made")
	}))
	for _, sha := range []string{"", "../x", "abc/def", "not hex", "ABC"} {
		if err := c.PostStatus(context.Background(), "acme/site", sha, Status{State: "success", Context: "kipp/site"}); err == nil {
			t.Errorf("sha %q was accepted", sha)
		}
	}
	for _, state := range []string{"", "running", "SUCCESS"} {
		if err := c.PostStatus(context.Background(), "acme/site", testSHA, Status{State: state, Context: "kipp/site"}); err == nil {
			t.Errorf("state %q was accepted", state)
		}
	}
}

func TestClientStatusDescriptionIsCutToWhatGitHubTakes(t *testing.T) {
	var body struct{ Description string }
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
	}))
	if err := c.PostStatus(context.Background(), "acme/site", testSHA, Status{State: "failure", Context: "kipp/site", Description: strings.Repeat("x", 300)}); err != nil {
		t.Fatal(err)
	}
	if len(body.Description) != 140 {
		t.Fatalf("the description was %d bytes, GitHub takes 140", len(body.Description))
	}
}

func TestClientStatusErrorNamesTheCallAndNeverTheToken(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"message":"Resource not accessible, token `+needle+`"}`)
	}))
	err := c.PostStatus(context.Background(), "acme/site", testSHA, Status{State: "success", Context: "kipp/site"})
	if err == nil {
		t.Fatal("a refusal was no error")
	}
	if strings.Contains(err.Error(), needle) || !strings.Contains(err.Error(), "POST /repos/acme/site/statuses/"+testSHA) || !strings.Contains(err.Error(), "403") {
		t.Fatalf("the error was %q", err)
	}
}

func TestClientStatusIsNotFollowedToAnotherHost(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the status was sent on to another host: %s %v", r.URL.Path, r.Header)
	}))
	t.Cleanup(other.Close)
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	if err := c.PostStatus(context.Background(), "acme/site", testSHA, Status{State: "success", Context: "kipp/site"}); err == nil {
		t.Fatal("a redirect was taken as a posted status")
	}
}
