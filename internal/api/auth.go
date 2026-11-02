package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/problem"
	"github.com/ndthuan/kipp/internal/store"
)

type ctxKey int

const (
	userKey ctxKey = iota
	sessionKey
)

// SessionCookie is the browser session's cookie name; the __Host- prefix
// pins it to this host and to https (or localhost).
const SessionCookie = "__Host-kipp_session"

// CSRFHeader carries the session's CSRF token on every mutating request a browser makes.
const CSRFHeader = "X-CSRF-Token"

// ErrCSRF refuses a session request that changes something without the
// session's CSRF token.
var ErrCSRF = errors.New("missing or wrong CSRF token")

// Refusal answers a request Authenticate turns away: with ErrCSRF, or with
// what the store failed with.
type Refusal func(w http.ResponseWriter, r *http.Request, err error)

// SessionCookieFor is the cookie a login sets. A browser drops a __Host-
// cookie that is not Secure, has a Domain or a Path other than /.
func SessionCookieFor(s *store.Session) *http.Cookie {
	return &http.Cookie{Name: SessionCookie, Value: s.ID, Path: "/", Expires: s.ExpiresAt, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}
}

// ExpiredSessionCookie is the cookie that ends SessionCookie in a browser.
func ExpiredSessionCookie() *http.Cookie {
	return &http.Cookie{Name: SessionCookie, Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}
}

// LocalSessionCookie is the session's cookie name for a browser on the
// server's own machine that reached it over plain HTTP. Not every browser
// keeps a Secure cookie from http://127.0.0.1 (Chrome does, Safari does
// not), so this one is not Secure, and a cookie that is not cannot carry
// the __Host- prefix. It is set and read on a request API.Local takes
// alone.
const LocalSessionCookie = "kipp_local_session"

// proxyHeaders are what a proxy adds to a request it passes on. One of
// them, whatever it says, means the request did not come straight from a
// browser.
var proxyHeaders = []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Via"}

// Local reports whether r came over plain HTTP straight from a client on
// this machine, asking for this machine by a loopback address or as
// localhost. A proxy in front, on this machine or not, makes it not Local:
// by the header it adds, or by the public Host it passes through. Erring
// towards false costs nothing but the Secure cookie being asked for.
func Local(r *http.Request) bool {
	if r.TLS != nil {
		return false
	}
	for _, h := range proxyHeaders {
		if len(r.Header.Values(h)) > 0 {
			return false
		}
	}
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil || !peer.Addr().Unmap().IsLoopback() {
		return false
	}
	return loopbackHost(r.Host)
}

// Local is the package's Local, for a server that listens on a loopback
// address alone (Deps.Listen). One that listens on another may have a
// proxy on its own machine in front, which reaches it over the loopback
// whatever its clients' addresses: should that proxy add no header and
// rewrite the Host, every request through it would look Local.
func (a *API) Local(r *http.Request) bool { return a.loopback && Local(r) }

// loopbackListen reports whether the address a server listens on is a
// loopback one: an IP, or localhost.
func loopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Unmap().IsLoopback()
}

// loopbackHost reports whether a Host header names this machine itself.
func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Unmap().IsLoopback()
}

// LoginCookies are the cookies a login answers r with: the Secure one,
// except to a Local request whose form was served by this same plain
// address (localLogin). The local cookie comes with the end of the Secure
// one, which would otherwise be read before it.
func (a *API) LoginCookies(r *http.Request, s *store.Session) []*http.Cookie {
	if !a.localLogin(r) {
		return []*http.Cookie{SessionCookieFor(s)}
	}
	local := &http.Cookie{Name: LocalSessionCookie, Value: s.ID, Path: "/", Expires: s.ExpiresAt, HttpOnly: true, SameSite: http.SameSiteLaxMode}
	return []*http.Cookie{local, ExpiredSessionCookie()}
}

