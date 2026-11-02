package api

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/store"
)

// person is a user with a password, logged in from the server's own
// machine: what the account and admin calls are made as.
type person struct {
	u    *store.User
	sess *store.Session
	pw   string
}

// here is an attempt from the server's own machine, which strangers'
// wrong passwords never throttle.
var here = auth.Attempt{Local: true, Peer: "127.0.0.1:5000"}

func (e *env) person(t *testing.T, name string, admin bool) *person {
	t.Helper()
	ctx := context.Background()
	pw := "an obvious test password of " + name
	hash, err := auth.HashPassword(pw)
	must(t, err)
	u, err := e.st.CreateUser(ctx, name, hash, admin)
	must(t, err)
	return &person{u: u, sess: e.login(t, name, pw), pw: pw}
}

func (e *env) login(t *testing.T, name, pw string) *store.Session {
	t.Helper()
	sess, _, err := e.api.deps.Auth.Login(context.Background(), name, pw, here)
	must(t, err)
	return sess
}

// sudo puts p's session in sudo mode.
func (e *env) sudo(t *testing.T, p *person) {
	t.Helper()
	must(t, e.api.svc.Sudo(context.Background(), p.u, p.sess, p.pw, here))
}

func is(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: got %v, want %v", what, err, want)
	}
}

// audit is every audit row, oldest first.
func (e *env) audit(t *testing.T) []*store.Audit {
	t.Helper()
	rows, err := e.st.AuditBefore(context.Background(), 0, 1000)
	must(t, err)
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows
}

// lastAudit is the newest audit row as "actor action target: detail".
func (e *env) lastAudit(t *testing.T) string {
	t.Helper()
	rows := e.audit(t)
	if len(rows) == 0 {
		t.Fatal("no audit row")
	}
	a := rows[len(rows)-1]
	return a.Actor + " " + a.Action + " " + a.Target + ": " + a.Detail
}

func TestSudoTakesTheOwnPasswordAndLastsTenMinutes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.person(t, "alice", false)
	err := e.api.svc.Sudo(ctx, alice.u, alice.sess, "not the password", here)
	is(t, "a wrong password", err, ErrWrongPassword)
	is(t, "a wrong password", err, ErrBadInput)
	if alice.sess.SudoUntil != nil {
		t.Fatal("a wrong password gave sudo")
	}
	e.sudo(t, alice)
	if until := alice.sess.SudoUntil; until == nil || time.Until(*until) < 9*time.Minute || time.Until(*until) > 10*time.Minute {
		t.Fatalf("sudo until %v", until)
	}
	stored, err := e.st.Session(ctx, alice.sess.ID)
	must(t, err)
	if stored.SudoUntil == nil {
		t.Fatal("sudo was not stored with the session")
	}
}

func TestSudoIsNeverGivenToAToken(t *testing.T) {
	e := newEnv(t)
	alice := e.person(t, "alice", false)
	is(t, "a token", e.api.svc.Sudo(context.Background(), alice.u, nil, alice.pw, here), ErrForbidden)
}

func TestAWrongPasswordForSudoCountsAsAFailedLogin(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.person(t, "alice", false)
	stranger := auth.Attempt{Peer: "192.0.2.1:5000"}
	var err error
	for range 6 {
		err = e.api.svc.Sudo(ctx, alice.u, alice.sess, "not the password", stranger)
	}
	var throttled *auth.ThrottledError
	if !errors.As(err, &throttled) {
		t.Fatalf("the sixth wrong password: %v", err)
	}
	if !errors.As(e.api.svc.Sudo(ctx, alice.u, alice.sess, alice.pw, stranger), &throttled) {
		t.Fatal("the right password was checked while the name waits")
	}
}

