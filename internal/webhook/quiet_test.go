package webhook

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/store"
)

const releaseHook = "/api/v1/hooks/github/release"

func quietServer(t *testing.T) (url string, st *store.Store, d *dispatch.Dispatcher) {
	t.Helper()
	srv, st, d := serverDispatch(t, Options{Secret: func(context.Context, string) (string, error) { return "topsecret", nil }})
	_, err := d.SetQuiet(context.Background(), true)
	must(t, err)
	return srv.URL, st, d
}

func sendTag(t *testing.T, url, tag, delivery, secret string) int {
	t.Helper()
	body := tagPush(tag, sha, sha)
	code, _, _ := send(t, url+releaseHook, "push", sign(secret, body), delivery, body)
	return code
}

// tags are the tags the release job built, oldest build first.
func tags(t *testing.T, st *store.Store) []string {
	t.Helper()
	bs, err := st.Builds(context.Background(), "release", 20)
	must(t, err)
	slices.SortFunc(bs, func(a, b *store.Build) int { return a.Number - b.Number })
	var out []string
	for _, b := range bs {
		out = append(out, b.TriggerTag)
	}
	return out
}

func pending(t *testing.T, st *store.Store) []store.PendingDelivery {
	t.Helper()
	p, err := st.PendingDeliveries(context.Background())
	must(t, err)
	return p
}

func TestASignedWebhookWhileQuietIsKeptAndStartsNothing(t *testing.T) {
	url, st, _ := quietServer(t)
	body := tagPush("v1", sha, sha)
	code, out, _ := send(t, url+releaseHook, "push", sign("topsecret", body), "d-1", body)
	if code != 202 || !strings.Contains(out, "kept") {
		t.Fatal(code, out)
	}
	if got := tags(t, st); len(got) != 0 {
		t.Fatalf("a build started while quiet: %v", got)
	}
	if p := pending(t, st); len(p) != 1 || p[0].Name != "release" || p[0].Event != "push" || p[0].ID != "d-1" || string(p[0].Body) != body {
		t.Fatalf("%+v", p)
	}
}

func TestAKeptWebhookStartsOnceWhenQuietEndsAndIsForgotten(t *testing.T) {
	url, st, d := quietServer(t)
	if code := sendTag(t, url, "v1", "d-1", "topsecret"); code != 202 {
		t.Fatal(code)
	}
	_, err := d.SetQuiet(context.Background(), false)
	must(t, err)
	if got := tags(t, st); !slices.Equal(got, []string{"v1"}) {
		t.Fatalf("builds %v", got)
	}
	if p := pending(t, st); len(p) != 0 {
		t.Fatalf("still kept: %+v", p)
	}
	// Quiet turned off again changes nothing.
	_, err = d.SetQuiet(context.Background(), false)
	must(t, err)
	if got := tags(t, st); len(got) != 1 {
		t.Fatalf("builds %v", got)
	}
}

func TestKeptWebhooksAreHandledInTheOrderTheyArrived(t *testing.T) {
	url, st, d := quietServer(t)
	for i, tag := range []string{"v3", "v1", "v2"} {
		if code := sendTag(t, url, tag, "d-"+tag, "topsecret"); code != 202 {
			t.Fatal(i, code)
		}
	}
	_, err := d.SetQuiet(context.Background(), false)
	must(t, err)
	if got := tags(t, st); !slices.Equal(got, []string{"v3", "v1", "v2"}) {
		t.Fatalf("builds %v", got)
	}
}

func TestAKeptWebhookStartsOnceAfterARestart(t *testing.T) {
	url, st, d := quietServer(t)
	if code := sendTag(t, url, "v1", "d-1", "topsecret"); code != 202 {
		t.Fatal(code)
	}
	// A restart ends quiet without anyone turning it off; the new server
	// replays at start, from what the old one kept.
	_, err := d.SetQuiet(context.Background(), false)
	must(t, err)
	if got := tags(t, st); len(got) != 1 {
		t.Fatalf("builds %v", got)
	}
	// What the old server kept and did not get to is still there.
	must(t, st.KeepDelivery(context.Background(), store.PendingDelivery{Route: routeJob, Name: "release", Event: "push", ID: "d-2", Body: []byte(tagPush("v2", sha, sha))}, []string{"release/d-2"}, "release"))
	New(d, keeping(Options{}, st)).Replay(context.Background())
	New(d, keeping(Options{}, st)).Replay(context.Background())
	if got := tags(t, st); !slices.Equal(got, []string{"v1", "v2"}) {
		t.Fatalf("builds %v", got)
	}
	if p := pending(t, st); len(p) != 0 {
		t.Fatalf("still kept: %+v", p)
	}
}

func TestTheSameDeliveryIdStartsOnceWhetherSentWhileQuietOrAfter(t *testing.T) {
	url, st, d := quietServer(t)
	if code := sendTag(t, url, "v1", "d-1", "topsecret"); code != 202 {
		t.Fatal(code)
	}
	if code := sendTag(t, url, "v1", "d-1", "topsecret"); code != 409 {
		t.Fatalf("the same id again while quiet: %d", code)
	}
	if p := pending(t, st); len(p) != 1 {
		t.Fatalf("kept %d", len(p))
	}
	_, err := d.SetQuiet(context.Background(), false)
	must(t, err)
	if code := sendTag(t, url, "v1", "d-1", "topsecret"); code != 409 {
		t.Fatalf("redelivered by hand after it ran: %d", code)
	}
	if got := tags(t, st); len(got) != 1 {
		t.Fatalf("builds %v", got)
	}
}

