package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/store"
)

// audit is every audit row of the data directory, oldest first, as
// "actor action target: detail".
func audit(t *testing.T, st *store.Store) []string {
	t.Helper()
	rows, err := st.AuditBefore(context.Background(), 0, 1000)
	must(t, err)
	out := make([]string, len(rows))
	for i, a := range rows {
		out[len(rows)-1-i] = a.Actor + " " + a.Action + " " + a.Target + ": " + a.Detail
	}
	return out
}

func TestEveryChangeTheCLIMakesIsAuditedAsCliAndNeverWithAValue(t *testing.T) {
	data := newData(t)
	const secret = "an obvious test secret value"
	pw := strings.TrimPrefix(strings.TrimSpace(ok(t, "", "user", "add", "root", "--admin", "--data", data)), "root password: ")
	ok(t, "a password long enough\n", "user", "add", "agent", "--password-stdin", "--data", data)
	ok(t, "", "user", "admin", "agent", "on", "--data", data)
	ok(t, "", "user", "admin", "agent", "off", "--data", data)
	ok(t, "another password long enough\n", "user", "passwd", "agent", "--password-stdin", "--data", data)
	ok(t, "", "user", "grant", "agent", "smoke", "build,view", "--data", data)
	ok(t, "", "user", "revoke", "agent", "smoke", "--data", data)
	out := ok(t, "", "token", "create", "agent", "--label", "ci", "--expires", "30", "--data", data)
	token := strings.TrimSpace(out[strings.LastIndex(out, "kipp_"):])
	ok(t, "", "token", "revoke", "1", "--data", data)
	ok(t, secret, "secret", "set", "TOKEN", "--data", data)
	ok(t, secret, "secret", "set", "TOKEN", "--data", data)
	ok(t, "", "secret", "unset", "TOKEN", "--data", data)
	ok(t, "", "user", "delete", "agent", "--data", data)

	expires := time.Now().AddDate(0, 0, 30).UTC().Format(time.DateOnly)
	want := []string{
		"cli user.add root: admin",
		"cli user.add agent: user",
		"cli user.admin agent: on",
		"cli user.admin agent: off",
		"cli user.password.reset agent: sessions ended",
		"cli grants.set agent: smoke: view,build",
		"cli grants.set agent: smoke: none",
		`cli token.create agent: label "ci", id 1, expires ` + expires,
		`cli token.revoke agent: label "ci", id 1`,
		"cli secret.set TOKEN: new text",
		"cli secret.set TOKEN: replaced text",
		"cli secret.delete TOKEN: ",
		"cli user.delete agent: ",
	}
	got := audit(t, openDB(t, data))
	eq(t, "the audit", strings.Join(got, "\n"), strings.Join(want, "\n"))
	lacks(t, strings.Join(got, "\n"), secret, pw, token, "long enough")
}

func TestARefusedChangeIsNotAudited(t *testing.T) {
	data := newData(t)
	ok(t, "", "user", "add", "root", "--admin", "--data", data)
	for _, args := range [][]string{
		{"user", "admin", "root", "off"},
		{"user", "delete", "root"},
		{"user", "add", "root"},
		{"secret", "unset", "NONE"},
		{"token", "revoke", "7"},
	} {
		if code, _, _ := kipp("", append(args, "--data", data)...); code == 0 {
			t.Fatalf("%v was taken", args)
		}
	}
	eq(t, "the audit", strings.Join(audit(t, openDB(t, data)), "\n"), "cli user.add root: admin")
}

// session gives name a live browser session.
func session(t *testing.T, st *store.Store, name, id string) {
	t.Helper()
	u, err := st.UserByName(context.Background(), name)
	must(t, err)
	must(t, st.CreateSession(context.Background(), &store.Session{ID: id, UserID: u.ID, CSRF: "c", ExpiresAt: time.Now().Add(time.Hour)}))
}

func sessions(t *testing.T, st *store.Store, name string) int {
	t.Helper()
	u, err := st.UserByName(context.Background(), name)
	must(t, err)
	n, err := st.CountSessions(context.Background(), u.ID)
	must(t, err)
	return n
}