// calls are every change to credentials or secrets, each as the user and
// session given; admin ones on bob.
func credentialCalls(e *env, bob *person) map[string]func(u *store.User, sess *store.Session) error {
	ctx := context.Background()
	svc := e.api.svc
	return map[string]func(u *store.User, sess *store.Session) error{
		"CreateOwnToken": func(u *store.User, sess *store.Session) error {
			_, _, err := svc.CreateOwnToken(ctx, u, sess, "ci", 0)
			return err
		},
		"RevokeOwnToken": func(u *store.User, sess *store.Session) error { return svc.RevokeOwnToken(ctx, u, sess, 1) },
		"AddUser": func(u *store.User, sess *store.Session) error {
			_, err := svc.AddUser(ctx, u, sess, "carol", false)
			return err
		},
		"SetAdmin": func(u *store.User, sess *store.Session) error { return svc.SetAdmin(ctx, u, sess, bob.u.Name, true) },
		"ResetPassword": func(u *store.User, sess *store.Session) error {
			_, err := svc.ResetPassword(ctx, u, sess, bob.u.Name, false)
			return err
		},
		"DeleteUser": func(u *store.User, sess *store.Session) error { return svc.DeleteUser(ctx, u, sess, bob.u.Name) },
		"SetGrants": func(u *store.User, sess *store.Session) error {
			return svc.SetGrants(ctx, u, sess, bob.u.Name, map[string]store.Perm{"smoke": {View: true}})
		},
		"RemoveGoneGrant": func(u *store.User, sess *store.Session) error {
			return svc.RemoveGoneGrant(ctx, u, sess, bob.u.Name, "gone")
		},
		"CreateToken": func(u *store.User, sess *store.Session) error {
			_, _, err := svc.CreateToken(ctx, u, sess, bob.u.Name, "ci", 0)
			return err
		},
		"RevokeToken": func(u *store.User, sess *store.Session) error { return svc.RevokeToken(ctx, u, sess, 1) },
		"SetSecret": func(u *store.User, sess *store.Session) error {
			return svc.SetSecret(ctx, u, sess, "TOKEN", "an obvious test secret value")
		},
		"DeleteSecret": func(u *store.User, sess *store.Session) error { return svc.DeleteSecret(ctx, u, sess, "TOKEN") },
	}
}

func TestATokenNeverChangesCredentialsOrSecretsAdminOrNot(t *testing.T) {
	e := newEnv(t)
	root, bob := e.person(t, "root", true), e.person(t, "bob", false)
	for name, call := range credentialCalls(e, bob) {
		is(t, name, call(root.u, nil), ErrForbidden)
	}
	_, err := e.api.svc.ChangePassword(context.Background(), root.u, nil, root.pw, "a new obvious test passphrase", "a new obvious test passphrase", here)
	is(t, "ChangePassword", err, ErrForbidden)
	eq(t, "audit rows", len(e.audit(t)), 0)
}

func TestASessionChangesCredentialsOrSecretsOnlyInSudoMode(t *testing.T) {
	e := newEnv(t)
	root, bob := e.person(t, "root", true), e.person(t, "bob", false)
	expired := *root.sess
	expired.SudoUntil = new(time.Now().Add(-time.Second))
	for name, call := range credentialCalls(e, bob) {
		is(t, name+" without sudo", call(root.u, root.sess), ErrSudoRequired)
		is(t, name+" with sudo that has ended", call(root.u, &expired), ErrSudoRequired)
	}
	eq(t, "audit rows", len(e.audit(t)), 0)
}

func TestAnAdminCallIsRefusedToANonAdminEvenInSudoMode(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice, bob := e.person(t, "alice", false), e.person(t, "bob", false)
	e.sudo(t, alice)
	for name, call := range credentialCalls(e, bob) {
		if strings.Contains(name, "Own") {
			continue
		}
		is(t, name, call(alice.u, alice.sess), ErrForbidden)
	}
	svc := e.api.svc
	_, err := svc.Users(ctx, alice.u)
	is(t, "Users", err, ErrForbidden)
	_, err = svc.User(ctx, alice.u, "bob")
	is(t, "User", err, ErrForbidden)
	_, err = svc.Secrets(ctx, alice.u)
	is(t, "Secrets", err, ErrForbidden)
	_, err = svc.ServerInfo(ctx, alice.u)
	is(t, "ServerInfo", err, ErrForbidden)
	_, _, err = svc.Audit(ctx, alice.u, 0, 0)
	is(t, "Audit", err, ErrForbidden)
	is(t, "Reload", svc.Reload(ctx, alice.u), ErrForbidden)
	eq(t, "audit rows", len(e.audit(t)), 0)
}

func TestChangingThePasswordReplacesTheSessionAndEndsTheOthers(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.person(t, "alice", false)
	e.sudo(t, alice)
	other := e.login(t, "alice", alice.pw)
	bob := e.person(t, "bob", false)
	next, err := e.api.svc.ChangePassword(ctx, alice.u, alice.sess, alice.pw, "a new obvious test passphrase", "a new obvious test passphrase", here)
	must(t, err)
	if next.ID == alice.sess.ID || next.CSRF == alice.sess.CSRF {
		t.Fatal("the session was not replaced")
	}
	for what, id := range map[string]string{"the old session": alice.sess.ID, "another session": other.ID} {
		_, err := e.st.Session(ctx, id)
		is(t, what, err, store.ErrNotFound)
	}
	kept, err := e.st.Session(ctx, next.ID)
	must(t, err)
	if kept.UserID != alice.u.ID || kept.SudoUntil == nil {
		t.Fatalf("the new session: %+v", kept)
	}
	if _, err := e.st.Session(ctx, bob.sess.ID); err != nil {
		t.Fatalf("another user's session ended: %v", err)
	}
	e.login(t, "alice", "a new obvious test passphrase")
	eq(t, "audit", e.lastAudit(t), "alice password.change alice: the other sessions ended")
}

