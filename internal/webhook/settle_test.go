package webhook

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/problem"
	"github.com/ndthuan/kipp/internal/store"
)

func topsecret(context.Context, string) (string, error) { return "topsecret", nil }

func plain(h http.Handler) http.Handler { return h }

func TestADeliveryThatQuietOvertakesAfterItWasRecordedIsKeptAndBuiltOnceWhenQuietEnds(t *testing.T) {
	var st *store.Store
	var d *dispatch.Dispatcher
	record := func(ctx context.Context, id, job string) error {
		if _, err := d.SetQuiet(ctx, true); err != nil {
			return err
		}
		return st.RecordDelivery(ctx, id, job, time.Now())
	}
	srv, st, d := serverDispatch(t, Options{Secret: topsecret, Record: record})
	body := tagPush("v1", sha, sha)
	code, out, _ := send(t, srv.URL+releaseHook, "push", sign("topsecret", body), "d-1", body)
	if code != 202 || !strings.Contains(out, "kept") {
		t.Fatal(code, out)
	}
	if p := pending(t, st); len(p) != 1 || p[0].ID != "d-1" || string(p[0].Body) != body {
		t.Fatalf("%+v", p)
	}
	_, err := d.SetQuiet(context.Background(), false)
	must(t, err)
	if got := tags(t, st); !slices.Equal(got, []string{"v1"}) {
		t.Fatalf("builds %v", got)
	}
	if p := pending(t, st); len(p) != 0 {
		t.Fatalf("still kept: %+v", p)
	}
}

func TestADeliveryThatQuietOvertakesWithNoPlaceToKeepItIs503AndIsBuiltWhenSentAgain(t *testing.T) {
	var st *store.Store
	var d *dispatch.Dispatcher
	var quiet atomic.Bool
	quiet.Store(true)
	record := func(ctx context.Context, id, job string) error {
		if quiet.Load() {
			if _, err := d.SetQuiet(ctx, true); err != nil {
				return err
			}
		}
		return st.RecordDelivery(ctx, id, job, time.Now())
	}
	srv, st, d := serverBuilt(t, Options{Secret: topsecret, Record: record}, plain, false)
	body := tagPush("v1", sha, sha)
	code, out, hdr := send(t, srv.URL+releaseHook, "push", sign("topsecret", body), "d-1", body)
	if code != 503 || !strings.Contains(out, "kipp is quiet") || hdr.Get("Retry-After") != "60" {
		t.Fatal(code, out, hdr)
	}
	quiet.Store(false)
	_, err := d.SetQuiet(context.Background(), false)
	must(t, err)
	if code, out, _ := send(t, srv.URL+releaseHook, "push", sign("topsecret", body), "d-1", body); code != 202 {
		t.Fatal(code, out)
	}
	if got := tags(t, st); !slices.Equal(got, []string{"v1"}) {
		t.Fatalf("builds %v", got)
	}
}

func TestADeliveryAnsweredWithAServerErrorIsBuiltWhenItIsSentAgain(t *testing.T) {
	var st *store.Store
	var fail atomic.Bool
	fail.Store(true)
	record := func(_ context.Context, id, job string) error {
		return st.RecordDelivery(context.Background(), id, job, time.Now())
	}
	cut := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if fail.Load() {
				ctx, cancel := context.WithCancel(r.Context())
				cancel()
				r = r.WithContext(ctx)
			}
			h.ServeHTTP(w, r)
		})
	}
	srv, st, _ := serverWrapped(t, Options{Secret: topsecret, Record: record}, cut)
	body := tagPush("v1", sha, sha)
	if code, out, _ := send(t, srv.URL+releaseHook, "push", sign("topsecret", body), "d-1", body); code != 500 {
		t.Fatal(code, out)
	}
	if got := tags(t, st); len(got) != 0 {
		t.Fatalf("builds %v", got)
	}
	fail.Store(false)
	if code, out, _ := send(t, srv.URL+releaseHook, "push", sign("topsecret", body), "d-1", body); code != 202 {
		t.Fatalf("sent again: %d %s", code, out)
	}
	if code, _, _ := send(t, srv.URL+releaseHook, "push", sign("topsecret", body), "d-1", body); code != 409 {
		t.Fatalf("a third time: %d", code)
	}
	if got := tags(t, st); !slices.Equal(got, []string{"v1"}) {
		t.Fatalf("builds %v", got)
	}
}

func TestAKeptDeliveryWhoseReplayEndsInAServerErrorIsBuiltWhenItIsSentAgain(t *testing.T) {
	url, st, d := quietServer(t)
	if code := sendTag(t, url, "v1", "d-1", "topsecret"); code != 202 {
		t.Fatal(code)
	}
	d.OnQuietEnd(nil)
	_, err := d.SetQuiet(context.Background(), false)
	must(t, err)
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	New(d, Options{
		Pending: func(context.Context) ([]store.PendingDelivery, error) {
			return st.PendingDeliveries(context.Background())
		},
		Done:   func(_ context.Context, seq int64) error { return st.DeletePendingDelivery(context.Background(), seq) },
		Forget: st.ForgetDeliveries,
	}).Replay(gone)
	if got := tags(t, st); len(got) != 0 {
		t.Fatalf("builds %v", got)
	}
	if code := sendTag(t, url, "v1", "d-1", "topsecret"); code != 202 {
		t.Fatalf("sent again: %d", code)
	}
	if got := tags(t, st); !slices.Equal(got, []string{"v1"}) {
		t.Fatalf("builds %v", got)
	}
}

func TestASettledAnswerIsSentAsJSONOrAProblemAndNeverAsAPage(t *testing.T) {
	for given, want := range map[string]string{
		"text/html":       "application/json",
		"":                "application/json",
		problem.MediaType: problem.MediaType,
	} {
		a := &answer{header: http.Header{}, code: http.StatusOK}
		a.Header().Set("Content-Type", given)
		a.Header().Set("Retry-After", "60")
		_, _ = a.Write([]byte(`{"ignored":"<b>"}`))
		rec := httptest.NewRecorder()
		a.sendTo(rec)
		h := rec.Header()
		if h.Get("Content-Type") != want || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Retry-After") != "60" {
			t.Fatalf("given %q: headers %v", given, h)
		}
		if rec.Body.String() != `{"ignored":"<b>"}` {
			t.Fatalf("body %q", rec.Body.String())
		}
	}
}
