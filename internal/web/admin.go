package web

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/store"
	"github.com/ndthuan/kipp/internal/webhook"
)

func (s *Site) adminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin", s.withUser(func(w http.ResponseWriter, r *http.Request, _ *store.User) {
		redirect(w, r, "/admin/users", http.StatusSeeOther)
	}))
	mux.HandleFunc("GET /admin/users", s.withUser(s.users))
	mux.HandleFunc("POST /admin/users", s.withUser(s.oneTime(s.addUser)))
	mux.HandleFunc("GET /admin/users/{name}", s.withUser(s.userPage))
	mux.HandleFunc("POST /admin/users/{name}/admin/on", s.withUser(s.setAdmin(true)))
	mux.HandleFunc("GET /admin/users/{name}/admin/off", s.withUser(s.confirmAdminOff))
	mux.HandleFunc("POST /admin/users/{name}/admin/off", s.withUser(s.setAdmin(false)))
	mux.HandleFunc("GET /admin/users/{name}/password", s.withUser(s.confirmReset))
	mux.HandleFunc("POST /admin/users/{name}/password", s.withUser(s.oneTime(s.resetPassword)))
	mux.HandleFunc("POST /admin/users/{name}/grants", s.withUser(s.setGrants))
	mux.HandleFunc("POST /admin/users/{name}/grants/remove", s.withUser(s.removeGoneGrant))
	mux.HandleFunc("POST /admin/users/{name}/tokens", s.withUser(s.oneTime(s.createToken)))
	mux.HandleFunc("GET /admin/users/{name}/tokens/{id}/revoke", s.withUser(s.confirmRevoke))
	mux.HandleFunc("POST /admin/users/{name}/tokens/{id}/revoke", s.withUser(s.revoke))
	mux.HandleFunc("GET /admin/users/{name}/delete", s.withUser(s.confirmDelete))
	mux.HandleFunc("POST /admin/users/{name}/delete", s.withUser(s.deleteUser))
	mux.HandleFunc("GET /admin/secrets", s.withUser(s.secrets))
	mux.HandleFunc("POST /admin/secrets", s.withUser(s.setSecret))
	mux.HandleFunc("POST /admin/secrets/{name}/scopes", s.withUser(s.scopeSecret))
	mux.HandleFunc("GET /admin/secrets/{name}/delete", s.withUser(s.confirmDeleteSecret))
	mux.HandleFunc("POST /admin/secrets/{name}/delete", s.withUser(s.deleteSecret))
	s.appRoutes(mux)
	s.agentRoutes(mux)
	s.cacheRoutes(mux)
	mux.HandleFunc("GET /admin/server", s.withUser(s.server))
	mux.HandleFunc("POST /admin/server/reload", s.withUser(s.reload))
	mux.HandleFunc("POST /admin/server/quiet", s.withUser(s.quietOn))
	mux.HandleFunc("POST /admin/server/quiet/off", s.withUser(s.quietOff))
	s.upgradeRoutes(mux)
}

type usersData struct {
	Users  []api.UserInfo
	Notice string
}

func (s *Site) users(w http.ResponseWriter, r *http.Request, u *store.User) {
	all, err := s.api.Service().Users(r.Context(), u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "users", http.StatusOK, page{Title: "Users", Here: "/admin/users", Data: usersData{Users: all, Notice: noticeOf(r)}, oneTime: true})
}

func (s *Site) addUser(w http.ResponseWriter, r *http.Request, u *store.User) {
	name := r.PostFormValue("name")
	pw, err := s.api.Service().AddUser(r.Context(), u, api.SessionFrom(r.Context()), name, r.PostFormValue("admin") == "on")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.once(w, r, "/admin/users", onceData{
		Heading: "Password for " + name, Value: pw, Back: userPath(name), BackLabel: "Go to " + name + "'s page",
		Lead: "Hand it to " + name + " in a way only they can read. They change it on their Account page.",
	})
}

// grantGroup is one group of the loaded jobs on a user's grants form, in
// the order of the job list.
type grantGroup struct {
	Label string
	Jobs  []grantRow
}

type grantRow struct {
	Job  string
	Perm store.Perm
	// Warn is why the row was not saved.
	Warn string
}

// grantFault is a refused grants form: what was ticked, and the jobs it
// was refused for.
type grantFault struct {
	perms map[string]store.Perm
	jobs  []string
}

