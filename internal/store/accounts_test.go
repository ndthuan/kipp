package store

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newSession(t *testing.T, s *Store, id string, userID int64) {
	t.Helper()
	must(t, s.CreateSession(ctx, &Session{ID: id, UserID: userID, CSRF: "csrf-" + id, ExpiresAt: time.Now().Add(time.Hour)}))
}

func newUser(t *testing.T, s *Store, name string, admin bool) *User {
	t.Helper()
	u, err := s.CreateUser(ctx, name, "h", admin)
	must(t, err)
	return u
}

// A copy of the database is no way into the server: what it holds of a
// session is a hash its cookie cannot be made from.
func TestASessionIsKeptByTheHashOfItsIdAndNeverTheId(t *testing.T) {
	s := open(t)
	u := newUser(t, s, "ann", false)
	newSession(t, s, "the-session-id", u.ID)
	var key string
	must(t, s.db.QueryRowContext(ctx, `SELECT hash FROM sessions`).Scan(&key))
	if key == "the-session-id" || key != sessionKey("the-session-id") || len(key) != 64 {
		t.Fatalf("the sessions table holds %q", key)
	}
	got, err := s.Session(ctx, "the-session-id")
	must(t, err)
	if got.ID != "the-session-id" || got.UserID != u.ID || got.CSRF != "csrf-the-session-id" || got.SudoUntil != nil {
		t.Fatalf("%+v", got)
	}
	if _, err := s.Session(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the stored hash works as a session id: %v", err)
	}
	must(t, s.DeleteSession(ctx, "the-session-id"))
	if _, err := s.Session(ctx, "the-session-id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a deleted session resolves: %v", err)
	}
}

func TestSudoIsSetOnALiveSessionAndComesBackWithIt(t *testing.T) {
	s := open(t)
	u := newUser(t, s, "ann", false)
	newSession(t, s, "sid", u.ID)
	until := time.Now().Add(10 * time.Minute)
	must(t, s.SetSessionSudo(ctx, "sid", until))
	got, err := s.Session(ctx, "sid")
	must(t, err)
	if got.SudoUntil == nil || !got.SudoUntil.Equal(until.UTC()) {
		t.Fatalf("sudo until %v, want %v", got.SudoUntil, until)
	}
	if err := s.SetSessionSudo(ctx, "no-such-session", until); !errors.Is(err, ErrNotFound) {
		t.Fatalf("sudo on a session that is not there: %v", err)
	}
	must(t, s.CreateSession(ctx, &Session{ID: "expired", UserID: u.ID, CSRF: "c", ExpiresAt: time.Now().Add(-time.Minute)}))
	if err := s.SetSessionSudo(ctx, "expired", until); !errors.Is(err, ErrNotFound) {
		t.Fatalf("sudo on an expired session: %v", err)
	}
}

func TestAChangedPasswordsSessionTakesTheOldIdsPlaceWithItsUserAndSudo(t *testing.T) {
	s := open(t)
	u := newUser(t, s, "ann", false)
	until := time.Now().Add(10 * time.Minute)
	newSession(t, s, "old", u.ID)
	must(t, s.SetSessionSudo(ctx, "old", until))
	next := &Session{ID: "new", CSRF: "new-csrf", ExpiresAt: time.Now().Add(time.Hour)}
	must(t, s.ChangePassword(ctx, u.ID, "h2", "old", next))
	if _, err := s.Session(ctx, "old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the old id still resolves: %v", err)
	}
	got, err := s.Session(ctx, "new")
	must(t, err)
	if got.UserID != u.ID || got.CSRF != "new-csrf" || next.UserID != u.ID {
		t.Fatalf("%+v, given %+v", got, next)
	}
	if got.SudoUntil == nil || !got.SudoUntil.Equal(until.UTC()) || next.SudoUntil == nil {
		t.Fatalf("the new session's sudo is %v", got.SudoUntil)
	}
	if err := s.ChangePassword(ctx, u.ID, "h3", "old", &Session{ID: "third", CSRF: "c", ExpiresAt: time.Now().Add(time.Hour)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the old id was replaced twice: %v", err)
	}
	if _, err := s.Session(ctx, "third"); !errors.Is(err, ErrNotFound) {
		t.Fatal("a refused replacement stored its session")
	}
}

