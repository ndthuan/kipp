package webhook

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/waitfor"
)

func sendFrom(t *testing.T, url, client, sig, body string) int {
	t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	must(t, err)
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", newDelivery())
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set(clientHeader, client)
	res, err := http.DefaultClient.Do(req)
	must(t, err)
	_, err = io.Copy(io.Discard, res.Body)
	must(t, err)
	must(t, res.Body.Close())
	return res.StatusCode
}

func TestOneClientAddressHoldsHalfTheReadersAtMostAndAnotherIsStillRead(t *testing.T) {
	srv, st := serverOpts(t, Options{Secret: topsecret, Readers: 4, Wait: time.Millisecond, Client: byHeader})
	sig := sign("topsecret", push)
	for range 4 {
		headFrom(t, srv.URL, sig, len(push), "a")
	}
	url := srv.URL + "/api/v1/hooks/github/smoke"
	waitfor.Until(t, "the client holds all the readers it may", func() bool {
		return sendFrom(t, url, "a", sign("nope", push), push) == http.StatusServiceUnavailable
	})
	eq(t, "a delivery from another address", sendFrom(t, url, "b", sig, push), http.StatusAccepted)
	if bs, err := st.Builds(context.Background(), "smoke", 5); err != nil || len(bs) != 1 {
		t.Fatalf("%+v %v", bs, err)
	}
}

func TestADeliveryThatFindsNoReaderFreeWaitsForOneAndIsRead(t *testing.T) {
	entered := make(chan struct{}, 3)
	srv, st, _ := serverWrapped(t, Options{Secret: topsecret, Readers: 2, Wait: time.Minute, Client: byHeader},
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				entered <- struct{}{}
				next.ServeHTTP(w, r)
			})
		})
	sig := sign("topsecret", push)
	held := headFrom(t, srv.URL, sig, len(push), "a")
	headFrom(t, srv.URL, sig, len(push), "b")
	<-entered
	<-entered
	answered := make(chan int, 1)
	other := strings.Replace(push, sha, strings.Repeat("1", 40), 1)
	go func() {
		answered <- sendFrom(t, srv.URL+"/api/v1/hooks/github/smoke", "c", sign("topsecret", other), other)
	}()
	<-entered
	_, err := io.WriteString(held, push)
	must(t, err)
	eq(t, "the held delivery", status(t, held), "HTTP/1.1 202")
	eq(t, "the delivery that waited", <-answered, http.StatusAccepted)
	if bs, err := st.Builds(context.Background(), "smoke", 5); err != nil || len(bs) == 0 {
		t.Fatalf("%+v %v", bs, err)
	}
}
