package oauth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A client served by a second server, at an https address of its own. The
// server's certificate is the test's, so the fetcher a test hands the
// authorization server trusts it; the guard is tested on its own below.
type clientHost struct {
	*httptest.Server
	hits    atomic.Int64
	body    atomic.Value // string
	headers http.Header
	status  int
}

func newClientHost(t *testing.T) *clientHost {
	t.Helper()
	h := &clientHost{headers: http.Header{}, status: http.StatusOK}
	h.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.hits.Add(1)
		maps.Copy(w.Header(), h.headers)
		w.WriteHeader(h.status)
		_, _ = fmt.Fprint(w, h.body.Load())
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *clientHost) id() string { return h.URL + "/client.json" }

func (h *clientHost) serve(doc string) { h.body.Store(doc) }

func (h *clientHost) valid(extra string) {
	h.serve(fmt.Sprintf(`{"client_id": %q, "client_name": "Claude", "redirect_uris": [%q]%s}`, h.id(), callback, extra))
}

func withFetch(h *clientHost) func(*Config) { return func(c *Config) { c.Fetch = h.Client() } }

func TestAClientNamedByTheUrlOfItsDocumentIsFetchedCheckedAndRecorded(t *testing.T) {
	h := newClientHost(t)
	h.valid(`, "token_endpoint_auth_method": "none"`)
	f := newFixture(t, withFetch(h))
	m := f.signIn(h.id(), true)
	eq(t, "scope", m["scope"], any("kipp:read kipp:build"))
	gs, err := f.srv.Grants(context.Background(), f.user.ID)
	must(t, err)
	eq(t, "client name", gs[0].ClientName, "Claude")
	eq(t, "client id", gs[0].ClientID, h.id())
	if !audited(t, f.st, "oauth.client.register") {
		t.Fatal("the first use of a client is not in the audit")
	}
	c, err := f.st.OAuthClient(context.Background(), h.id())
	must(t, err)
	eq(t, "cimd", c.CIMD, true)
}

func TestAClientsDocumentIsKeptForWhatItsHeadersAllowWithinBounds(t *testing.T) {
	for _, tc := range []struct {
		cache string
		hits  int64 // for requests at 0, 30 minutes, 90 minutes and 25 hours
	}{
		{"", 3},               // an hour by default: fetched again at 90 minutes and at 25 hours
		{"max-age=60", 3},     // not less than an hour
		{"max-age=7200", 2},   // two hours: fetched again at 25 hours only
		{"max-age=864000", 2}, // not more than a day
		{"no-store", 3},       // still an hour
		{"public, max-age=x", 3},
	} {
		h := newClientHost(t)
		h.valid("")
		if tc.cache != "" {
			h.headers.Set("Cache-Control", tc.cache)
		}
		f := newFixture(t, withFetch(h))
		for _, d := range []time.Duration{0, 30 * time.Minute, time.Hour, 23*time.Hour + 30*time.Minute} {
			f.advance(d)
			if _, e := f.srv.ParseAuthorize(context.Background(), f.query(h.id(), nil)); e != nil {
				t.Fatal(e)
			}
		}
		eq(t, "fetches with Cache-Control "+tc.cache, h.hits.Load(), tc.hits)
	}
}

func TestAClientIdIsRefusedUnlessItsDocumentIsTheClientsOwn(t *testing.T) {
	h := newClientHost(t)
	f := newFixture(t, withFetch(h))
	refuse := func(why, doc string) {
		t.Helper()
		h.serve(doc)
		_, e := f.srv.ParseAuthorize(context.Background(), f.query(h.id(), nil))
		if e == nil || e.RedirectURI != "" || e.Code != "invalid_client" {
			t.Errorf("%s: %+v", why, e)
		}
	}
	refuse("another client_id", fmt.Sprintf(`{"client_id": "%s/other.json", "redirect_uris": [%q]}`, h.URL, callback))
	refuse("no client_id", fmt.Sprintf(`{"redirect_uris": [%q]}`, callback))
	refuse("a trailing slash", fmt.Sprintf(`{"client_id": "%s/", "redirect_uris": [%q]}`, h.id(), callback))
	refuse("no redirects", fmt.Sprintf(`{"client_id": %q}`, h.id()))
	refuse("an empty list", fmt.Sprintf(`{"client_id": %q, "redirect_uris": []}`, h.id()))
	refuse("a plain http redirect", fmt.Sprintf(`{"client_id": %q, "redirect_uris": ["http://evil.example/cb"]}`, h.id()))
	refuse("a script redirect", fmt.Sprintf(`{"client_id": %q, "redirect_uris": ["javascript:alert(1)"]}`, h.id()))
	refuse("not JSON", `<html>`)
	refuse("a list", `[]`)
	refuse("larger than 64 KiB", fmt.Sprintf(`{"client_id": %q, "redirect_uris": [%q], "pad": %q}`, h.id(), callback, strings.Repeat("x", maxDocument)))
	h.status = http.StatusNotFound
	refuse("a 404", "{}")
	h.status = http.StatusOK

	// What it names is the client's own, and the request is held to it.
	h.valid("")
	if _, e := f.srv.ParseAuthorize(context.Background(), f.query(h.id(), url.Values{"redirect_uri": {"https://evil.example/cb"}})); e == nil || e.RedirectURI != "" {
		t.Errorf("an address the document does not name: %+v", e)
	}
	// A name that is not text is cut and cleaned; an empty one is the host.
	f.advance(2 * time.Hour) // the cached document ends
	h.serve(fmt.Sprintf(`{"client_id": %q, "client_name": "  \u0007  ", "redirect_uris": [%q]}`, h.id(), callback))
	req, e := f.srv.ParseAuthorize(context.Background(), f.query(h.id(), nil))
	if e != nil {
		t.Fatal(e)
	}
	eq(t, "default name", req.Client.Name, "127.0.0.1")
}