// The change is one transaction: a session that ended while the password
// was being checked leaves the old password, the other sessions and the
// devices as they were, and no audit row says otherwise.
func TestAPasswordChangeOnASessionThatEndedMeanwhileChangesNothing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.person(t, "alice", false)
	other := e.login(t, "alice", alice.pw)
	rows := len(e.audit(t))
	must(t, e.st.DeleteSession(ctx, alice.sess.ID))
	_, err := e.api.svc.ChangePassword(ctx, alice.u, alice.sess, alice.pw, "a new obvious test passphrase", "a new obvious test passphrase", here)
	is(t, "the change", err, ErrNotFound)
	if _, err := e.st.Session(ctx, other.ID); err != nil {
		t.Fatalf("another session ended: %v", err)
	}
	u, err := e.st.UserByName(ctx, "alice")
	must(t, err)
	eq(t, "the device epoch", u.DeviceEpoch, int64(0))
	e.login(t, "alice", alice.pw)
	eq(t, "audit rows", len(e.audit(t)), rows)
}

func TestAPasswordChangeIsRefusedForAWrongCurrentOneABadNewOneOrTwoThatDiffer(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.person(t, "alice", false)
	for what, c := range map[string]struct {
		current, next, again, says string
		want                       error
	}{
		"a wrong current one": {"not the password", "a new obvious test passphrase", "a new obvious test passphrase", "wrong password", ErrWrongPassword},
		"two that differ":     {alice.pw, "a new obvious test passphrase", "another obvious test passphrase", "the two new passwords differ", ErrBadInput},
		"a short one":         {alice.pw, "too short", "too short", "at least 15 characters", ErrBadInput},
		"one with the name":   {alice.pw, "alice obvious test passphrase", "alice obvious test passphrase", `may not contain "alice"`, ErrBadInput},
	} {
		_, err := e.api.svc.ChangePassword(ctx, alice.u, alice.sess, c.current, c.next, c.again, here)
		is(t, what, err, c.want)
		contains(t, err.Error(), c.says)
	}
	if _, err := e.st.Session(ctx, alice.sess.ID); err != nil {
		t.Fatalf("a refused change ended the session: %v", err)
	}
	e.login(t, "alice", alice.pw)
	eq(t, "audit rows", len(e.audit(t)), 0)
}