type userData struct {
	Info *api.UserInfo
	// Self: the page is the viewer's own.
	Self   bool
	Groups []grantGroup
	Days   []int
	Notice string
	// Apps are the apps the user let in, when serve has sign-in for MCP
	// clients (AppsOn).
	Apps   []*store.OAuthGrant
	AppsOn bool
}

func (s *Site) userPage(w http.ResponseWriter, r *http.Request, u *store.User) {
	s.showUser(w, r, u, r.PathValue("name"), http.StatusOK, nil)
}

// showUser draws the user's page; with fault, the grants form is as it
// was sent and the refused rows say why.
func (s *Site) showUser(w http.ResponseWriter, r *http.Request, u *store.User, name string, status int, fault *grantFault) {
	svc := s.api.Service()
	info, err := svc.User(r.Context(), u, name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	jobs, err := svc.Jobs(r.Context(), u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d := userData{Info: info, Self: info.Name == u.Name, Days: api.TokenDays, Notice: noticeOf(r)}
	if s.oauth != nil {
		d.AppsOn = true
		if d.Apps, err = s.userApps(r, info.Name); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	for _, g := range groups(jobs) {
		if g.Removed { // not loaded: their grants are the gone ones, listed apart
			continue
		}
		gg := grantGroup{Label: g.label()}
		for _, j := range g.Jobs {
			gg.Jobs = append(gg.Jobs, grantRowOf(j.Name, info.Grants[j.Name], fault))
		}
		d.Groups = append(d.Groups, gg)
	}
	s.render(w, r, "user", status, page{Title: info.Name, Here: "/admin/users", Data: d, oneTime: true})
}

// grantRowOf is a job's row of the grants form. Grants stored as build or
// cancel without view show view ticked, as saving them stores it. After a
// refusal the row is what was sent, and the refused ones say so.
func grantRowOf(job string, stored store.Perm, fault *grantFault) grantRow {
	row := grantRow{Job: job, Perm: stored}
	if fault != nil {
		row.Perm = fault.perms[job]
		if slices.Contains(fault.jobs, job) {
			row.Warn = "Not saved: build and cancel need view too. Tick View for " + job + ", or untick the others."
		}
	} else if stored.Build || stored.Cancel {
		row.Perm.View = true
	}
	return row
}

// userApps are the apps the user called name let in.
func (s *Site) userApps(r *http.Request, name string) ([]*store.OAuthGrant, error) {
	who, err := s.st.UserByName(r.Context(), name)
	if err != nil {
		return nil, err
	}
	return s.oauth.Grants(r.Context(), who.ID)
}

// someone is the user the path names, for an admin.
func (s *Site) someone(w http.ResponseWriter, r *http.Request, u *store.User) *api.UserInfo {
	info, err := s.api.Service().User(r.Context(), u, r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return nil
	}
	return info
}

func (s *Site) setAdmin(on bool) pageHandler {
	what := map[bool]string{true: "admin-on", false: "admin-off"}[on]
	return func(w http.ResponseWriter, r *http.Request, u *store.User) {
		name := r.PathValue("name")
		if err := s.api.Service().SetAdmin(r.Context(), u, api.SessionFrom(r.Context()), name, on); err != nil {
			s.fail(w, r, err)
			return
		}
		done(w, r, userPath(name), what)
	}
}

func (s *Site) confirmAdminOff(w http.ResponseWriter, r *http.Request, u *store.User) {
	info := s.someone(w, r, u)
	if info == nil {
		return
	}
	lines := []string{"From then on " + info.Name + " sees and does only what their grants allow."}
	if info.Name == u.Name {
		lines = append(lines, "This is you: the Users, Secrets and Server pages close to you at once, and only another admin or the kipp command on the server gives admin back.")
	}
	s.confirm(w, r, "/admin/users", confirmData{
		Heading: "Take admin away from " + info.Name + "?", Lines: lines,
		Button: "Take admin away", Back: userPath(info.Name),
	})
}

func (s *Site) confirmReset(w http.ResponseWriter, r *http.Request, u *store.User) {
	info := s.someone(w, r, u)
	if info == nil {
		return
	}
	s.confirm(w, r, "/admin/users", confirmData{
		Heading: "Reset " + info.Name + "'s password?",
		Lines: []string{
			"A new password is made and shown to you once; the one " + info.Name + " has stops working, and every session of theirs ends.",
			"Their tokens keep working unless you tick the box: revoke them when somebody else may hold the account.",
		},
		Check:  &checkBox{Name: "revoke", Label: "Also revoke " + info.Name + "'s tokens"},
		Button: "Reset the password", Back: userPath(info.Name), oneTime: true,
	})
}

func (s *Site) resetPassword(w http.ResponseWriter, r *http.Request, u *store.User) {
	name := r.PathValue("name")
	pw, err := s.api.Service().ResetPassword(r.Context(), u, api.SessionFrom(r.Context()), name, r.PostFormValue("revoke") == "on")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.once(w, r, "/admin/users", onceData{
		Heading: "New password for " + name, Value: pw, Back: userPath(name), BackLabel: "Back to " + name + "'s page",
		Lead: "Hand it to " + name + " in a way only they can read. They change it on their Account page.",
	})
}

// permFields are the grants form's checkboxes: one name a permission, the
// job as the value, so that no job's name can be read as another field.
var permFields = map[string]func(*store.Perm){
	"view":   func(p *store.Perm) { p.View = true },
	"build":  func(p *store.Perm) { p.Build = true },
	"cancel": func(p *store.Perm) { p.Cancel = true },
}

// grantForm is the permissions a grants form sent, by job.
func grantForm(form url.Values) map[string]store.Perm {
	perms := map[string]store.Perm{}
	for field, set := range permFields {
		for _, job := range form[field] {
			p := perms[job]
			set(&p)
			perms[job] = p
		}
	}
	return perms
}

func (s *Site) setGrants(w http.ResponseWriter, r *http.Request, u *store.User) {
	s.applyGrants(w, r, u, r.PathValue("name"), r.PostForm)
}

// applyGrants saves the grants of a form for name, from the grants route
// or from the password step that held the form. A set with build or
// cancel and no view is shown back on the user's page, the ticks as
// sent and the rows named.
func (s *Site) applyGrants(w http.ResponseWriter, r *http.Request, u *store.User, name string, form url.Values) {
	perms := grantForm(form)
	err := s.api.Service().SetGrants(r.Context(), u, api.SessionFrom(r.Context()), name, perms)
	if nv, ok := errors.AsType[*store.NeedsViewError](err); ok {
		s.showUser(w, r, u, name, http.StatusBadRequest, &grantFault{perms: perms, jobs: nv.Jobs})
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	done(w, r, userPath(name), "grants")
}

func (s *Site) removeGoneGrant(w http.ResponseWriter, r *http.Request, u *store.User) {
	name := r.PathValue("name")
	if err := s.api.Service().RemoveGoneGrant(r.Context(), u, api.SessionFrom(r.Context()), name, r.PostFormValue("job")); err != nil {
		s.fail(w, r, err)
		return
	}
	done(w, r, userPath(name), "grant-removed")
}

func (s *Site) createToken(w http.ResponseWriter, r *http.Request, u *store.User) {
	n, err := days(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	name := r.PathValue("name")
	plain, t, err := s.api.Service().CreateToken(r.Context(), u, api.SessionFrom(r.Context()), name, r.PostFormValue("label"), n)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.once(w, r, "/admin/users", onceData{
		Heading: "New token for " + name, Value: plain, Back: userPath(name), BackLabel: "Back to " + name + "'s page",
		Lead: "The token " + strconv.Quote(t.Label) + " acts as " + name + ", with the API and the kipp command. " + tokenLasts(t),
	})
}

// usersToken is the token the path names, of the user it names.
func (s *Site) usersToken(r *http.Request, u *store.User) (*api.UserInfo, *store.Token, error) {
	id, err := tokenID(r)
	if err != nil {
		return nil, nil, err
	}
	info, err := s.api.Service().User(r.Context(), u, r.PathValue("name"))
	if err != nil {
		return nil, nil, err
	}
	t, err := tokenIn(info.Tokens, id)
	return info, t, err
}

func (s *Site) confirmRevoke(w http.ResponseWriter, r *http.Request, u *store.User) {
	info, t, err := s.usersToken(r, u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.confirm(w, r, "/admin/users", confirmData{
		Heading: "Revoke " + info.Name + "'s token " + strconv.Quote(t.Label) + "?",
		Lines:   []string{"Every script and machine that uses it is refused from then on. This cannot be undone."},
		Button:  "Revoke the token", Back: userPath(info.Name),
	})
}

func (s *Site) revoke(w http.ResponseWriter, r *http.Request, u *store.User) {
	info, t, err := s.usersToken(r, u)
	if err == nil {
		err = s.api.Service().RevokeToken(r.Context(), u, api.SessionFrom(r.Context()), t.ID)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	done(w, r, userPath(info.Name), "revoked")
}

func (s *Site) confirmDelete(w http.ResponseWriter, r *http.Request, u *store.User) {
	info := s.someone(w, r, u)
	if info == nil {
		return
	}
	s.confirm(w, r, "/admin/users", confirmData{
		Heading: "Delete " + info.Name + "?",
		Lines: []string{fmt.Sprintf("Their grants on %d jobs, their %d tokens and their %d sessions go with them. This cannot be undone; the builds they started stay.",
			len(info.Grants), len(info.Tokens), info.Sessions)},
		Button: "Delete " + info.Name, Back: userPath(info.Name),
	})
}

func (s *Site) deleteUser(w http.ResponseWriter, r *http.Request, u *store.User) {
	if err := s.api.Service().DeleteUser(r.Context(), u, api.SessionFrom(r.Context()), r.PathValue("name")); err != nil {
		s.fail(w, r, err)
		return
	}
	done(w, r, "/admin/users", "deleted")
}

type secretsData struct {
	*api.Secrets
	// Name is what the set form's name holds: the one a "set" link
	// picked, or the one a refused form sent.
	Name    string
	Notice  string
	Choices *api.ScopeChoices
}

// scopeFieldData is a scopes field: a select of every choice, which
// scopes.js turns into tags.
type scopeFieldData struct {
	ID, Help string
	Groups   []scopeOptionGroup
}

func scopeField(id, help string, c *api.ScopeChoices, have []string) scopeFieldData {
	return scopeFieldData{ID: id, Help: help, Groups: scopeOptions(c, have)}
}

// scopeOptionGroup is one optgroup of a scopes field.
type scopeOptionGroup struct {
	Label   string
	Options []scopeOption
}

type scopeOption struct {
	Value    string
	Selected bool
}

// scopeOptions are the choices of a scopes field with the scopes a secret
// has selected. A scope it has that the loaded jobs do not offer, a job
// pattern or a group no job is in any more, is kept as an option of its
// own, so that saving the field leaves it where it was.
func scopeOptions(c *api.ScopeChoices, have []string) []scopeOptionGroup {
	offered := append(append([]string{config.ScopeGlobal}, c.Groups...), c.Jobs...)
	var other []string
	for _, s := range have {
		if !slices.Contains(offered, s) {
			other = append(other, s)
		}
	}
	group := func(label string, values []string) scopeOptionGroup {
		g := scopeOptionGroup{Label: label}
		for _, v := range values {
			g.Options = append(g.Options, scopeOption{Value: v, Selected: slices.Contains(have, v)})
		}
		return g
	}
	out := []scopeOptionGroup{group("Every job", []string{config.ScopeGlobal})}
	for _, g := range []scopeOptionGroup{group("Groups", c.Groups), group("Jobs", c.Jobs), group("Other", other)} {
		if len(g.Options) > 0 {
			out = append(out, g)
		}
	}
	return out
}

func (s *Site) secrets(w http.ResponseWriter, r *http.Request, u *store.User) {
	s.secretsPage(w, r, u, http.StatusOK, r.URL.Query().Get("name"), "")
}

func (s *Site) secretsPage(w http.ResponseWriter, r *http.Request, u *store.User, code int, name, msg string) {
	all, err := s.api.Service().Secrets(r.Context(), u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	choices, err := s.api.Service().SecretScopeChoices(r.Context(), u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d := secretsData{Secrets: all, Name: name, Choices: choices}
	if code == http.StatusOK {
		d.Notice = noticeOf(r)
	}
	s.render(w, r, "secrets", code, page{Title: "Secrets", Here: "/admin/secrets", Error: msg, Data: d})
}

// setSecret stores a secret of the kind the form names, a value when it
// names none. A value that is refused is not sent back: the form comes
// back with the name and empty values.
func (s *Site) setSecret(w http.ResponseWriter, r *http.Request, u *store.User) {
	in, err := secretForm(r)
	if err == nil {
		err = s.api.Service().SetSecretOf(r.Context(), u, api.SessionFrom(r.Context()), in)
	}
	switch {
	case err == nil:
		done(w, r, "/admin/secrets", "secret-set")
	case errors.Is(err, api.ErrBadInput):
		s.secretsPage(w, r, u, http.StatusBadRequest, in.Name, plainly(err))
	default:
		s.fail(w, r, err)
	}
}

// scopeSecret replaces the scopes of a secret and leaves its value.
func (s *Site) scopeSecret(w http.ResponseWriter, r *http.Request, u *store.User) {
	err := s.api.Service().SetSecretScopes(r.Context(), u, api.SessionFrom(r.Context()), r.PathValue("name"), scopesOf(r))
	switch {
	case err == nil:
		done(w, r, "/admin/secrets", "scoped")
	case errors.Is(err, api.ErrBadInput):
		s.secretsPage(w, r, u, http.StatusBadRequest, "", plainly(err))
	default:
		s.fail(w, r, err)
	}
}

// scopesOf are the scopes a form picked, one value each; nil when it picked
// none.
func scopesOf(r *http.Request) []string {
	var out []string
	for _, f := range r.PostForm["scopes"] {
		if f = strings.TrimSpace(f); f != "" && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

// secretForm is the secret the form sets.
func secretForm(r *http.Request) (api.SecretInput, error) {
	in := api.SecretInput{Name: r.PostFormValue("name"), Kind: cmp.Or(r.PostFormValue("kind"), config.SecretText), Scopes: scopesOf(r)}
	switch in.Kind {
	case config.SecretFile:
		f, _, err := r.FormFile("file")
		if err != nil {
			return in, fmt.Errorf("%w: choose the file to set", api.ErrBadInput)
		}
		defer func() { _ = f.Close() }()
		if in.Data, err = io.ReadAll(io.LimitReader(f, config.MaxSecretFile+1)); err != nil {
			return in, err
		}
	case config.SecretUserPass:
		in.User, in.Value = r.PostFormValue("user"), r.PostFormValue("password")
	default:
		in.Value = r.PostFormValue("value")
	}
	return in, nil
}

func (s *Site) confirmDeleteSecret(w http.ResponseWriter, r *http.Request, u *store.User) {
	all, err := s.api.Service().Secrets(r.Context(), u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	name := r.PathValue("name")
	i := -1
	for at, row := range all.Set {
		if row.Name == name {
			i = at
		}
	}
	if i < 0 {
		s.fail(w, r, api.ErrNotFound)
		return
	}
	line := "No loaded job names it."
	if jobs := all.Set[i].Jobs; len(jobs) > 0 {
		line = "Still named by " + strings.Join(jobs, ", ") + ": their builds start without it, and those that need it fail."
	}
	s.confirm(w, r, "/admin/secrets", confirmData{
		Heading: "Delete the secret " + name + "?",
		Lines:   []string{line, "The value is gone for good; setting it again takes the value once more."},
		Button:  "Delete " + name, Back: "/admin/secrets",
	})
}

func (s *Site) deleteSecret(w http.ResponseWriter, r *http.Request, u *store.User) {
	if err := s.api.Service().DeleteSecret(r.Context(), u, api.SessionFrom(r.Context()), r.PathValue("name")); err != nil {
		s.fail(w, r, err)
		return
	}
	done(w, r, "/admin/secrets", "secret-gone")
}

// webhookRow is a job with a webhook trigger: where GitHub sends its
// pushes, and whether the secret they are signed with is set.
type webhookRow struct {
	Job, URL string
	Set      bool
}

type serverData struct {
	Info             *api.ServerInfo
	Uptime           string
	FreeDisk, DBSize string
	Webhooks         []webhookRow
	Audit            []*store.Audit
	// Older is the id to ask for the rows before, when there may be
	// more; Paged: the page is not the newest rows.
	Older  int64
	Paged  bool
	Notice string
	// Quiet is the server's quiet, for the button that turns it on or off.
	Quiet dispatch.QuietState
	// Upgrade is the card's: what the engine says, nil for a build that is not a release.
	Upgrade *upgradeView
}

// auditPage is how many audit rows the server page shows at a time.
const auditPage = 100

func (s *Site) server(w http.ResponseWriter, r *http.Request, u *store.User) {
	s.serverPage(w, r, u, http.StatusOK, "")
}

func (s *Site) serverPage(w http.ResponseWriter, r *http.Request, u *store.User, code int, msg string) {
	d, err := s.serverData(r, u)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if code == http.StatusOK {
		d.Notice = noticeOf(r)
	}
	s.render(w, r, "server", code, page{Title: "Server", Here: "/admin/server", Error: msg, Data: d, Upgrade: d.Upgrade})
}

func (s *Site) serverData(r *http.Request, u *store.User) (*serverData, error) {
	before, err := auditBefore(r)
	if err != nil {
		return nil, err
	}
	svc := s.api.Service()
	info, err := svc.ServerInfo(r.Context(), u)
	if err != nil {
		return nil, err
	}
	secrets, err := svc.Secrets(r.Context(), u)
	if err != nil {
		return nil, err
	}
	rows, _, err := svc.Audit(r.Context(), u, before, auditPage)
	if err != nil {
		return nil, err
	}
	quiet, err := svc.Quiet(r.Context(), u)
	if err != nil {
		return nil, err
	}
	d := &serverData{Quiet: quiet, Info: info, Uptime: short(time.Since(info.StartedAt)), FreeDisk: "unknown", DBSize: "unknown",
		Webhooks: webhooks(info.BaseURL, secrets), Audit: rows, Paged: before > 0}
	if info.FreeDiskKnown {
		d.FreeDisk = byteSize(int64(min(info.FreeDisk, uint64(1)<<62)))
	}
	if info.DBSize >= 0 {
		d.DBSize = byteSize(info.DBSize)
	}
	if d.Upgrade = s.upgradeViewOf(r, u); d.Upgrade != nil && d.Upgrade.Manual {
		d.Upgrade.ManualLines = manualLines(info.Origin, d.Upgrade.Info.Latest)
	}
	if len(rows) == auditPage {
		d.Older = rows[len(rows)-1].ID
	}
	return d, nil
}

// auditBefore is the audit row the page starts before, 0 for the newest.
func auditBefore(r *http.Request) (int64, error) {
	raw := r.URL.Query().Get("before")
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%w: before is an audit row's number, 1 or more", api.ErrBadInput)
	}
	return n, nil
}

// webhooks are the jobs with a webhook trigger, read from the secrets
// their pushes are signed with, set or missing.
func webhooks(base string, all *api.Secrets) []webhookRow {
	var out []webhookRow
	add := func(rows []api.SecretRow, set bool) {
		for _, row := range rows {
			if !row.Webhook {
				continue
			}
			for _, job := range row.Jobs {
				if webhook.SecretName(job) == row.Name {
					out = append(out, webhookRow{Job: job, URL: webhook.JobURL(base, job), Set: set})
				}
			}
		}
	}
	add(all.Set, true)
	add(all.Missing, false)
	slices.SortFunc(out, func(a, b webhookRow) int { return strings.Compare(a.Job, b.Job) })
	return out
}

// reload re-reads the job files. A refusal is the job files' to explain,
// and the page an admin sees says why.
func (s *Site) reload(w http.ResponseWriter, r *http.Request, u *store.User) {
	err := s.api.Service().Reload(r.Context(), u)
	switch {
	case err == nil:
		done(w, r, "/admin/server", "reloaded")
	case errors.Is(err, api.ErrForbidden):
		s.fail(w, r, err)
	default:
		s.serverPage(w, r, u, http.StatusUnprocessableEntity, "The job files were not reloaded: "+err.Error())
	}
}

// quietOn and quietOff make the server quiet before a restart, or end it.
func (s *Site) quietOn(w http.ResponseWriter, r *http.Request, u *store.User) {
	s.setQuiet(w, r, u, true)
}

func (s *Site) quietOff(w http.ResponseWriter, r *http.Request, u *store.User) {
	s.setQuiet(w, r, u, false)
}

func (s *Site) setQuiet(w http.ResponseWriter, r *http.Request, u *store.User, on bool) {
	if _, err := s.api.Service().SetQuiet(r.Context(), u, on); err != nil {
		s.fail(w, r, err)
		return
	}
	what := "quiet-off"
	if on {
		what = "quiet-on"
	}
	done(w, r, "/admin/server", what)
}
