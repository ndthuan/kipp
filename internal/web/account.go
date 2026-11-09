package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/store"
)

func (s *Site) accountRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /sudo", s.withUser(s.sudo))
	mux.HandleFunc("GET /account", s.withUser(s.account))
	mux.HandleFunc("POST /account/password", s.withUser(s.changePassword))
	mux.HandleFunc("POST /account/tokens", s.withUser(s.oneTime(s.createOwnToken)))
	mux.HandleFunc("GET /account/tokens/{id}/revoke", s.withUser(s.confirmRevokeOwn))
	mux.HandleFunc("POST /account/tokens/{id}/revoke", s.withUser(s.revokeOwn))
	mux.HandleFunc("POST /account/sessions/end", s.withUser(s.endSessions))
}

// sudoAction is a change that needs the password again: what the form
// that asks for it says the user was doing, and the page the user goes
// back to once it is given. The change is not made then: the user sends
// its form once more, seeing it, so that a password never sends on its
// own a form whose fields were lost on the way.
type sudoAction struct {
	what string
	back func(r *http.Request) string
}

// sudoActions are by the route a change is sent to. A change whose
// confirmation is a page of its own goes back to that page, at the same
// address; the others go back to the page their form is on.
var sudoActions = map[string]sudoAction{
	"POST /account/tokens":                        {"make a token", at("/account")},
	"POST /account/tokens/{id}/revoke":            {"revoke a token", samePath},
	"POST /oauth/authorize":                       {"let an app in", func(r *http.Request) string { return authorizeURL(r.PostForm) }},
	"POST /admin/users":                           {"add a user", at("/admin/users")},
	"POST /admin/users/{name}/admin/on":           {"make a user an admin", userPage},
	"POST /admin/users/{name}/admin/off":          {"take admin away from a user", samePath},
	"POST /admin/users/{name}/password":           {"reset a user's password", samePath},
	"POST /admin/users/{name}/grants":             {"change what a user may do", userPage},
	"POST /admin/users/{name}/grants/remove":      {"remove a grant", userPage},
	"POST /admin/users/{name}/tokens":             {"make a token for a user", userPage},
	"POST /admin/users/{name}/tokens/{id}/revoke": {"revoke a user's token", samePath},
	"POST /admin/users/{name}/delete":             {"delete a user", samePath},
	"POST /admin/apps":                            {"make a GitHub App", at("/admin/apps")},
	"GET /admin/apps/created":                     {"finish making a GitHub App", func(r *http.Request) string { return r.URL.RequestURI() }},
	"POST /admin/secrets":                         {"set a secret", at("/admin/secrets")},
	"POST /admin/secrets/{name}/delete":           {"delete a secret", samePath},
	"POST /admin/secrets/{name}/scopes":           {"change a secret's scopes", at("/admin/secrets")},
}

func at(path string) func(*http.Request) string {
	return func(*http.Request) string { return path }
}

func samePath(r *http.Request) string { return r.URL.EscapedPath() }

func userPage(r *http.Request) string { return userPath(r.PathValue("name")) }

// userPath is a user's page. A name made before the name rule may hold
// anything, a slash included, so it is escaped as one segment.
func userPath(name string) string { return "/admin/users/" + url.PathEscape(name) }

// secretPath is where a secret's own actions are.
func secretPath(name string) string { return "/admin/secrets/" + url.PathEscape(name) }

type sudoData struct {
	// Action is the route of the change, a key of sudoActions; About
	// names what it is about, a user or a secret, when it is about one.
	Action, About string
	What, Next    string
}

// sudoFor is what the password form says to a user whose change, sent
// to the route action and about about, needs it. A route it does not
// know is "this change", leading to the front page.
func sudoFor(action, about string) sudoData {
	d := sudoData{Action: action, About: about, What: "make this change", Next: "/"}
	if a, ok := sudoActions[action]; ok {
		d.What = a.what
	}
	return d
}

// askSudo answers a change that needs the password again with the form
// that asks for it.
func (s *Site) askSudo(w http.ResponseWriter, r *http.Request) {
	d := sudoFor(r.Pattern, r.PathValue("name"))
	if a, ok := sudoActions[r.Pattern]; ok {
		d.Next = localPath(a.back(r))
	}
	s.render(w, r, "sudo", http.StatusForbidden, page{Title: "Confirm your password", Data: d})
}

// sudo gives the session leave to change credentials and secrets, once
// its user has given the password again, and leads back to the page the
// change was started on.
func (s *Site) sudo(w http.ResponseWriter, r *http.Request, u *store.User) {
	d := sudoFor(r.PostFormValue("action"), r.PostFormValue("about"))
	d.Next = localPath(r.PostFormValue("next"))
	err := s.api.Service().Sudo(r.Context(), u, api.SessionFrom(r.Context()), r.PostFormValue("password"), s.api.Attempt(r))
	if err == nil {
		redirect(w, r, d.Next, http.StatusSeeOther)
		return
	}
	var throttled *auth.ThrottledError
	if !errors.Is(err, api.ErrWrongPassword) && !errors.As(err, &throttled) && !errors.Is(err, auth.ErrBusy) {
		s.fail(w, r, err)
		return
	}
	code, msg := refusal(w, err)
	s.render(w, r, "sudo", code, page{Title: "Confirm your password", Error: msg, Data: d})
}

