package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// siteTag is a push of tag to acme/site at after, naming head as
// head_commit when given.
func siteTag(tag, after, head string) string {
	return strings.Replace(tagPush(tag, after, head), "example/app", "acme/site", 1)
}

func TestAParentsWebhookBuildsAPushedTagAtTheCommitItNames(t *testing.T) {
	srv, st, _ := multibranchServer(t)
	url := srv.URL + "/api/v1/hooks/github/site"
	tagObject, commit := strings.Repeat("b", 40), strings.Repeat("c", 40)
	body := siteTag("v1.2.0", tagObject, commit)
	code, out := post(t, url, "push", sign("topsecret", body), body)
	if code != http.StatusAccepted {
		t.Fatalf("%d %s", code, out)
	}
	var got struct {
		Builds []queued `json:"builds"`
	}
	must(t, json.Unmarshal([]byte(out), &got))
	if len(got.Builds) != 1 || got.Builds[0].Job != "site.tags" {
		t.Fatalf("%s", out)
	}
	b, err := st.BuildByNumber(context.Background(), "site.tags", 1)
	must(t, err)
	if b.TriggerDetail != commit || b.TriggerTag != "v1.2.0" || b.TriggerBranch != "" || b.Parent != "site" {
		t.Fatalf("an annotated tag's build: %+v", b)
	}
	deleted := `{"ref":"refs/tags/v1.2.0","after":"` + strings.Repeat("0", 40) + `","deleted":true,"repository":{"full_name":"acme/site","default_branch":"main"}}`
	if code, out := post(t, url, "push", sign("topsecret", deleted), deleted); code != http.StatusOK || !strings.Contains(out, "tag v1.2.0 was deleted") {
		t.Fatalf("a tag deletion: %d %s", code, out)
	}
}

func TestAnOwnersWebhookBuildsAPushedTagOfItsRepo(t *testing.T) {
	srv, st := ownerServer(t)
	url := srv.URL + "/api/v1/hooks/github/acme"
	body := siteTag("v2", sha, "")
	if code, out := post(t, url, "push", sign("ownersecret", body), body); code != http.StatusAccepted {
		t.Fatalf("%d %s", code, out)
	}
	b, err := st.BuildByNumber(context.Background(), "acme.site.tags", 1)
	must(t, err)
	if b.TriggerDetail != sha || b.TriggerTag != "v2" {
		t.Fatalf("%+v", b)
	}
}