// A deploy script sets the same password on every run; that must not log
// its owner out.
func TestPasswdEndsTheSessionsOnlyWhenThePasswordChanges(t *testing.T) {
	data := newData(t)
	const pw = "the same password every run"
	ok(t, pw+"\n", "user", "add", "owner", "--admin", "--password-stdin", "--data", data)
	st := openDB(t, data)
	session(t, st, "owner", "an obvious test session id one")

	eq(t, "the same password", ok(t, pw+"\n", "user", "passwd", "owner", "--password-stdin", "--data", data), "owner: password unchanged\n")
	eq(t, "sessions after the same password", sessions(t, st, "owner"), 1)
	eq(t, "rows after the same password", len(audit(t, st)), 1)

	ok(t, "a different password now\n", "user", "passwd", "owner", "--password-stdin", "--data", data)
	eq(t, "sessions after a new password", sessions(t, st, "owner"), 0)

	session(t, st, "owner", "an obvious test session id two")
	ok(t, "", "user", "passwd", "owner", "--data", data)
	eq(t, "sessions after a generated password", sessions(t, st, "owner"), 0)
}

// A new password ends the owner's devices; the same one given again by a
// deploy script does not.
func TestPasswdStartsANewDeviceEpochOnlyWhenThePasswordChanges(t *testing.T) {
	data := newData(t)
	const pw = "the same password every run"
	ok(t, pw+"\n", "user", "add", "owner", "--admin", "--password-stdin", "--data", data)
	st := openDB(t, data)
	epoch := func() int64 {
		t.Helper()
		u, err := st.UserByName(context.Background(), "owner")
		must(t, err)
		return u.DeviceEpoch
	}
	ok(t, pw+"\n", "user", "passwd", "owner", "--password-stdin", "--data", data)
	eq(t, "the epoch after the same password", epoch(), int64(0))
	ok(t, "a different password now\n", "user", "passwd", "owner", "--password-stdin", "--data", data)
	eq(t, "the epoch after a new password", epoch(), int64(1))
}

// A password stored before the rule for a chosen one still logs in, so
// giving it again is not refused by that rule: it sets nothing.
func TestTheSamePasswordIsUnchangedEvenWhenTheRuleWouldRefuseItNow(t *testing.T) {
	data := newData(t)
	st := openDB(t, data)
	hash, err := auth.HashPassword("short")
	must(t, err)
	_, err = st.CreateUser(context.Background(), "old", hash, false)
	must(t, err)
	eq(t, "the old password again", ok(t, "short\n", "user", "passwd", "old", "--password-stdin", "--data", data), "old: password unchanged\n")
	code, _, errOut := kipp("other\n", "user", "passwd", "old", "--password-stdin", "--data", data)
	eq(t, "a new password under the rule", code, 2)
	contains(t, errOut, "at least 15 characters")
}

func TestPasswdRevokesTheTokensOnlyWhenAsked(t *testing.T) {
	data := newData(t)
	ok(t, "a password long enough\n", "user", "add", "agent", "--password-stdin", "--data", data)
	ok(t, "", "token", "create", "agent", "--label", "a", "--data", data)
	ok(t, "", "token", "create", "agent", "--label", "b", "--data", data)
	ok(t, "a second password, long\n", "user", "passwd", "agent", "--password-stdin", "--data", data)
	contains(t, ok(t, "", "token", "list", "agent", "--data", data), "  a  ", "  b  ")

	out := ok(t, "a third password, long\n", "user", "passwd", "agent", "--password-stdin", "--revoke-tokens", "--data", data)
	contains(t, out, "agent: password set", "agent: 2 tokens revoked")
	eq(t, "tokens after --revoke-tokens", ok(t, "", "token", "list", "agent", "--data", data), "")
	got := audit(t, openDB(t, data))
	eq(t, "the row", got[len(got)-1], "cli user.password.reset agent: sessions ended, 2 tokens revoked")
}

