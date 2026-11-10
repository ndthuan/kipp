package web

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/oauth"
	"github.com/ndthuan/kipp/internal/store"
)

// WithOAuth adds the pages of the sign-in for MCP clients: the consent
// page and the lists of connected apps. nil adds none.
func WithOAuth(o *oauth.Server) Option {
	return func(s *Site) { s.oauth = o }
}

func (s *Site) oauthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /oauth/authorize", s.withUser(s.authorize))
	mux.HandleFunc("POST /oauth/authorize", s.withUser(s.decide))
	mux.HandleFunc("GET /account/apps/{id}/revoke", s.withUser(s.confirmRevokeApp))
	mux.HandleFunc("POST /account/apps/{id}/revoke", s.withUser(s.revokeApp))
	mux.HandleFunc("GET /admin/users/{name}/apps/{id}/revoke", s.withUser(s.confirmRevokeUsersApp))
	mux.HandleFunc("POST /admin/users/{name}/apps/{id}/revoke", s.withUser(s.revokeUsersApp))
}

// sudoForConsent counts a login that leads to the consent page as the
// password given again: the password has just been typed, and asking for it
// a second time to approve would be the same question twice.
func (s *Site) sudoForConsent(r *http.Request, next string, sess *store.Session) {
	if s.oauth == nil {
		return
	}
	if u, err := url.Parse(next); err != nil || u.Path != "/oauth/authorize" {
		return
	}
	if err := s.st.SetSessionSudo(r.Context(), sess.ID, time.Now().Add(api.SudoFor)); err != nil {
		slog.Warn("web: a login was not given leave to approve an app", "err", err)
	}
}

// consentFields are the parameters of an authorization request, the ones
// the consent form carries back.
var consentFields = []string{"response_type", "client_id", "redirect_uri", "state", "code_challenge", "code_challenge_method", "resource", "scope"}

// authorizeURL is the consent page for the request in v, for the password
// form to lead back to.
func authorizeURL(v url.Values) string {
	q := url.Values{}
	for _, k := range consentFields {
		if v.Get(k) != "" {
			q.Set(k, v.Get(k))
		}
	}
	return "/oauth/authorize?" + q.Encode()
}

type consentData struct {
	Client, Host string
	User         string
	Hidden       []hiddenField
	// AsksBuild: the app asked to start builds; the box is on then.
	AsksBuild bool
}

// authFailed answers a request that was refused: an app whose address is
// not to be trusted is told to the user, any other is told, at its own.
func (s *Site) authFailed(w http.ResponseWriter, r *http.Request, e *oauth.AuthError) {
	if e.RedirectURI == "" {
		s.errorPage(w, r, http.StatusBadRequest, "Cannot connect", "This app cannot be connected: "+e.Description+".")
		return
	}
	http.Redirect(w, r, s.oauth.ErrorRedirect(e), http.StatusSeeOther)
}

// hostSource matches what may stand for a host in a CSP source: letters,
// digits, dots and hyphens, or an IPv6 literal, and a port or a wildcard.
var hostSource = regexp.MustCompile(`^(?:[A-Za-z0-9.-]+|\[[0-9A-Fa-f:]+\])(?::(?:[0-9]+|\*))?$`)

// formAction is the form-action of the consent page: the browser holds the
// redirect that follows a form to it, so it must name where the app is sent.
// An address that does not fit the strict pattern is left out, and the
// redirect is then refused by the browser: the CSP is never built from
// whatever an app registered.
func formAction(redirectURI string) string {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return "'self'"
	}
	host := u.Host
	if u.Scheme == "http" { // on this machine, where the port is the app's to pick
		host = strings.TrimSuffix(host[:len(host)-len(u.Port())], ":")
		host += ":*"
	}
	if !hostSource.MatchString(host) {
		return "'self'"
	}
	return "'self' " + u.Scheme + "://" + host
}

func (s *Site) authorize(w http.ResponseWriter, r *http.Request, u *store.User) {
	req, e := s.oauth.ParseAuthorize(s.oauth.WithSource(r.Context(), r), r.URL.Query())
	if e != nil {
		s.authFailed(w, r, e)
		return
	}
	d := consentData{Client: req.Client.Name, User: u.Name, AsksBuild: req.WantsBuild()}
	if to, err := url.Parse(req.RedirectURI); err == nil {
		d.Host = to.Host
	}
	fields := req.Fields()
	for _, k := range consentFields {
		d.Hidden = append(d.Hidden, hiddenField{k, fields.Get(k)})
	}
	w.Header().Set("Content-Security-Policy", strings.Replace(csp, "form-action 'self'", "form-action "+formAction(req.RedirectURI), 1))
	s.render(w, r, "authorize", http.StatusOK, page{Title: "Connect " + req.Client.Name, Data: d})
}