func TestTheSameBodyUnderANewIdIsNotKeptTwiceWhileQuiet(t *testing.T) {
	url, st, _ := quietServer(t)
	if code := sendTag(t, url, "v1", "d-1", "topsecret"); code != 202 {
		t.Fatal(code)
	}
	if code := sendTag(t, url, "v1", "d-2", "topsecret"); code != 409 {
		t.Fatal(code)
	}
	if p := pending(t, st); len(p) != 1 {
		t.Fatalf("kept %d", len(p))
	}
}

func TestABadSignatureWhileQuietIsRefusedAndKeepsNothing(t *testing.T) {
	url, st, _ := quietServer(t)
	if code := sendTag(t, url, "v1", "d-1", "wrong"); code != 401 {
		t.Fatal(code)
	}
	body := tagPush("v1", sha, sha)
	if code, _, _ := send(t, url+releaseHook, "push", "", "d-2", body); code != 401 {
		t.Fatal(code)
	}
	if p := pending(t, st); len(p) != 0 {
		t.Fatalf("kept %+v", p)
	}
	// A refusal did not record the id: the signed delivery is kept.
	if code := sendTag(t, url, "v1", "d-1", "topsecret"); code != 202 {
		t.Fatal(code)
	}
}

func TestAWebhookWithoutAnIdWhileQuietIs400AndKeepsNothing(t *testing.T) {
	url, st, _ := quietServer(t)
	body := tagPush("v1", sha, sha)
	if code, _, _ := send(t, url+releaseHook, "push", sign("topsecret", body), "", body); code != 400 {
		t.Fatal(code)
	}
	if p := pending(t, st); len(p) != 0 {
		t.Fatalf("kept %+v", p)
	}
}

func TestAnyOtherTriggerWhileQuietIsStillRefused(t *testing.T) {
	_, st, d := quietServer(t)
	for _, kind := range []string{"manual", "cron", "api"} {
		if _, err := d.Trigger(context.Background(), dispatch.TriggerRequest{Job: "release", Kind: kind, By: "someone"}); !errors.Is(err, dispatch.ErrQuiet) {
			t.Fatalf("%s: %v", kind, err)
		}
	}
	if got := tags(t, st); len(got) != 0 {
		t.Fatalf("builds %v", got)
	}
}

func TestAKeptWebhookOfAKeptJobGoneAfterAReloadIsForgottenNotBuilt(t *testing.T) {
	url, st, d := quietServer(t)
	if code := sendTag(t, url, "v1", "d-1", "topsecret"); code != 202 {
		t.Fatal(code)
	}
	snap := d.Snapshot()
	gone := *snap
	jobs := map[string]*config.Job{}
	for k, v := range snap.Jobs {
		if k != "release" {
			jobs[k] = v
		}
	}
	gone.Jobs = jobs
	d.SetSnapshot(&gone)
	_, err := d.SetQuiet(context.Background(), false)
	must(t, err)
	if got := tags(t, st); len(got) != 0 {
		t.Fatalf("builds %v", got)
	}
	if p := pending(t, st); len(p) != 0 {
		t.Fatalf("kept %+v", p)
	}
}

func TestReplayLeavesWhatIsKeptWhileQuietIsOn(t *testing.T) {
	url, st, d := quietServer(t)
	if code := sendTag(t, url, "v1", "d-1", "topsecret"); code != 202 {
		t.Fatal(code)
	}
	New(d, keeping(Options{}, st)).Replay(context.Background())
	if got := tags(t, st); len(got) != 0 {
		t.Fatalf("builds %v", got)
	}
	if p := pending(t, st); len(p) != 1 {
		t.Fatalf("kept %d", len(p))
	}
}

func TestAnAppDeliveryWhileQuietIsKeptAndBuildsWhenQuietEnds(t *testing.T) {
	f := newAppHooks(t, true)
	_, err := f.d.SetQuiet(context.Background(), true)
	must(t, err)
	body := pushOf("acme/site", "acme")
	code, out := f.app(t, "ci", "push", appSecret, body)
	if code != 202 || !strings.Contains(out, "kept") {
		t.Fatal(code, out)
	}
	if f.builds(t, "acme/site") != 0 && f.builds(t, "site") != 0 {
		t.Fatal("a build queued while quiet")
	}
	if p := pending(t, f.st); len(p) != 1 || p[0].Route != routeApp || p[0].Name != "ci" {
		t.Fatalf("%+v", p)
	}
	_, err = f.d.SetQuiet(context.Background(), false)
	must(t, err)
	if p := pending(t, f.st); len(p) != 0 {
		t.Fatalf("%+v", p)
	}
	if code, _ := f.app(t, "ci", "push", appSecret, body); code != 409 {
		t.Fatalf("the same body again: %d", code)
	}
}