func TestAnOwnTokenIsShownOnceKeptAsAHashAndExpiresWhenAsked(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.person(t, "alice", false)
	e.sudo(t, alice)
	plain, tok, err := e.api.svc.CreateOwnToken(ctx, alice.u, alice.sess, "  laptop  ", 30)
	must(t, err)
	eq(t, "label", tok.Label, "laptop")
	if tok.ExpiresAt == nil || time.Until(*tok.ExpiresAt) < 29*24*time.Hour || time.Until(*tok.ExpiresAt) > 30*24*time.Hour {
		t.Fatalf("expires %v", tok.ExpiresAt)
	}
	u, err := e.api.deps.Auth.UserFromToken(ctx, plain)
	must(t, err)
	eq(t, "the token's user", u.Name, "alice")
	_, never, err := e.api.svc.CreateOwnToken(ctx, alice.u, alice.sess, "ci", 0)
	must(t, err)
	if never.ExpiresAt != nil {
		t.Fatal("a token asked to last for good expires")
	}
	eq(t, "audit", e.lastAudit(t), "alice token.create alice: label \"ci\", id "+itoa(never.ID)+", never expires")
	tokens, err := e.api.svc.OwnTokens(ctx, alice.u)
	must(t, err)
	eq(t, "own tokens", len(tokens), 2)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestATokenLabelIsOneToAHundredCharactersWithoutControlCharacters(t *testing.T) {
	e := newEnv(t)
	alice := e.person(t, "alice", false)
	e.sudo(t, alice)
	for label, ok := range map[string]bool{
		"":                                 false,
		"   ":                              false,
		strings.Repeat("ä", 100):           true,
		strings.Repeat("a", 101):           false,
		"tab\there":                        false,
		"bell\a":                           false,
		"line\nbreak":                      false,
		"\xff not utf-8":                   false,
		"agent on the build box (deploys)": true,
	} {
		_, _, err := e.api.svc.CreateOwnToken(context.Background(), alice.u, alice.sess, label, 0)
		if ok && err != nil {
			t.Errorf("%q: %v", label, err)
		}
		if !ok && !errors.Is(err, ErrBadInput) {
			t.Errorf("%q: got %v, want ErrBadInput", label, err)
		}
	}
}

func TestATokenLastsForGoodOr30Or90Or365DaysAndNothingElse(t *testing.T) {
	e := newEnv(t)
	alice := e.person(t, "alice", false)
	e.sudo(t, alice)
	for _, days := range []int{0, 30, 90, 365} {
		_, _, err := e.api.svc.CreateOwnToken(context.Background(), alice.u, alice.sess, "t", days)
		must(t, err)
	}
	for _, days := range []int{-1, 1, 7, 366, 3650} {
		_, _, err := e.api.svc.CreateOwnToken(context.Background(), alice.u, alice.sess, "t", days)
		is(t, "days "+itoa(int64(days)), err, ErrBadInput)
	}
}

func TestTheFiftyFirstTokenOfAUserIsRefused(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.person(t, "alice", false)
	e.sudo(t, alice)
	for range MaxTokens {
		_, hash, err := auth.NewToken()
		must(t, err)
		_, err = e.st.CreateToken(ctx, alice.u.ID, "t", hash)
		must(t, err)
	}
	_, _, err := e.api.svc.CreateOwnToken(ctx, alice.u, alice.sess, "one more", 0)
	is(t, "the 51st", err, ErrConflict)
	contains(t, err.Error(), "at most 50 tokens")
}

func TestAUserRevokesTheirOwnTokenAndAnotherUsersIsNotFound(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice, bob := e.person(t, "alice", false), e.person(t, "bob", false)
	e.sudo(t, alice)
	e.sudo(t, bob)
	_, mine, err := e.api.svc.CreateOwnToken(ctx, alice.u, alice.sess, "mine", 0)
	must(t, err)
	_, theirs, err := e.api.svc.CreateOwnToken(ctx, bob.u, bob.sess, "theirs", 0)
	must(t, err)
	is(t, "bob's token", e.api.svc.RevokeOwnToken(ctx, alice.u, alice.sess, theirs.ID), ErrNotFound)
	is(t, "no token", e.api.svc.RevokeOwnToken(ctx, alice.u, alice.sess, 99999), ErrNotFound)
	must(t, e.api.svc.RevokeOwnToken(ctx, alice.u, alice.sess, mine.ID))
	_, err = e.st.Token(ctx, mine.ID)
	is(t, "revoked", err, store.ErrNotFound)
	if _, err := e.st.Token(ctx, theirs.ID); err != nil {
		t.Fatalf("bob's token went: %v", err)
	}
	eq(t, "audit", e.lastAudit(t), "alice token.revoke alice: label \"mine\", id "+itoa(mine.ID))
}

func TestEndingTheOtherSessionsKeepsThisOneAndNeedsNoSudo(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.person(t, "alice", false)
	e.login(t, "alice", alice.pw)
	e.login(t, "alice", alice.pw)
	n, err := e.api.svc.CountOtherSessions(ctx, alice.u, alice.sess)
	must(t, err)
	eq(t, "other sessions", n, 2)
	must(t, e.api.svc.EndOtherSessions(ctx, alice.u, alice.sess))
	n, err = e.api.svc.CountOtherSessions(ctx, alice.u, alice.sess)
	must(t, err)
	eq(t, "other sessions after", n, 0)
	if _, err := e.st.Session(ctx, alice.sess.ID); err != nil {
		t.Fatalf("this session ended: %v", err)
	}
	eq(t, "audit", e.lastAudit(t), "alice sessions.end alice: 2 other sessions")
}

func TestATokenEndsEverySessionOfItsUser(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.person(t, "alice", false)
	n, err := e.api.svc.CountOtherSessions(ctx, alice.u, nil)
	must(t, err)
	eq(t, "sessions a token sees", n, 1)
	must(t, e.api.svc.EndOtherSessions(ctx, alice.u, nil))
	_, err = e.st.Session(ctx, alice.sess.ID)
	is(t, "the session", err, store.ErrNotFound)
}