// notices are what a page says after a change it led to, by the done
// parameter of its address: a word, so that nothing a user typed is ever
// put in an address and shown back from it.
var notices = map[string]string{
	"password":          "Your password is changed. Every other session of yours has ended; this browser stays logged in.",
	"sessions":          "Every other session of yours has ended.",
	"revoked":           "The token is revoked: nothing can use it any more.",
	"app-revoked":       "The app is disconnected: its tokens are refused from now on.",
	"admin-on":          "The user is an admin now.",
	"admin-off":         "The user is no longer an admin.",
	"grants":            "The grants are saved.",
	"grant-removed":     "The grant is removed.",
	"deleted":           "The user is deleted, with their grants, tokens and sessions.",
	"scoped":            "The scopes are replaced. Builds that start from now on are held to them.",
	"secret-set":        "The secret is set. Builds that start from now on get the new value; running ones keep the one they started with.",
	"app-created":       "The App is made. Its key is in a file only kipp's user reads and its webhook secret is set. Install it on the account, then put its line in the job file.",
	"secret-gone":       "The secret is deleted.",
	"reloaded":          "The job files are reloaded.",
	"quiet-on":          "kipp is quiet: no build starts, and the running ones end by themselves.",
	"quiet-off":         "kipp takes builds again.",
	"upgrade-started":   "The upgrade is on its way. This page shows how far it is.",
	"upgrade-cancelled": "The upgrade is cancelled. kipp takes builds again and nothing was changed.",
	"engine-added":      "The engine is added. A stage that waits for its labels is placed on it once it is reached.",
	"engine-set":        "The engine is changed.",
	"engine-gone":       "The engine is removed. The containers it runs finish.",
	"agent-set":         "The agent is changed. A disabled agent takes nothing new; what it runs finishes.",
	"agent-gone":        "The agent is removed, and its token refused.",
	"cache-gone":        "The cache entry is removed.",
	"cache-cleared":     "The scope's cache entries are removed.",
}

func noticeOf(r *http.Request) string { return notices[r.URL.Query().Get("done")] }

// done leads back to a page after a change, which then says it was made.
func done(w http.ResponseWriter, r *http.Request, to, what string) {
	redirect(w, r, to+"?done="+what, http.StatusSeeOther)
}

type accountData struct {
	Tokens []*store.Token
	// Apps are the apps let in, when serve has sign-in for MCP clients;
	// AppsOn says whether it has.
	Apps   []*store.OAuthGrant
	AppsOn bool
	// Others is how many other sessions the user has.
	Others int
	Days   []int
	Notice string
	// PasswordError is why the password form was refused.
	PasswordError string
}

func (s *Site) account(w http.ResponseWriter, r *http.Request, u *store.User) {
	s.accountPage(w, r, u, http.StatusOK, "")
}