// localLogin reports whether a login from r gets the cookies that are not
// Secure: r is Local and its form was served by this same plain address.
// The browser says so itself in Origin, which a proxy passes on as it
// came: a login through a proxy that hid itself and rewrote the Host
// still gets the Secure cookies.
func (a *API) localLogin(r *http.Request) bool {
	return a.Local(r) && r.Header.Get("Origin") == "http://"+r.Host
}

// DeviceCookie holds the value that marks a browser as one that has
// logged in as a name (auth.Attempt.Devices). It is pinned to this host
// and to https as the session cookie is, and kept a year: a browser that
// loses it only goes back to being counted with strangers until its next
// login.
const DeviceCookie = "__Host-kipp_device"

// LocalDeviceCookie is DeviceCookie for a browser on the server's own
// machine over plain HTTP, set and read as LocalSessionCookie is.
const LocalDeviceCookie = "kipp_local_device"

// deviceAge is how long a browser keeps its device cookie.
const deviceAge = 365 * 24 * time.Hour

// DeviceCookieFor is the device cookie a login answers r with, holding
// value. It is set anew at every login, so that a browser in use never
// sees it expire.
func (a *API) DeviceCookieFor(r *http.Request, value string) *http.Cookie {
	c := &http.Cookie{Name: DeviceCookie, Value: value, Path: "/", MaxAge: int(deviceAge / time.Second), Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}
	if a.localLogin(r) {
		c.Name, c.Secure = LocalDeviceCookie, false
	}
	return c
}

// HeldDevices are the device values r carries, the Secure cookie's first;
// the local one counts on a Local request alone.
func (a *API) HeldDevices(r *http.Request) []string {
	var out []string
	if c, err := r.Cookie(DeviceCookie); err == nil {
		out = append(out, c.Value)
	}
	if c, err := r.Cookie(LocalDeviceCookie); err == nil && a.Local(r) {
		out = append(out, c.Value)
	}
	return out
}

// ClientIP is where the address of a request's client is read when the
// server stands behind a proxy (serve --client-ip-header and
// --trusted-proxy). The zero value reads none.
type ClientIP struct {
	// Header names the header the proxy puts the client's address in,
	// like X-Forwarded-For or CF-Connecting-IP.
	Header string
	// Trusted are the peers whose Header is believed. Anyone else could
	// write any address there, so for them the peer is the client.
	Trusted []netip.Prefix
}

// client is the address r's client is counted by, or "" when the server
// counts by none.
func (c ClientIP) client(r *http.Request) string {
	if c.Header == "" {
		return ""
	}
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	addr := peer.Addr().Unmap()
	if c.trusts(addr) {
		if sent, ok := lastAddr(r.Header.Values(c.Header)); ok {
			addr = sent
		}
	}
	return counted(addr)
}

func (c ClientIP) trusts(addr netip.Addr) bool {
	for _, p := range c.Trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// lastAddr is the rightmost address in a header's values. In a list such
// as X-Forwarded-For each proxy appends what it saw, so the rightmost is
// the one the trusted proxy wrote and those before it are the client's to
// make up.
func lastAddr(values []string) (netip.Addr, bool) {
	if len(values) == 0 {
		return netip.Addr{}, false
	}
	parts := strings.Split(values[len(values)-1], ",")
	last := strings.TrimSpace(parts[len(parts)-1])
	if ap, err := netip.ParseAddrPort(last); err == nil {
		return ap.Addr().Unmap(), true
	}
	addr, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(last, "["), "]"))
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

// counted is the key a client address is counted by. An IPv6 client is
// counted by its /64, the block one subscriber is commonly given: one
// counter for each of its addresses would be no counter at all.
func counted(addr netip.Addr) string {
	if addr.Is6() {
		p, err := addr.Prefix(64)
		if err == nil {
			return p.String()
		}
	}
	return addr.String()
}

