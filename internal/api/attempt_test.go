package api

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestAClientAddressIsReadFromTheHeaderOnlyWhenATrustedProxySentIt(t *testing.T) {
	loopback := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	xff := ClientIP{Header: "X-Forwarded-For", Trusted: loopback}
	cf := ClientIP{Header: "CF-Connecting-IP", Trusted: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}
	for _, c := range []struct {
		what   string
		ip     ClientIP
		peer   string
		header []string
		want   string
	}{
		{what: "no header configured", peer: "127.0.0.1:5000", header: []string{"203.0.113.9"}, want: ""},
		{what: "a trusted proxy's one address", ip: xff, peer: "127.0.0.1:5000", header: []string{"203.0.113.9"}, want: "203.0.113.9"},
		{what: "the rightmost of a list", ip: xff, peer: "127.0.0.1:5000", header: []string{"198.51.100.1, 203.0.113.9"}, want: "203.0.113.9"},
		{what: "the rightmost of several lines", ip: xff, peer: "127.0.0.1:5000", header: []string{"198.51.100.1", "203.0.113.9"}, want: "203.0.113.9"},
		{what: "with a port", ip: xff, peer: "127.0.0.1:5000", header: []string{"203.0.113.9:4711"}, want: "203.0.113.9"},
		{what: "IPv6, by its /64", ip: xff, peer: "[::1]:5000", header: []string{"2001:db8:1:2:3:4:5:6"}, want: "2001:db8:1:2::/64"},
		{what: "an untrusted peer's header", ip: xff, peer: "192.0.2.7:5000", header: []string{"203.0.113.9"}, want: "192.0.2.7"},
		{what: "a trusted proxy that sent none", ip: xff, peer: "127.0.0.1:5000", want: "127.0.0.1"},
		{what: "a header that is no address", ip: xff, peer: "127.0.0.1:5000", header: []string{"unknown"}, want: "127.0.0.1"},
		{what: "a single-value header from a trusted range", ip: cf, peer: "10.1.2.3:443", header: []string{"203.0.113.9"}, want: "203.0.113.9"},
		{what: "the same header from outside the range", ip: cf, peer: "127.0.0.1:443", header: []string{"203.0.113.9"}, want: "127.0.0.1"},
		{what: "an IPv4 peer written as IPv6", ip: xff, peer: "[::ffff:127.0.0.1]:5000", header: []string{"203.0.113.9"}, want: "203.0.113.9"},
	} {
		r := httptest.NewRequest("POST", "/login", nil)
		r.RemoteAddr = c.peer
		for _, v := range c.header {
			r.Header.Add(c.ip.Header, v)
		}
		a := &API{deps: Deps{ClientIP: c.ip}}
		at := a.Attempt(r)
		eq(t, c.what, at.Client, c.want)
		eq(t, c.what+": peer", at.Peer, c.peer)
	}
}

func TestTheDeviceCookieIsSecureAndTheOnlyOneRead(t *testing.T) {
	plain := func(origin string, hdr ...string) *http.Request {
		r := httptest.NewRequest("POST", "/login", nil)
		r.RemoteAddr, r.Host = "127.0.0.1:5000", "127.0.0.1:8085"
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		return r
	}
	a := &API{loopback: true}
	if c := DeviceCookieFor("v"); c.Name != DeviceCookie || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" || c.MaxAge < 300*24*3600 {
		t.Fatalf("got %+v", c)
	}
	both := plain("")
	both.Header.Set("Cookie", DeviceCookie+"=secure; kipp_local_device=local")
	got := a.HeldDevices(both)
	if len(got) != 1 || got[0] != "secure" {
		t.Fatalf("the devices of a Local request: %v", got)
	}
	if at := a.Attempt(both); !at.Local || len(at.Devices) != 1 || at.Client != "" {
		t.Fatalf("%+v", at)
	}
}

// A proxy on the server's own machine reaches it over the loopback; one
// that adds no header and rewrites the Host makes its requests look Local.
// Only a server that listens on a loopback address alone has no such proxy
// it could not tell from a browser.
func TestOnlyAServerListeningOnALoopbackAddressCountsARequestAsItsOwnMachines(t *testing.T) {
	for listen, want := range map[string]bool{
		"127.0.0.1:8085": true, "[::1]:8085": true, "localhost:8085": true,
		"0.0.0.0:8085": false, "[::]:8085": false, "192.168.1.2:8085": false, "": false,
	} {
		a := New(Deps{Listen: listen})
		r := httptest.NewRequest("POST", "/login", nil)
		r.RemoteAddr, r.Host = "127.0.0.1:5000", "127.0.0.1:8085"
		eq(t, listen+": Local", a.Local(r), want)
		eq(t, listen+": counted as the server's own machine", a.Attempt(r).Local, want)
	}
}