func TestEndingAUsersOtherSessionsSparesTheOneNamedAndNoOtherUsers(t *testing.T) {
	s := open(t)
	ann, bob := newUser(t, s, "ann", false), newUser(t, s, "bob", false)
	for _, id := range []string{"a1", "a2", "a3"} {
		newSession(t, s, id, ann.ID)
	}
	newSession(t, s, "b1", bob.ID)
	must(t, s.CreateSession(ctx, &Session{ID: "a-expired", UserID: ann.ID, CSRF: "c", ExpiresAt: time.Now().Add(-time.Minute)}))
	count := func(id int64) int {
		t.Helper()
		n, err := s.CountSessions(ctx, id)
		must(t, err)
		return n
	}
	if n := count(ann.ID); n != 3 {
		t.Fatalf("ann has %d live sessions, want 3 (the expired one is not counted)", n)
	}
	must(t, s.DeleteUserSessions(ctx, ann.ID, "a2"))
	if n := count(ann.ID); n != 1 {
		t.Fatalf("ann has %d sessions after ending the others", n)
	}
	if _, err := s.Session(ctx, "a2"); err != nil {
		t.Fatalf("the session spared is gone: %v", err)
	}
	must(t, s.DeleteUserSessions(ctx, ann.ID, ""))
	if n := count(ann.ID); n != 0 {
		t.Fatalf("ann has %d sessions after ending all", n)
	}
	if n := count(bob.ID); n != 1 {
		t.Fatalf("ending ann's sessions ended %d of bob's", 1-n)
	}
}

func TestALoginTimeComesBackWithTheUserEveryWayItIsRead(t *testing.T) {
	s := open(t)
	u := newUser(t, s, "ann", false)
	_, err := s.CreateToken(ctx, u.ID, "ci", "th")
	must(t, err)
	if got, err := s.UserByID(ctx, u.ID); err != nil || got.LastLoginAt != nil {
		t.Fatalf("a user who never logged in: %+v, %v", got, err)
	}
	at := time.Date(2026, 10, 17, 8, 0, 0, 500, time.UTC)
	must(t, s.RecordLogin(ctx, u.ID, at))
	if err := s.RecordLogin(ctx, 999, at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a login of nobody: %v", err)
	}
	byName, err := s.UserByName(ctx, "ann")
	must(t, err)
	byToken, err := s.UserByTokenHash(ctx, "th")
	must(t, err)
	all, err := s.Users(ctx)
	must(t, err)
	for _, got := range []*User{byName, byToken, all[0]} {
		if got.LastLoginAt == nil || !got.LastLoginAt.Equal(at) {
			t.Fatalf("last login %v, want %v", got.LastLoginAt, at)
		}
	}
}

func TestTurningOffTheLastAdminIsRefused(t *testing.T) {
	s := open(t)
	one, two, user := newUser(t, s, "one", true), newUser(t, s, "two", true), newUser(t, s, "user", false)
	must(t, s.SetAdmin(ctx, one.ID, false))
	if err := s.SetAdmin(ctx, two.ID, false); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("the last admin turned off: %v", err)
	}
	if got, _ := s.UserByID(ctx, two.ID); !got.Admin {
		t.Fatal("a refused change was kept")
	}
	must(t, s.SetAdmin(ctx, user.ID, false)) // no admin was taken away
	must(t, s.SetAdmin(ctx, two.ID, true))   // already one
	must(t, s.SetAdmin(ctx, user.ID, true))
	must(t, s.SetAdmin(ctx, two.ID, false))
	if got, _ := s.UserByID(ctx, two.ID); got.Admin {
		t.Fatal("an admin with another beside them was not turned off")
	}
	if err := s.SetAdmin(ctx, 999, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown user: %v", err)
	}
}

func TestDeletingTheLastAdminIsRefused(t *testing.T) {
	s := open(t)
	one, two := newUser(t, s, "one", true), newUser(t, s, "two", true)
	must(t, s.DeleteUser(ctx, one.ID))
	if err := s.DeleteUser(ctx, two.ID); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("the last admin deleted: %v", err)
	}
	if _, err := s.UserByID(ctx, two.ID); err != nil {
		t.Fatalf("a refused delete was kept: %v", err)
	}
	if err := s.DeleteUser(ctx, one.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a user deleted twice: %v", err)
	}

	// A user who is no admin is deleted even where nobody is one.
	bare := open(t)
	u := newUser(t, bare, "user", false)
	must(t, bare.DeleteUser(ctx, u.ID))
}