func (s *Site) decide(w http.ResponseWriter, r *http.Request, u *store.User) {
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, errors.Join(api.ErrBadInput, err))
		return
	}
	req, e := s.oauth.ParseAuthorize(s.oauth.WithSource(r.Context(), r), r.PostForm)
	if e != nil {
		s.authFailed(w, r, e)
		return
	}
	switch r.PostFormValue("decision") {
	case "deny":
		http.Redirect(w, r, s.oauth.Deny(req), http.StatusSeeOther)
	case "approve":
		// What the app gets lasts up to 90 days: as much as a token, so it
		// needs the password as making one does.
		if err := api.RequireSudo(u, api.SessionFrom(r.Context())); err != nil {
			s.fail(w, r, err)
			return
		}
		to, err := s.oauth.Approve(req, u, r.PostFormValue("build") == "on")
		if err != nil {
			s.fail(w, r, err)
			return
		}
		http.Redirect(w, r, to, http.StatusSeeOther)
	default:
		s.fail(w, r, errors.Join(api.ErrBadInput, errors.New("approve or deny")))
	}
}

// appsView is a list of connected apps with the address of their revoke
// pages under Base.
type appsView struct {
	Apps []*store.OAuthGrant
	Base string
}

func appList(apps []*store.OAuthGrant, base string) appsView { return appsView{apps, base} }

// appScopes says what a grant allows, in words.
func appScopes(g *store.OAuthGrant) string {
	if slices.Contains(g.Scopes, oauth.ScopeBuild) {
		return "read, and start and cancel builds"
	}
	return "read only"
}

func appID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, api.ErrNotFound
	}
	return id, nil
}

// appOf is the grant the path names among owner's; one that is not theirs,
// or is gone, is not found.
func (s *Site) appOf(r *http.Request, owner *store.User) (*store.OAuthGrant, error) {
	id, err := appID(r)
	if err != nil {
		return nil, err
	}
	g, err := s.st.OAuthGrant(r.Context(), owner.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, api.ErrNotFound
	}
	return g, err
}

func appLines() []string {
	return []string{"The app is refused from then on, and has to ask you again to get in. This cannot be undone."}
}

func (s *Site) confirmRevokeApp(w http.ResponseWriter, r *http.Request, u *store.User) {
	g, err := s.appOf(r, u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.confirm(w, r, "/account", confirmData{Heading: "Disconnect " + strconv.Quote(g.ClientName) + "?", Lines: appLines(), Button: "Disconnect the app", Back: "/account"})
}

func (s *Site) revokeApp(w http.ResponseWriter, r *http.Request, u *store.User) {
	g, err := s.appOf(r, u)
	if err == nil {
		err = s.oauth.Revoke(r.Context(), u.Name, u, g.ID)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	done(w, r, "/account", "app-revoked")
}

// usersApp is the user the path names, for an admin, with their grant.
func (s *Site) usersApp(r *http.Request, admin *store.User) (*store.User, *store.OAuthGrant, error) {
	info, err := s.api.Service().User(r.Context(), admin, r.PathValue("name"))
	if err != nil {
		return nil, nil, err
	}
	owner, err := s.st.UserByName(r.Context(), info.Name)
	if err != nil {
		return nil, nil, err
	}
	g, err := s.appOf(r, owner)
	return owner, g, err
}

func (s *Site) confirmRevokeUsersApp(w http.ResponseWriter, r *http.Request, u *store.User) {
	owner, g, err := s.usersApp(r, u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.confirm(w, r, "/admin/users", confirmData{
		Heading: "Disconnect " + owner.Name + "'s app " + strconv.Quote(g.ClientName) + "?",
		Lines:   appLines(), Button: "Disconnect the app", Back: userPath(owner.Name),
	})
}

func (s *Site) revokeUsersApp(w http.ResponseWriter, r *http.Request, u *store.User) {
	owner, g, err := s.usersApp(r, u)
	if err == nil {
		err = s.oauth.Revoke(r.Context(), u.Name, owner, g.ID)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	done(w, r, userPath(owner.Name), "app-revoked")
}