func TestOnlyAnHttpsUrlWithAPathIsFetchedAsAClient(t *testing.T) {
	var fetched atomic.Int64
	counting := &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		fetched.Add(1)
		return nil, fmt.Errorf("no network in this test")
	})}
	f := newFixture(t, func(c *Config) { c.Fetch = counting })
	for _, id := range []string{
		"http://app.example/client.json", "https://app.example", "https://app.example/", "https://user:pw@app.example/c.json",
		"https://app.example/c.json#frag", "https://app.example/a/../c.json", "https://app.example/a/./c.json",
		"ftp://app.example/c.json", "file:///etc/passwd", "https://" + strings.Repeat("a", maxURILen) + ".example/c.json", "https://app.example/\x00",
	} {
		_, e := f.srv.ParseAuthorize(context.Background(), f.query(id, nil))
		if e == nil || e.RedirectURI != "" {
			t.Errorf("%q: %+v", id, e)
		}
	}
	eq(t, "requests made for them", fetched.Load(), 0)
	// A valid one is fetched, and a failure of the network is a refusal.
	_, e := f.srv.ParseAuthorize(context.Background(), f.query("https://app.example/client.json", nil))
	if e == nil || e.Code != "invalid_client" {
		t.Errorf("%+v", e)
	}
	eq(t, "requests made", fetched.Load(), 1)
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAFetchIsNotFollowedToAnythingButHttps(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the plain http server was called")
	}))
	t.Cleanup(plain.Close)
	h := newClientHost(t)
	h.status = http.StatusFound
	h.headers.Set("Location", plain.URL+"/client.json")
	h.valid("")
	f := newFixture(t, withFetch(h))
	_, e := f.srv.ParseAuthorize(context.Background(), f.query(h.id(), nil))
	if e == nil || e.Code != "invalid_client" {
		t.Fatalf("%+v", e)
	}
	// A redirect to https is followed.
	target := newClientHost(t)
	target.serve(fmt.Sprintf(`{"client_id": %q, "redirect_uris": [%q]}`, h.id(), callback))
	h.headers.Set("Location", target.URL+"/moved.json")
	// The test fetcher trusts one server's certificate; give it both.
	pool := h.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	pool.AddCert(target.Certificate())
	f2 := newFixture(t, withFetch(h))
	if _, e := f2.srv.ParseAuthorize(context.Background(), f2.query(h.id(), nil)); e != nil {
		t.Fatalf("a redirect to https: %+v", e)
	}
}

func TestTheGuardRefusesPrivateLoopbackAndLinkLocalAddresses(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1": true, "127.1.2.3": true, "::1": true, "10.0.0.1": true, "172.16.0.1": true, "172.31.255.255": true, "192.168.1.1": true,
		"169.254.169.254": true, "fe80::1": true, "fd00::1": true, "fc00::1": true, "0.0.0.0": true, "::": true, "100.64.0.1": true,
		"::ffff:127.0.0.1": true, "::ffff:10.0.0.1": true, "::ffff:169.254.169.254": true, "224.0.0.1": true, "ff02::1": true,
		"64:ff9b::7f00:1": true, "2002:7f00:1::": true, "198.18.0.1": true, "240.0.0.1": true, "255.255.255.255": true,
		"8.8.8.8": false, "1.1.1.1": false, "93.184.216.34": false, "2606:4700:4700::1111": false, "172.32.0.1": false, "::ffff:8.8.8.8": false,
	} {
		eq(t, addr, blocked(netip.MustParseAddr(addr)), want)
	}
}

func TestTheGuardedClientConnectsToNoPrivateAddress(t *testing.T) {
	var called atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called.Add(1) }))
	t.Cleanup(srv.Close)
	c := GuardedClient()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	c.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	for _, u := range []string{srv.URL, "https://[::1]:1/", "https://localhost:1/", "https://169.254.169.254/latest/meta-data/", "https://10.1.2.3/"} {
		res, err := c.Get(u)
		if err == nil {
			_ = res.Body.Close()
			t.Errorf("%s was fetched", u)
			continue
		}
		if !strings.Contains(err.Error(), "refusing to connect") {
			t.Errorf("%s: %v", u, err)
		}
	}
	eq(t, "calls", called.Load(), 0)
	if c.Timeout == 0 || c.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("the client has no timeout or uses a proxy")
	}
}

func TestAGuardedFetchOfAClientAtALoopbackAddressIsAnInvalidClient(t *testing.T) {
	h := newClientHost(t) // at 127.0.0.1
	h.valid("")
	f := newFixture(t) // the guarded client
	_, e := f.srv.ParseAuthorize(context.Background(), f.query(h.id(), nil))
	if e == nil || e.Code != "invalid_client" || e.RedirectURI != "" {
		t.Fatalf("%+v", e)
	}
	eq(t, "calls", h.hits.Load(), 0)
	if strings.Contains(e.Description, "127.0.0.1") {
		t.Fatalf("the refusal tells what the guard saw: %q", e.Description)
	}
}