func TestUserAdminTurnsAdminOnAndOffButNeverOffTheLastOne(t *testing.T) {
	data := newData(t)
	ok(t, "", "user", "add", "root", "--admin", "--data", data)
	ok(t, "", "user", "add", "agent", "--data", data)
	eq(t, "on", ok(t, "", "user", "admin", "agent", "on", "--data", data), "agent: admin\n")
	contains(t, ok(t, "", "user", "list", "--data", data), "agent  admin\n")
	eq(t, "off", ok(t, "", "user", "admin", "root", "off", "--data", data), "root: not an admin\n")
	for what, c := range map[string]struct {
		args []string
		code int
		says string
	}{
		"the last admin":     {[]string{"agent", "off"}, 1, "agent is the last admin"},
		"nobody":             {[]string{"ghost", "on"}, 1, `no user called "ghost"`},
		"neither on nor off": {[]string{"agent", "yes"}, 2, "usage: kipp user"},
		"no state":           {[]string{"agent"}, 2, "usage: kipp user"},
	} {
		code, _, errOut := kipp("", append(append([]string{"user", "admin"}, c.args...), "--data", data)...)
		eq(t, what, code, c.code)
		contains(t, errOut, c.says)
	}
	eq(t, "the list", ok(t, "", "user", "list", "--data", data), "agent  admin\nroot  user\n")
}

func TestATokenExpiresWhenAskedAndTheListSaysWhen(t *testing.T) {
	data := newData(t)
	ok(t, "", "user", "add", "agent", "--data", data)
	for _, days := range []string{"30", "90", "365"} {
		ok(t, "", "token", "create", "agent", "--label", "d"+days, "--expires", days, "--data", data)
	}
	ok(t, "", "token", "create", "agent", "--label", "forever", "--data", data)
	st := openDB(t, data)
	u, err := st.UserByName(context.Background(), "agent")
	must(t, err)
	tokens, err := st.Tokens(context.Background(), u.ID)
	must(t, err)
	if len(tokens) != 4 || tokens[3].ExpiresAt != nil {
		t.Fatalf("tokens: %+v", tokens)
	}
	for i, days := range []int{30, 90, 365} {
		want := time.Now().AddDate(0, 0, days)
		if d := tokens[i].ExpiresAt.Sub(want).Abs(); d > time.Minute {
			t.Fatalf("a token of %d days expires %v", days, tokens[i].ExpiresAt)
		}
	}
	_, hash, err := auth.NewToken()
	must(t, err)
	_, err = st.CreateTokenUntil(context.Background(), u.ID, "old", hash, new(time.Now().Add(-time.Hour)))
	must(t, err)
	list := ok(t, "", "token", "list", "agent", "--data", data)
	contains(t, list,
		"  d30  created ", "  expires "+tokens[0].ExpiresAt.Local().Format(time.DateOnly)+"\n",
		"  forever  created ", "  never expires\n",
		"  old  created ", "  expired "+time.Now().Add(-time.Hour).Local().Format(time.DateOnly)+"\n")
	for _, bad := range []string{"0", "7", "x", "-30"} {
		code, _, errOut := kipp("", "token", "create", "agent", "--label", "x", "--expires", bad, "--data", data)
		eq(t, "--expires "+bad, code, 2)
		contains(t, errOut, "30, 90 or 365")
	}
}

func TestAGrantThatChangesNothingIsNotAudited(t *testing.T) {
	data := newData(t)
	ok(t, "a password long enough\n", "user", "add", "agent", "--password-stdin", "--data", data)
	ok(t, "", "user", "grant", "agent", "smoke", "view,build", "--data", data)
	ok(t, "", "user", "grant", "agent", "smoke", "build,view", "--data", data)
	ok(t, "", "user", "revoke", "agent", "other", "--data", data)
	eq(t, "the audit", strings.Join(audit(t, openDB(t, data)), "\n"),
		"cli user.add agent: user\ncli grants.set agent: smoke: view,build")
}