func (s *Site) accountPage(w http.ResponseWriter, r *http.Request, u *store.User, code int, pwErr string) {
	svc := s.api.Service()
	tokens, err := svc.OwnTokens(r.Context(), u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	others, err := svc.CountOtherSessions(r.Context(), u, api.SessionFrom(r.Context()))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d := accountData{Tokens: tokens, Others: others, Days: api.TokenDays, PasswordError: pwErr}
	if s.oauth != nil {
		d.AppsOn = true
		if d.Apps, err = s.oauth.Grants(r.Context(), u.ID); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if code == http.StatusOK {
		d.Notice = noticeOf(r)
	}
	s.render(w, r, "account", code, page{Title: "Account", Here: "/account", Data: d, oneTime: true})
}

// changePassword replaces the user's password, and this browser's session
// with the new one the Service made: its cookie is set as a login's is.
func (s *Site) changePassword(w http.ResponseWriter, r *http.Request, u *store.User) {
	renewed, err := s.api.Service().ChangePassword(r.Context(), u, api.SessionFrom(r.Context()),
		r.PostFormValue("current"), r.PostFormValue("new"), r.PostFormValue("again"), s.api.Attempt(r))
	switch {
	case err == nil:
		for _, c := range s.api.LoginCookies(r, renewed) {
			http.SetCookie(w, c)
		}
		done(w, r, "/account", "password")
	case errors.Is(err, api.ErrWrongPassword):
		s.accountPage(w, r, u, http.StatusUnauthorized, "The current password is wrong.")
	case errors.Is(err, api.ErrBadInput):
		s.accountPage(w, r, u, http.StatusBadRequest, plainly(err))
	default:
		s.fail(w, r, err)
	}
}

// days is the token lifetime a form picked.
func days(r *http.Request) (int, error) {
	n, err := strconv.Atoi(r.PostFormValue("days"))
	if err != nil {
		return 0, fmt.Errorf("%w: pick how long the token lasts", api.ErrBadInput)
	}
	return n, nil
}

// onceData is a value shown the one time it is ever seen: a token or a
// generated password, on the answer to the form that made it.
type onceData struct {
	Heading, Lead, Value string
	Back, BackLabel      string
}

// once shows a value that is never shown again. It is the answer to the
// POST itself, never a redirect: the value is in no address, and no
// history entry leads back to it.
func (s *Site) once(w http.ResponseWriter, r *http.Request, here string, d onceData) {
	s.render(w, r, "once", http.StatusOK, page{Title: d.Heading, Here: here, Data: d})
}

// tokenLasts says how long a token made now lasts.
func tokenLasts(t *store.Token) string {
	if t.ExpiresAt == nil {
		return "It never expires."
	}
	return "It expires on " + t.ExpiresAt.UTC().Format(time.DateOnly) + "."
}

func (s *Site) createOwnToken(w http.ResponseWriter, r *http.Request, u *store.User) {
	n, err := days(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	plain, t, err := s.api.Service().CreateOwnToken(r.Context(), u, api.SessionFrom(r.Context()), r.PostFormValue("label"), n)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.once(w, r, "/account", onceData{
		Heading: "Your new token", Value: plain, Back: "/account", BackLabel: "Back to your account",
		Lead: "The token " + strconv.Quote(t.Label) + " acts as you, with the API and the kipp command. " + tokenLasts(t),
	})
}

// confirmData is a change that cannot be undone, asked about on a page of
// its own before it is made: the form there sends it.
type confirmData struct {
	Heading string
	Lines   []string
	Action  string // where the form posts, the page's own address
	Button  string
	Back    string
	// BackLabel is what the link back says, "Cancel" when empty.
	BackLabel string
	// Check is a box on the form, off unless ticked.
	Check *checkBox
	// Hidden are fields the form sends besides the token.
	Hidden []hiddenField
	// oneTime: the change's answer shows a value once.
	oneTime bool
}

type checkBox struct{ Name, Label string }

type hiddenField struct{ Name, Value string }

func (s *Site) confirm(w http.ResponseWriter, r *http.Request, here string, d confirmData) {
	d.Action = r.URL.EscapedPath()
	s.render(w, r, "confirm", http.StatusOK, page{Title: d.Heading, Here: here, Data: d, oneTime: d.oneTime})
}

// tokenID is the token the path names; one that is not a number names
// none.
func tokenID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, api.ErrNotFound
	}
	return id, nil
}

// ownToken is the token of u's that the path names.
func (s *Site) ownToken(r *http.Request, u *store.User) (*store.Token, error) {
	id, err := tokenID(r)
	if err != nil {
		return nil, err
	}
	tokens, err := s.api.Service().OwnTokens(r.Context(), u)
	if err != nil {
		return nil, err
	}
	return tokenIn(tokens, id)
}

func tokenIn(tokens []*store.Token, id int64) (*store.Token, error) {
	for _, t := range tokens {
		if t.ID == id {
			return t, nil
		}
	}
	return nil, api.ErrNotFound
}

func (s *Site) confirmRevokeOwn(w http.ResponseWriter, r *http.Request, u *store.User) {
	t, err := s.ownToken(r, u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.confirm(w, r, "/account", confirmData{
		Heading: "Revoke the token " + strconv.Quote(t.Label) + "?",
		Lines:   []string{"Every script and machine that uses it is refused from then on. This cannot be undone: a new token is a new value to hand out."},
		Button:  "Revoke the token", Back: "/account",
	})
}

func (s *Site) revokeOwn(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := tokenID(r)
	if err == nil {
		err = s.api.Service().RevokeOwnToken(r.Context(), u, api.SessionFrom(r.Context()), id)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	done(w, r, "/account", "revoked")
}

func (s *Site) endSessions(w http.ResponseWriter, r *http.Request, u *store.User) {
	if err := s.api.Service().EndOtherSessions(r.Context(), u, api.SessionFrom(r.Context())); err != nil {
		s.fail(w, r, err)
		return
	}
	done(w, r, "/account", "sessions")
}

// lasts is how long a token made with a form's choice lasts, as the
// choice reads.
func lasts(days int) string {
	if days == 0 {
		return "Never expires"
	}
	return strconv.Itoa(days) + " days"
}

// byteSize is a number of bytes as a person reads it.
func byteSize(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// tokensView is a list of tokens with the address of their revoke pages
// under Base.
type tokensView struct {
	Tokens []*store.Token
	Base   string
}

func tokenList(tokens []*store.Token, base string) tokensView { return tokensView{tokens, base} }

// tokenFormView is the form that makes a token, posted to Action.
type tokenFormView struct {
	Action string
	Days   []int
	CSRF   string
	Once   string
}

func tokenForm(action string, days []int, csrf, once string) tokenFormView {
	return tokenFormView{action, days, csrf, once}
}

// expired reports whether a token's end has passed.
func expired(at *time.Time) bool { return at != nil && !time.Now().Before(*at) }
