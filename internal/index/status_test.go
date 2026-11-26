package index

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/dispatch"
)

const statusSHA = "0123456789abcdef0123456789abcdef01234567"

func statusJob(token string) *config.Job {
	return &config.Job{Name: "site.main", Multibranch: &config.Multibranch{Repo: "acme/site", Token: token}}
}

func TestAStatusIsPostedToTheReposCommitWithTheSecretTheJobNames(t *testing.T) {
	var path, auth string
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.Method+" "+r.URL.Path, r.Header.Get("Authorization")
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	var asked string
	post := PostStatus(srv.URL, func(_ context.Context, name string) (string, error) {
		asked = name
		return "status-token", nil
	})
	err := post(context.Background(), statusJob("GH_TOKEN"), dispatch.CommitStatus{
		SHA: statusSHA, State: "success", Context: "kipp/site", Description: "site.main #3 success", TargetURL: "https://ci.example/jobs/site.main/3"})
	if err != nil {
		t.Fatal(err)
	}
	if path != "POST /repos/acme/site/statuses/"+statusSHA || auth != "Bearer status-token" || asked != "GH_TOKEN" {
		t.Fatalf("%s with %q, the secret asked for was %q", path, auth, asked)
	}
	if body["state"] != "success" || body["context"] != "kipp/site" || body["description"] != "site.main #3 success" || body["target_url"] != "https://ci.example/jobs/site.main/3" {
		t.Fatalf("the body was %v", body)
	}
}

func TestNoTokenPostsNothing(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	t.Cleanup(srv.Close)
	opened := false
	post := PostStatus(srv.URL, func(context.Context, string) (string, error) {
		opened = true
		return "x", nil
	})
	if err := post(context.Background(), statusJob(""), dispatch.CommitStatus{SHA: statusSHA, State: "success", Context: "kipp/site"}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 || opened {
		t.Fatalf("a job with no token opened a secret (%v) or called GitHub %d times", opened, calls.Load())
	}
}

func TestAnUnsetSecretIsAnErrorThatNamesItAndNothingIsSent(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	t.Cleanup(srv.Close)
	post := PostStatus(srv.URL, func(_ context.Context, name string) (string, error) {
		return "", errors.New("secret " + name + " is not set")
	})
	err := post(context.Background(), statusJob("GH_TOKEN"), dispatch.CommitStatus{SHA: statusSHA, State: "success", Context: "kipp/site"})
	if err == nil || !strings.Contains(err.Error(), "GH_TOKEN") || calls.Load() != 0 {
		t.Fatalf("err = %v after %d calls", err, calls.Load())
	}
}

func TestARefusalFromGitHubIsReturnedWithoutTheToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"no access for status-token"}`)
	}))
	t.Cleanup(srv.Close)
	post := PostStatus(srv.URL, func(context.Context, string) (string, error) { return "status-token", nil })
	err := post(context.Background(), statusJob("GH_TOKEN"), dispatch.CommitStatus{SHA: statusSHA, State: "success", Context: "kipp/site"})
	if err == nil || strings.Contains(err.Error(), "status-token") || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v", err)
	}
}