// The CLI and the server are two processes on one file: each change counts
// the admins in the statement that makes it, so two at once cannot each
// see the other's admin and leave none.
func TestTwoStoresTakingAwayTheTwoAdminsAtOnceLeaveOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	a, err := Open(path)
	must(t, err)
	t.Cleanup(func() { must(t, a.Close()) })
	b, err := OpenCurrent(path)
	must(t, err)
	t.Cleanup(func() { must(t, b.Close()) })
	for round := range 20 {
		one, err := a.CreateUser(ctx, "one", "h", true)
		must(t, err)
		two, err := a.CreateUser(ctx, "two", "h", true)
		must(t, err)
		changes := []func() error{
			func() error { return a.SetAdmin(ctx, one.ID, false) },
			func() error { return b.SetAdmin(ctx, two.ID, false) },
		}
		if round%2 == 1 {
			changes = []func() error{
				func() error { return a.DeleteUser(ctx, one.ID) },
				func() error { return b.DeleteUser(ctx, two.ID) },
			}
		}
		errs := make([]error, len(changes))
		var wg sync.WaitGroup
		for i, change := range changes {
			wg.Go(func() { errs[i] = change() })
		}
		wg.Wait()
		refused := 0
		for _, err := range errs {
			if errors.Is(err, ErrLastAdmin) {
				refused++
			} else if err != nil {
				t.Fatal(err)
			}
		}
		var admins int
		must(t, a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE admin = 1`).Scan(&admins))
		if refused != 1 || admins != 1 {
			t.Fatalf("round %d: %d refused, %d admins left", round, refused, admins)
		}
		_, err = a.db.ExecContext(ctx, `DELETE FROM users`)
		must(t, err)
	}
}

func TestReplacingAUsersGrantsSetsEveryJobAtOnce(t *testing.T) {
	s := open(t)
	u, other := newUser(t, s, "ann", false), newUser(t, s, "bob", false)
	must(t, s.Grant(ctx, u.ID, "gone", []string{"view"}))
	must(t, s.Grant(ctx, other.ID, "a", []string{"view"}))
	want := map[string]Perm{"a": {View: true, Build: true}, "b": {View: true, Cancel: true}}
	must(t, s.ReplaceGrants(ctx, u.ID, map[string]Perm{"a": want["a"], "b": want["b"], "none": {}}))
	got, err := s.Permissions(ctx, u.ID)
	must(t, err)
	if len(got) != 2 || got["a"] != want["a"] || got["b"] != want["b"] {
		t.Fatalf("grants %+v, want %+v", got, want)
	}
	if theirs, _ := s.Permissions(ctx, other.ID); !theirs["a"].View {
		t.Fatal("another user's grants were touched")
	}
	must(t, s.ReplaceGrants(ctx, u.ID, nil))
	if got, _ := s.Permissions(ctx, u.ID); len(got) != 0 {
		t.Fatalf("no grants left %+v", got)
	}
	if err := s.ReplaceGrants(ctx, 999, want); !errors.Is(err, ErrNotFound) {
		t.Fatalf("grants for nobody: %v", err)
	}
}

func TestSecretInfosNameEachSecretAndWhenItWasSet(t *testing.T) {
	s := open(t)
	before := time.Now().Add(-time.Second)
	must(t, s.PutSecret(ctx, "ZED", []byte("sealed")))
	must(t, s.PutSecret(ctx, "ALPHA", []byte("sealed")))
	infos, err := s.SecretInfos(ctx)
	must(t, err)
	if len(infos) != 2 || infos[0].Name != "ALPHA" || infos[1].Name != "ZED" || infos[0].UpdatedAt.Before(before) {
		t.Fatalf("%+v", infos)
	}
}

func TestATokenIsFoundByIdWithItsUserAndCounted(t *testing.T) {
	s := open(t)
	ann, bob := newUser(t, s, "ann", false), newUser(t, s, "bob", false)
	tok, err := s.CreateToken(ctx, ann.ID, "ci", "h1")
	must(t, err)
	_, err = s.CreateToken(ctx, ann.ID, "laptop", "h2")
	must(t, err)
	got, err := s.Token(ctx, tok.ID)
	must(t, err)
	if got.UserID != ann.ID || got.Label != "ci" || got.ExpiresAt != nil {
		t.Fatalf("%+v", got)
	}
	if _, err := s.Token(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a token that is not there: %v", err)
	}
	for user, want := range map[int64]int{ann.ID: 2, bob.ID: 0} {
		if n, err := s.CountTokens(ctx, user); err != nil || n != want {
			t.Fatalf("user %d has %d tokens, want %d (%v)", user, n, want, err)
		}
	}
}

func TestAnExpiredTokenResolvesAsNoToken(t *testing.T) {
	s := open(t)
	u := newUser(t, s, "ann", false)
	past, future := time.Now().Add(-time.Second), time.Now().Add(time.Hour)
	expired, err := s.CreateTokenUntil(ctx, u.ID, "old", "expired", &past)
	must(t, err)
	_, err = s.CreateTokenUntil(ctx, u.ID, "new", "live", &future)
	must(t, err)
	if _, err := s.UserByTokenHash(ctx, "expired"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an expired token resolves: %v", err)
	}
	if got, err := s.UserByTokenHash(ctx, "live"); err != nil || got.ID != u.ID {
		t.Fatalf("a token before its expiry: %+v, %v", got, err)
	}
	got, err := s.Token(ctx, expired.ID)
	must(t, err)
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(past.UTC()) || got.LastUsedAt != nil {
		t.Fatalf("the expired token reads %+v", got)
	}
}

func TestAuditComesBackNewestFirstPageByPageAndPrunesToTheNewest(t *testing.T) {
	s := open(t)
	at := time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)
	for i := range 5 {
		must(t, s.AppendAudit(ctx, Audit{At: at.Add(time.Duration(i) * time.Minute), Actor: "cli", Action: "user.add", Target: string(rune('a' + i)), Detail: "admin"}))
	}
	page, err := s.AuditBefore(ctx, 0, 2)
	must(t, err)
	if len(page) != 2 || page[0].Target != "e" || page[1].Target != "d" || page[0].Actor != "cli" || page[0].Detail != "admin" || !page[0].At.Equal(at.Add(4*time.Minute)) {
		t.Fatalf("the newest page %+v", page)
	}
	next, err := s.AuditBefore(ctx, page[1].ID, 10)
	must(t, err)
	if len(next) != 3 || next[0].Target != "c" || next[2].Target != "a" {
		t.Fatalf("the page after %+v", next)
	}
	must(t, s.PruneAudit(ctx, 2))
	left, err := s.AuditBefore(ctx, 0, 10)
	must(t, err)
	if len(left) != 2 || left[0].Target != "e" || left[1].Target != "d" {
		t.Fatalf("after pruning to 2: %+v", left)
	}
}

func TestADeliveryIdIsRecordedOnceAndForgottenWhenOld(t *testing.T) {
	s := open(t)
	at := time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)
	must(t, s.RecordDelivery(ctx, "d-1", "a", at))
	if err := s.RecordDelivery(ctx, "d-1", "b", at.Add(time.Minute)); !errors.Is(err, ErrSeen) {
		t.Fatalf("a delivery seen before: %v", err)
	}
	must(t, s.RecordDelivery(ctx, "d-2", "a", at.Add(time.Hour)))
	must(t, s.PruneDeliveries(ctx, at.Add(time.Minute)))
	must(t, s.RecordDelivery(ctx, "d-1", "a", at.Add(2*time.Hour)))
	if err := s.RecordDelivery(ctx, "d-2", "a", at.Add(2*time.Hour)); !errors.Is(err, ErrSeen) {
		t.Fatalf("a delivery newer than the cut was forgotten: %v", err)
	}
}

func TestRevokingAUsersTokensTakesEveryOneOfTheirsAndNoOtherUsers(t *testing.T) {
	s := open(t)
	ann, bob := newUser(t, s, "ann", false), newUser(t, s, "bob", false)
	for _, u := range []*User{ann, ann, bob} {
		_, err := s.CreateToken(ctx, u.ID, "t", "hash-"+time.Now().String())
		must(t, err)
	}
	n, err := s.DeleteUserTokens(ctx, ann.ID)
	must(t, err)
	left, err := s.CountTokens(ctx, ann.ID)
	must(t, err)
	kept, err := s.CountTokens(ctx, bob.ID)
	must(t, err)
	if n != 2 || left != 0 || kept != 1 {
		t.Fatalf("revoked %d, ann has %d, bob has %d", n, left, kept)
	}
}

// A new password ends the devices that logged in with the old one; a hash
// renewed at a login is the same password and keeps them.
func TestANewPasswordStartsANewDeviceEpochAndARenewedHashDoesNot(t *testing.T) {
	s := open(t)
	u := newUser(t, s, "ann", false)
	epoch := func() int64 {
		t.Helper()
		got, err := s.UserByID(ctx, u.ID)
		must(t, err)
		return got.DeviceEpoch
	}
	if epoch() != 0 {
		t.Fatal("a new user's epoch is not 0")
	}
	must(t, s.ResetPassword(ctx, u.ID, "h2"))
	if epoch() != 1 {
		t.Fatal("a new password kept the epoch")
	}
	must(t, s.SetUserHash(ctx, u.ID, "h3"))
	if epoch() != 1 {
		t.Fatal("a renewed hash changed the epoch")
	}
	byToken := func() int64 {
		t.Helper()
		_, err := s.CreateToken(ctx, u.ID, "ci", "th")
		must(t, err)
		got, err := s.UserByTokenHash(ctx, "th")
		must(t, err)
		return got.DeviceEpoch
	}
	if byToken() != 1 {
		t.Fatal("a user read by token has another epoch")
	}
}

func TestChangePasswordIsOneChangeOfTheHashTheEpochAndTheSessions(t *testing.T) {
	s := open(t)
	ann, bob := newUser(t, s, "ann", false), newUser(t, s, "bob", false)
	newSession(t, s, "this", ann.ID)
	newSession(t, s, "other", ann.ID)
	newSession(t, s, "bobs", bob.ID)
	next := &Session{ID: "next", CSRF: "c", ExpiresAt: time.Now().Add(time.Hour)}

	// A session of another user's is not this one: nothing changes.
	if err := s.ChangePassword(ctx, ann.ID, "h2", "bobs", next); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	must(t, s.DeleteSession(ctx, "this"))
	if err := s.ChangePassword(ctx, ann.ID, "h2", "this", next); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	u, err := s.UserByID(ctx, ann.ID)
	must(t, err)
	if u.Hash != "h" || u.DeviceEpoch != 0 {
		t.Fatalf("a refused change changed %+v", u)
	}
	for _, id := range []string{"other", "bobs"} {
		if _, err := s.Session(ctx, id); err != nil {
			t.Fatalf("a refused change ended %s: %v", id, err)
		}
	}

	newSession(t, s, "this", ann.ID)
	must(t, s.ChangePassword(ctx, ann.ID, "h2", "this", next))
	u, err = s.UserByID(ctx, ann.ID)
	must(t, err)
	if u.Hash != "h2" || u.DeviceEpoch != 1 || next.UserID != ann.ID {
		t.Fatalf("%+v %+v", u, next)
	}
	for _, id := range []string{"this", "other"} {
		if _, err := s.Session(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s is still there: %v", id, err)
		}
	}
	for _, id := range []string{"next", "bobs"} {
		if _, err := s.Session(ctx, id); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
}

func TestResetPasswordIsOneChangeOfTheHashTheEpochAndEverySession(t *testing.T) {
	s := open(t)
	ann, bob := newUser(t, s, "ann", false), newUser(t, s, "bob", false)
	newSession(t, s, "one", ann.ID)
	newSession(t, s, "two", ann.ID)
	newSession(t, s, "bobs", bob.ID)
	must(t, s.ResetPassword(ctx, ann.ID, "h2"))
	u, err := s.UserByID(ctx, ann.ID)
	must(t, err)
	if u.Hash != "h2" || u.DeviceEpoch != 1 {
		t.Fatalf("%+v", u)
	}
	n, err := s.CountSessions(ctx, ann.ID)
	must(t, err)
	if n != 0 {
		t.Fatalf("%d sessions left", n)
	}
	if _, err := s.Session(ctx, "bobs"); err != nil {
		t.Fatal(err)
	}
	if err := s.ResetPassword(ctx, 999, "h"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown user: %v", err)
	}
}
