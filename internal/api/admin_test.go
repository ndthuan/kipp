package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/secretbox"
	"github.com/ndthuan/kipp/internal/store"
)

// root is an admin in sudo mode.
func (e *env) root(t *testing.T) *person {
	t.Helper()
	p := e.person(t, "root", true)
	e.sudo(t, p)
	return p
}

// withJobs loads the test's job files and these beside them.
func (e *env) withJobs(t *testing.T, files map[string]string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "jobs")
	must(t, os.MkdirAll(dir, 0o750))
	for name, body := range jobFiles {
		must(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}
	for name, body := range files {
		must(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}
	snap, err := config.Load(dir)
	must(t, err)
	e.api.deps.Dispatch.SetSnapshot(snap)
}

func TestAnAdminAddsAUserWithAGeneratedPasswordShownOnce(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	root := e.root(t)
	pw, err := e.api.svc.AddUser(ctx, root.u, root.sess, "carol", true)
	must(t, err)
	if len(pw) < 40 {
		t.Fatalf("a generated password of %d characters", len(pw))
	}
	e.login(t, "carol", pw)
	eq(t, "admin", e.user(t, "carol").Admin, true)
	eq(t, "audit", e.lastAudit(t), "root user.add carol: admin")
	_, err = e.api.svc.AddUser(ctx, root.u, root.sess, "carol", false)
	is(t, "a name taken", err, ErrConflict)
	_, err = e.api.svc.AddUser(ctx, root.u, root.sess, "Carol Smith", false)
	is(t, "a bad name", err, ErrBadInput)
	contains(t, err.Error(), "is not a name a user can have")
}

func TestTheLastAdminIsNotTurnedOff(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	root := e.root(t)
	must(t, e.st.DeleteUser(ctx, e.user(t, "admin").ID))
	err := e.api.svc.SetAdmin(ctx, root.u, root.sess, "root", false)
	is(t, "the last admin", err, ErrConflict)
	contains(t, err.Error(), "root is the last admin")
	must(t, e.api.svc.SetAdmin(ctx, root.u, root.sess, "agent", true))
	eq(t, "audit", e.lastAudit(t), "root user.admin agent: on")
	must(t, e.api.svc.SetAdmin(ctx, root.u, root.sess, "root", false))
	eq(t, "audit", e.lastAudit(t), "root user.admin root: off")
	is(t, "nobody", e.api.svc.SetAdmin(ctx, root.u, root.sess, "nobody", true), ErrNotFound)
}

func TestAResetPasswordEndsTheUsersSessionsAndRevokesTokensOnlyWhenAsked(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	root, alice := e.root(t), e.person(t, "alice", false)
	_, tok, err := e.api.svc.CreateToken(ctx, root.u, root.sess, "alice", "ci", 0)
	must(t, err)
	pw, err := e.api.svc.ResetPassword(ctx, root.u, root.sess, "alice", false)
	must(t, err)
	_, err = e.st.Session(ctx, alice.sess.ID)
	is(t, "alice's session", err, store.ErrNotFound)
	if _, err := e.st.Token(ctx, tok.ID); err != nil {
		t.Fatalf("the token went without being asked: %v", err)
	}
	e.login(t, "alice", pw)
	eq(t, "audit", e.lastAudit(t), "root user.password.reset alice: sessions ended")

	_, err = e.api.svc.ResetPassword(ctx, root.u, root.sess, "alice", true)
	must(t, err)
	_, err = e.st.Token(ctx, tok.ID)
	is(t, "the token", err, store.ErrNotFound)
	eq(t, "audit", e.lastAudit(t), "root user.password.reset alice: sessions ended, 1 tokens revoked")
	if _, err := e.st.Session(ctx, root.sess.ID); err != nil {
		t.Fatalf("the admin's own session ended: %v", err)
	}
}

func TestAnAdminDoesNotDeleteThemselves(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	root := e.root(t)
	err := e.api.svc.DeleteUser(ctx, root.u, root.sess, "root")
	is(t, "oneself", err, ErrConflict)
	contains(t, err.Error(), "another admin or the CLI does that")
	must(t, e.api.svc.DeleteUser(ctx, root.u, root.sess, "viewer"))
	_, err = e.st.UserByName(ctx, "viewer")
	is(t, "viewer", err, store.ErrNotFound)
	eq(t, "audit", e.lastAudit(t), "root user.delete viewer: ")
	is(t, "nobody", e.api.svc.DeleteUser(ctx, root.u, root.sess, "viewer"), ErrNotFound)
}

func TestGrantsAreGivenOnlyOnLoadedJobsAndAGoneJobsGrantStaysUntilRemoved(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	root := e.root(t)
	agent := e.user(t, "agent")
	must(t, e.st.Grant(ctx, agent.ID, "gone", []string{"view"}))

	err := e.api.svc.SetGrants(ctx, root.u, root.sess, "agent", map[string]store.Perm{"smoke": {View: true}, "nosuch": {View: true}})
	is(t, "an unknown job", err, ErrBadInput)
	contains(t, err.Error(), `"nosuch"`)

	must(t, e.api.svc.SetGrants(ctx, root.u, root.sess, "agent", map[string]store.Perm{"gc": {View: true, Build: true}, "smoke": {}}))
	perms, err := e.st.Permissions(ctx, agent.ID)
	must(t, err)
	if len(perms) != 2 || perms["gc"] != (store.Perm{View: true, Build: true}) || perms["gone"] != (store.Perm{View: true}) {
		t.Fatalf("grants %v", perms)
	}
	eq(t, "audit", e.lastAudit(t), "root grants.set agent: gc: view,build")

	err = e.api.svc.RemoveGoneGrant(ctx, root.u, root.sess, "agent", "gc")
	is(t, "a loaded job", err, ErrConflict)
	is(t, "no grant", e.api.svc.RemoveGoneGrant(ctx, root.u, root.sess, "agent", "never"), ErrNotFound)
	must(t, e.api.svc.RemoveGoneGrant(ctx, root.u, root.sess, "agent", "gone"))
	perms, err = e.st.Permissions(ctx, agent.ID)
	must(t, err)
	if len(perms) != 1 || perms["gc"] == (store.Perm{}) {
		t.Fatalf("grants after the removal %v", perms)
	}
	eq(t, "audit", e.lastAudit(t), "root grant.remove agent: gone")
}

func TestAnAdminMakesAndRevokesAnyUsersToken(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	root := e.root(t)
	plain, tok, err := e.api.svc.CreateToken(ctx, root.u, root.sess, "agent", "deploys", 365)
	must(t, err)
	u, err := e.api.deps.Auth.UserFromToken(ctx, plain)
	must(t, err)
	eq(t, "the token's user", u.Name, "agent")
	contains(t, e.lastAudit(t), "root token.create agent: label \"deploys\", id "+itoa(tok.ID)+", expires ")
	_, _, err = e.api.svc.CreateToken(ctx, root.u, root.sess, "nobody", "x", 0)
	is(t, "nobody", err, ErrNotFound)
	must(t, e.api.svc.RevokeToken(ctx, root.u, root.sess, tok.ID))
	eq(t, "audit", e.lastAudit(t), "root token.revoke agent: label \"deploys\", id "+itoa(tok.ID))
	is(t, "again", e.api.svc.RevokeToken(ctx, root.u, root.sess, tok.ID), ErrNotFound)
}

func TestASecretIsSealedUnderItsNameAndOnlyItsNameIsAudited(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	root := e.root(t)
	const value = "an obvious test secret value"
	must(t, e.api.svc.SetSecret(ctx, root.u, root.sess, "TOKEN", value))
	sealed, err := e.st.Secret(ctx, "TOKEN")
	must(t, err)
	if strings.Contains(string(sealed), value) {
		t.Fatal("stored in the clear")
	}
	opened, err := secretbox.Open(testSecretKey, "TOKEN", sealed)
	must(t, err)
	eq(t, "opened", string(opened), value)
	eq(t, "audit", e.lastAudit(t), "root secret.set TOKEN: new text")
	must(t, e.api.svc.SetSecret(ctx, root.u, root.sess, "TOKEN", "another obvious value"))
	eq(t, "audit", e.lastAudit(t), "root secret.set TOKEN: replaced text")

	for what, c := range map[string][2]string{
		"a bad name":  {"NOT-A-NAME", value},
		"two lines":   {"TOKEN", value + "\nmore"},
		"short":       {"TOKEN", "1234567"},
		"over 64 KiB": {"TOKEN", strings.Repeat("x", config.MaxSecret+1)},
	} {
		is(t, what, e.api.svc.SetSecret(ctx, root.u, root.sess, c[0], c[1]), ErrBadInput)
	}

	must(t, e.api.svc.DeleteSecret(ctx, root.u, root.sess, "TOKEN"))
	eq(t, "audit", e.lastAudit(t), "root secret.delete TOKEN: ")
	is(t, "gone", e.api.svc.DeleteSecret(ctx, root.u, root.sess, "TOKEN"), ErrNotFound)
}

func TestAReloadAnAdminAsksForIsAudited(t *testing.T) {
	e := newEnv(t)
	e.call(t, 403, e.viewer, "POST", "/api/v1/admin/reload", "")
	eq(t, "audit rows", len(e.audit(t)), 0)
	e.call(t, 200, e.admin, "POST", "/api/v1/admin/reload", "")
	eq(t, "audit", e.lastAudit(t), "admin reload : taken, 5 jobs")
	refused := error(&JobsRefused{Err: errors.New("smoke.yml: stage s: run is empty")})
	e.reloadErr.Store(&refused)
	text, _ := e.call(t, 422, e.admin, "POST", "/api/v1/admin/reload", "")
	contains(t, text, "run is empty")
	eq(t, "audit", e.lastAudit(t), "admin reload : refused")
}

func TestAnAdminReadsEveryUserWithGrantsTokensAndSessions(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.person(t, "alice", false)
	must(t, e.st.Grant(ctx, alice.u.ID, "smoke", []string{"build"}))
	must(t, e.st.Grant(ctx, alice.u.ID, "gone", []string{"view"}))
	admin := e.user(t, "admin")
	users, err := e.api.svc.Users(ctx, admin) // as with a token: reads need no session
	must(t, err)
	names := []string{}
	for _, u := range users {
		names = append(names, u.Name)
	}
	eq(t, "users", strings.Join(names, ","), "admin,agent,alice,viewer")
	a := users[2]
	if a.Grants["smoke"] != (store.Perm{Build: true}) || !slices.Equal(a.GoneGrants, []string{"gone"}) || a.Sessions != 1 || a.LastLoginAt == nil || a.Admin {
		t.Fatalf("alice: %+v", a)
	}
	eq(t, "agent's tokens", len(users[1].Tokens), 1)
	one, err := e.api.svc.User(ctx, admin, "alice")
	must(t, err)
	eq(t, "one user", one.Name, "alice")
	_, err = e.api.svc.User(ctx, admin, "nobody")
	is(t, "nobody", err, ErrNotFound)
}

func TestTheSecretsReadNamesWhatJobsAskForAndWhatIsMissingWebhooksIncluded(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.withJobs(t, map[string]string{
		"deploy.yml":   "name: deploy\nsecrets: [TOKEN, SSH_KEY]\nstages:\n  s: {run: echo}\n",
		"site-web.yml": "name: site-web\nsecrets: [TOKEN]\ntriggers: {webhook: {repo: o/site}}\nstages:\n  s: {run: echo}\n",
	})
	sealed, err := secretbox.Seal(testSecretKey, "TOKEN", []byte("an obvious test value"))
	must(t, err)
	must(t, e.st.PutSecret(ctx, "TOKEN", sealed))
	must(t, e.st.PutSecret(ctx, "UNUSED", sealed))
	v, err := e.api.svc.Secrets(ctx, e.user(t, "admin"))
	must(t, err)
	if len(v.Set) != 2 || v.Set[0].Name != "TOKEN" || !slices.Equal(v.Set[0].Jobs, []string{"deploy", "site-web"}) || v.Set[0].UpdatedAt == nil ||
		v.Set[1].Name != "UNUSED" || len(v.Set[1].Jobs) != 0 {
		t.Fatalf("set: %+v", v.Set)
	}
	if len(v.Missing) != 2 || v.Missing[0].Name != "GITHUB_WEBHOOK_SITE_WEB" || !v.Missing[0].Webhook || !slices.Equal(v.Missing[0].Jobs, []string{"site-web"}) ||
		v.Missing[1].Name != "SSH_KEY" || v.Missing[1].Webhook || v.Missing[1].UpdatedAt != nil {
		t.Fatalf("missing: %+v", v.Missing)
	}
}

func TestServerInfoIsWhatServeSaysWithTheJobsLoaded(t *testing.T) {
	e := newEnv(t)
	info, err := e.api.svc.ServerInfo(context.Background(), e.user(t, "admin"))
	must(t, err)
	if info.Version != "test" || info.Listen != "127.0.0.1:8085" || info.Executors != 4 || info.Jobs != len(jobFiles) {
		t.Fatalf("%+v", info)
	}
}

func TestTheAuditIsReadNewestFirstPageByPage(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for i := range 5 {
		must(t, e.st.AppendAudit(ctx, store.Audit{At: time.Now(), Actor: "cli", Action: "user.add", Target: "u" + itoa(int64(i))}))
	}
	admin := e.user(t, "admin")
	first, _, err := e.api.svc.Audit(ctx, admin, 0, 2)
	must(t, err)
	eq(t, "first page", first[0].Target+","+first[1].Target, "u4,u3")
	next, _, err := e.api.svc.Audit(ctx, admin, first[1].ID, 0)
	must(t, err)
	eq(t, "next page", len(next), 3)
}

// Whatever went wrong or was recorded on the way, a password, a token or
// a secret's value is in none of it.
func TestNoAuditRowOrErrorHoldsAPasswordATokenOrASecretValue(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	svc := e.api.svc
	root, alice := e.root(t), e.person(t, "alice", false)
	e.sudo(t, alice)
	const secret = "an obvious test secret value"
	const fresh = "a new obvious test passphrase"
	var errs []error
	keep := func(err error) { errs = append(errs, err) }
	values := []string{secret, fresh, root.pw, alice.pw, "a wrong obvious password"}

	keep(svc.SetSecret(ctx, root.u, root.sess, "TOKEN", secret))
	keep(svc.SetSecret(ctx, root.u, root.sess, "TOKEN", secret+"\nmore"))
	keep(svc.SetSecret(ctx, root.u, root.sess, "NOT-A-NAME", secret))
	keep(svc.SetSecret(ctx, alice.u, alice.sess, "TOKEN", secret))
	keep(svc.SetSecret(ctx, root.u, nil, "TOKEN", secret))
	keep(svc.Sudo(ctx, alice.u, alice.sess, "a wrong obvious password", here))
	_, err := svc.ChangePassword(ctx, alice.u, alice.sess, "a wrong obvious password", fresh, fresh, here)
	keep(err)
	_, err = svc.ChangePassword(ctx, alice.u, alice.sess, alice.pw, fresh, fresh+"x", here)
	keep(err)
	next, err := svc.ChangePassword(ctx, alice.u, alice.sess, alice.pw, fresh, fresh, here)
	keep(err)
	if next != nil {
		alice.sess = next
	}
	plain, _, err := svc.CreateOwnToken(ctx, alice.u, alice.sess, "laptop", 30)
	keep(err)
	values = append(values, plain)
	for _, who := range []string{"agent", "nobody"} {
		pw, err := svc.ResetPassword(ctx, root.u, root.sess, who, true)
		keep(err)
		if pw != "" {
			values = append(values, pw)
		}
		tok, _, err := svc.CreateToken(ctx, root.u, root.sess, who, "ci", 0)
		keep(err)
		if tok != "" {
			values = append(values, tok)
		}
	}
	pw, err := svc.AddUser(ctx, root.u, root.sess, "carol", false)
	keep(err)
	values = append(values, pw)
	keep(svc.DeleteSecret(ctx, root.u, root.sess, "TOKEN"))

	rows := e.audit(t)
	if len(rows) != 7 {
		t.Fatalf("%d audit rows, want 7: the calls above did not all run", len(rows))
	}
	noneHolds(t, said(rows, errs), values)
}

// said is every word of the audit rows and the errors.
func said(rows []*store.Audit, errs []error) []string {
	var out []string
	for _, a := range rows {
		out = append(out, a.Actor, a.Action, a.Target, a.Detail)
	}
	for _, err := range errs {
		if err != nil {
			out = append(out, err.Error())
		}
	}
	return out
}

func noneHolds(t *testing.T, said, values []string) {
	t.Helper()
	for _, v := range values {
		for _, s := range said {
			if strings.Contains(s, v) {
				t.Errorf("%q holds a password, token or secret value", s)
			}
		}
	}
}

// A reset password or a changed one ends the user's devices: a browser
// that logged in with the old one is counted with strangers again.
func TestAResetOrAChangedPasswordStartsANewDeviceEpoch(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	root := e.root(t)
	e.person(t, "alice", false)
	epoch := func() int64 {
		t.Helper()
		u, err := e.st.UserByName(ctx, "alice")
		must(t, err)
		return u.DeviceEpoch
	}
	pw, err := e.api.svc.ResetPassword(ctx, root.u, root.sess, "alice", false)
	must(t, err)
	eq(t, "the epoch after a reset", epoch(), int64(1))
	sess := e.login(t, "alice", pw)
	u, err := e.st.UserByName(ctx, "alice")
	must(t, err)
	_, err = e.api.svc.ChangePassword(ctx, u, sess, pw, "a new obvious test passphrase", "a new obvious test passphrase", here)
	must(t, err)
	eq(t, "the epoch after a change", epoch(), int64(2))
}

func TestQuietIsAnAdminsAndRefusesATriggerWith503AndAMessage(t *testing.T) {
	e := newEnv(t)
	e.call(t, 403, e.viewer, "GET", "/api/v1/admin/quiet", "")
	e.call(t, 403, e.viewer, "POST", "/api/v1/admin/quiet", "")
	e.call(t, 403, e.viewer, "DELETE", "/api/v1/admin/quiet", "")
	off := e.object(t, 200, e.admin, "GET", "/api/v1/admin/quiet", "")
	eq(t, "quiet", off["quiet"], false)
	eq(t, "idle", off["idle"], true)

	on := e.object(t, 200, e.admin, "POST", "/api/v1/admin/quiet", "")
	eq(t, "quiet", on["quiet"], true)
	if on["since"] == nil {
		t.Fatal("no since while quiet")
	}
	eq(t, "audit", e.lastAudit(t), "admin quiet : on")

	text, hdr := e.call(t, 503, e.admin, "POST", "/api/v1/jobs/smoke/builds", "")
	contains(t, text, "kipp is quiet: restarting once running builds end")
	eq(t, "Retry-After", hdr.Get("Retry-After"), "60")

	e.object(t, 200, e.admin, "DELETE", "/api/v1/admin/quiet", "")
	eq(t, "audit", e.lastAudit(t), "admin quiet : off")
	e.finished(t, e.trigger(t, "smoke", ""))
}

func TestAFileAndAUserAndPasswordAreSealedAuditedByKindAndListedWithTheirKind(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	root := e.root(t)
	key := []byte("\x00\xff the bytes of a key")
	must(t, e.api.svc.SetSecretOf(ctx, root.u, root.sess, SecretInput{Name: "KEYFILE", Kind: "file", Data: key}))
	eq(t, "audit", e.lastAudit(t), "root secret.set KEYFILE: new file")
	must(t, e.api.svc.SetSecretOf(ctx, root.u, root.sess, SecretInput{Name: "LOGIN", Kind: "userpass", User: "deployer", Value: "a password long enough"}))
	eq(t, "audit", e.lastAudit(t), "root secret.set LOGIN: new userpass")

	sealed, err := e.st.Secret(ctx, "KEYFILE")
	must(t, err)
	opened, err := secretbox.Open(testSecretKey, "KEYFILE", sealed)
	must(t, err)
	eq(t, "the file's bytes", string(opened), string(key))

	all, err := e.api.svc.Secrets(ctx, root.u)
	must(t, err)
	var got []string
	for _, r := range all.Set {
		got = append(got, r.Name+" "+r.Kind+" "+r.User)
	}
	eq(t, "rows", strings.Join(got, "|"), "KEYFILE file |LOGIN userpass deployer")

	for what, in := range map[string]SecretInput{
		"an empty file":          {Name: "F", Kind: "file"},
		"a user and no pass":     {Name: "U", Kind: "userpass", User: "deployer", Value: "short"},
		"a password and no user": {Name: "U", Kind: "userpass", Value: "a password long enough"},
		"an unknown kind":        {Name: "K", Kind: "cert", Value: "a value long enough"},
	} {
		err := e.api.svc.SetSecretOf(ctx, root.u, root.sess, in)
		is(t, what, err, ErrBadInput)
		if strings.Contains(err.Error(), "a password long enough") {
			t.Errorf("%s: the error holds the value: %v", what, err)
		}
	}
}

func TestAScopeIsSetAuditedAndRefusedWhenItIsNotOneAndNeverTouchesTheValue(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	root := e.root(t)
	must(t, e.api.svc.SetSecret(ctx, root.u, root.sess, "TOKEN", "an obvious test value"))
	rec, err := e.st.SecretRecord(ctx, "TOKEN")
	must(t, err)
	eq(t, "a new secret is global", strings.Join(rec.Scopes, ","), "global")
	must(t, e.api.svc.SetSecretScopes(ctx, root.u, root.sess, "TOKEN", []string{"group:site", "job:acme-*"}))
	eq(t, "audit", e.lastAudit(t), "root secret.scope TOKEN: group:site,job:acme-*")
	must(t, e.api.svc.SetSecretOf(ctx, root.u, root.sess, SecretInput{Name: "TOKEN", Kind: config.SecretText, Value: "another obvious value"}))
	eq(t, "audit", e.lastAudit(t), "root secret.set TOKEN: replaced text")
	after, err := e.st.SecretRecord(ctx, "TOKEN")
	must(t, err)
	eq(t, "a value set without scopes keeps them", strings.Join(after.Scopes, ","), "group:site,job:acme-*")
	must(t, e.api.svc.SetSecretOf(ctx, root.u, root.sess, SecretInput{Name: "TOKEN", Kind: config.SecretText, Value: "another obvious value", Scopes: []string{"global"}}))
	eq(t, "audit", e.lastAudit(t), "root secret.set TOKEN: replaced text scopes global")
	for what, scopes := range map[string][]string{"none": nil, "unknown": {"everyone"}, "bad pattern": {"job:["}} {
		is(t, what, e.api.svc.SetSecretScopes(ctx, root.u, root.sess, "TOKEN", scopes), ErrBadInput)
	}
	is(t, "unset", e.api.svc.SetSecretScopes(ctx, root.u, root.sess, "NONE", []string{"global"}), ErrNotFound)
	is(t, "a viewer", e.api.svc.SetSecretScopes(ctx, e.user(t, "viewer"), nil, "TOKEN", []string{"global"}), ErrForbidden)
}

func TestTheSecretsReadListsWhatAScopeKeepsFromAJobAndHintsAtNarrowingAGlobalOne(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.withJobs(t, map[string]string{
		"deploy.yml":   "name: deploy\ngroup: ops\nsecrets: [KEY, SHARED, LONE]\nstages:\n  s: {run: echo}\n",
		"site-web.yml": "name: site-web\nsecrets: [KEY, SHARED]\nstages:\n  s: {run: echo}\n",
	})
	sealed, err := secretbox.Seal(testSecretKey, "KEY", []byte("an obvious test value"))
	must(t, err)
	for _, name := range []string{"KEY", "SHARED", "LONE"} {
		must(t, e.st.PutSecret(ctx, name, sealed))
	}
	must(t, e.st.SetSecretScopes(ctx, "KEY", []string{"group:ops"}))
	v, err := e.api.svc.Secrets(ctx, e.user(t, "admin"))
	must(t, err)
	if len(v.OutOfScope) != 1 || v.OutOfScope[0].Name != "KEY" || !slices.Equal(v.OutOfScope[0].Jobs, []string{"site-web"}) ||
		!slices.Equal(v.OutOfScope[0].Scopes, []string{"group:ops"}) {
		t.Fatalf("out of scope: %+v", v.OutOfScope)
	}
	narrow := map[string]bool{}
	for _, row := range v.Set {
		narrow[row.Name] = row.Narrow
	}
	if narrow["KEY"] || narrow["SHARED"] || !narrow["LONE"] {
		t.Fatalf("only a global secret one job lists is marked: %v", narrow)
	}
}