// Attempt is what a password check from r is counted by.
func (a *API) Attempt(r *http.Request) auth.Attempt {
	return auth.Attempt{Devices: a.HeldDevices(r), Local: a.Local(r), Peer: r.RemoteAddr, Client: a.deps.ClientIP.client(r)}
}

// LogoutCookies end every session cookie r may have come with.
func (a *API) LogoutCookies(r *http.Request) []*http.Cookie {
	out := []*http.Cookie{ExpiredSessionCookie()}
	if a.Local(r) {
		out = append(out, &http.Cookie{Name: LocalSessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	}
	return out
}

// HeldSessions are the session ids r carries in the cookies it may be
// known by, the Secure one first; the local one counts on a Local request
// alone.
func (a *API) HeldSessions(r *http.Request) []string {
	var out []string
	if c, err := r.Cookie(SessionCookie); err == nil {
		out = append(out, c.Value)
	}
	if c, err := r.Cookie(LocalSessionCookie); err == nil && a.Local(r) {
		out = append(out, c.Value)
	}
	return out
}

// UserFrom is the authenticated user, or nil.
func UserFrom(ctx context.Context) *store.User {
	u, _ := ctx.Value(userKey).(*store.User)
	return u
}

// SessionFrom is the browser session, or nil for a token request.
func SessionFrom(ctx context.Context) *store.Session {
	s, _ := ctx.Value(sessionKey).(*store.Session)
	return s
}

// Authenticate resolves a bearer token or a session cookie into the
// context. A request with neither, or with one nobody holds, goes on
// anonymous; handlers decide. A session request that is not GET or HEAD
// must carry the CSRF header, or it is 403. A refusal is answered in JSON.
func (a *API) Authenticate(next http.Handler) http.Handler {
	return a.AuthenticateWith(a.refuse, next)
}

// AuthenticateWith is Authenticate for a caller that answers a refusal in
// its own way: the web UI, with a page.
func (a *API) AuthenticateWith(refuse Refusal, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, err := a.identify(r)
		if err != nil {
			refuse(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *API) refuse(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrCSRF) {
		problem.Write(w, problem.CSRF, err.Error())
		return
	}
	a.fail(w, r, err)
}

// identify is the request's context with who sent it.
func (a *API) identify(r *http.Request) (context.Context, error) {
	ctx := r.Context()
	u, s, err := a.who(r)
	if err != nil || u == nil {
		return ctx, err
	}
	ctx = context.WithValue(ctx, userKey, u)
	if s != nil {
		ctx = context.WithValue(ctx, sessionKey, s)
	}
	return ctx, nil
}

// who is the user who sent r, and their session for a session request; no
// user for an anonymous one. A bearer token, its scheme in any case (RFC
// 9110), is resolved alone: the cookie beside it neither rescues a bad one
// nor adds to what a good one may do.
func (a *API) who(r *http.Request) (*store.User, *store.Session, error) {
	ctx := r.Context()
	if scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " "); strings.EqualFold(scheme, "Bearer") {
		u, err := a.deps.Auth.UserFromToken(ctx, strings.TrimSpace(token))
		if err != nil {
			return nil, nil, unknownIsAnonymous(err)
		}
		return u, nil, nil
	}
	// The first cookie that is a session is the one: a dead one beside a
	// live one does not shut its holder out.
	for _, id := range a.HeldSessions(r) {
		u, s, err := a.deps.Auth.UserFromSession(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !csrfMatches(r.Header.Get(CSRFHeader), s.CSRF) {
			return nil, nil, ErrCSRF
		}
		return u, s, nil
	}
	return nil, nil, nil
}

// csrfMatches compares in constant time. A session stored without a token
// matches nothing, an absent header included.
func csrfMatches(sent, want string) bool {
	return want != "" && subtle.ConstantTimeCompare([]byte(sent), []byte(want)) == 1
}

// unknownIsAnonymous keeps a store that is down from reading as a bad login.
func unknownIsAnonymous(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}
