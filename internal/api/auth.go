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

var proxyHeaders = []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Via"}

// Local reports whether r came straight from a client on this machine,
// asking for this machine by a loopback address or as localhost. A proxy in
// front, on this machine or not, makes it not Local: by the header it adds,
// or by the public Host it passes through.
func Local(r *http.Request) bool {
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

// DeviceCookie holds the value that marks a browser as one that has
// logged in as a name (auth.Attempt.Devices). It is pinned to this host
// and to https as the session cookie is, and kept a year: a browser that
// loses it only goes back to being counted with strangers until its next
// login.
const DeviceCookie = "__Host-kipp_device"

const deviceAge = 365 * 24 * time.Hour

// DeviceCookieFor is the device cookie a login answers with, holding
// value. It is set anew at every login, so that a browser in use never
// sees it expire.
func DeviceCookieFor(value string) *http.Cookie {
	return &http.Cookie{Name: DeviceCookie, Value: value, Path: "/", MaxAge: int(deviceAge / time.Second), Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}
}

// HeldDevices are the device values r carries.
func (a *API) HeldDevices(r *http.Request) []string {
	var out []string
	if c, err := r.Cookie(DeviceCookie); err == nil {
		out = append(out, c.Value)
	}
	return out
}

// ClientIP is where the address of a request's client is read when the
// server stands behind a proxy (serve --client-ip-header and
// --trusted-proxy). The zero value reads none.
type ClientIP struct {
	Header  string
	Trusted []netip.Prefix
}

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

// HeldSessions are the session ids r carries.
func (a *API) HeldSessions(r *http.Request) []string {
	var out []string
	if c, err := r.Cookie(SessionCookie); err == nil {
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

func (a *API) who(r *http.Request) (*store.User, *store.Session, error) {
	ctx := r.Context()
	if scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " "); strings.EqualFold(scheme, "Bearer") {
		u, err := a.deps.Auth.UserFromToken(ctx, strings.TrimSpace(token))
		if err != nil {
			return nil, nil, unknownIsAnonymous(err)
		}
		return u, nil, nil
	}
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

func csrfMatches(sent, want string) bool {
	return want != "" && subtle.ConstantTimeCompare([]byte(sent), []byte(want)) == 1
}

func unknownIsAnonymous(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}
