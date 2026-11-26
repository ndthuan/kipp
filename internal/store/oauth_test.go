package store

import (
	"errors"
	"strconv"
	"testing"
	"time"
)

func issue(tag string) OAuthIssue {
	return OAuthIssue{AccessHash: "a-" + tag, RefreshHash: "r-" + tag, AccessTTL: time.Hour, RefreshTTL: 30 * 24 * time.Hour}
}

func newGrant(t *testing.T, s *Store, u *User, now time.Time) *OAuthGrant {
	t.Helper()
	g := &OAuthGrant{UserID: u.ID, ClientID: "app", ClientName: "An app", Resource: "https://ci.example.com/mcp",
		Scopes: []string{"kipp:read", "kipp:build"}, ExpiresAt: now.Add(90 * 24 * time.Hour)}
	must(t, s.CreateOAuthGrant(ctx, g, issue("1"), now))
	return g
}

func TestAGrantEndsWithItsUserAndItsTokensWithTheGrant(t *testing.T) {
	s := open(t)
	_ = newUser(t, s, "root", true)
	u := newUser(t, s, "ann", false)
	now := time.Now()
	g := newGrant(t, s, u, now)
	must(t, s.DeleteUser(ctx, u.ID))
	if _, _, err := s.OAuthAccess(ctx, "a-1", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a token of a deleted user: %v", err)
	}
	var n int
	must(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_tokens WHERE grant_id = ?`, g.ID).Scan(&n))
	if n != 0 {
		t.Fatalf("%d tokens outlive their user", n)
	}
}

func TestOnlyTheHashOfATokenIsKept(t *testing.T) {
	s := open(t)
	u := newUser(t, s, "ann", false)
	newGrant(t, s, u, time.Now())
	rows, err := s.db.QueryContext(ctx, `SELECT hash FROM oauth_tokens`)
	must(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var h string
		must(t, rows.Scan(&h))
		if h != "a-1" && h != "r-1" {
			t.Fatalf("an unexpected row %q", h)
		}
	}
}

func TestAnAccessTokenWorksUntilItOrItsGrantExpires(t *testing.T) {
	s := open(t)
	u := newUser(t, s, "ann", false)
	now := time.Now()
	g := newGrant(t, s, u, now)
	got, who, err := s.OAuthAccess(ctx, "a-1", now.Add(59*time.Minute))
	must(t, err)
	if who.Name != "ann" || got.ClientID != "app" || got.Resource != "https://ci.example.com/mcp" || len(got.Scopes) != 2 {
		t.Fatalf("%+v %+v", got, who)
	}
	if _, _, err := s.OAuthAccess(ctx, "a-1", now.Add(61*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an expired access token: %v", err)
	}
	if _, _, err := s.OAuthAccess(ctx, "r-1", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refresh token as an access token: %v", err)
	}
	var exp string
	must(t, s.db.QueryRowContext(ctx, `SELECT expires_at FROM oauth_tokens WHERE hash = 'r-1'`).Scan(&exp))
	if exp != ts(now.Add(30*24*time.Hour)) {
		t.Fatalf("refresh token ends %s", exp)
	}
	must(t, s.DeleteOAuthGrant(ctx, g.ID))
	if _, _, err := s.OAuthAccess(ctx, "a-1", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the token of a deleted grant: %v", err)
	}
	if err := s.DeleteOAuthGrant(ctx, g.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting a grant twice: %v", err)
	}
}

func TestARefreshTokenIsCutShortByTheEndOfTheConsent(t *testing.T) {
	s := open(t)
	u := newUser(t, s, "ann", false)
	now := time.Now()
	g := newGrant(t, s, u, now)
	for i, day := range []int{29, 58, 87} {
		old := "r-" + strconv.Itoa(i+1)
		_, err := s.RotateOAuthRefresh(ctx, old, "app", issue(strconv.Itoa(i+2)), now.Add(time.Duration(day)*24*time.Hour))
		must(t, err)
	}
	var exp string
	must(t, s.db.QueryRowContext(ctx, `SELECT expires_at FROM oauth_tokens WHERE hash = 'r-4'`).Scan(&exp))
	if exp != ts(g.ExpiresAt) {
		t.Fatalf("the new refresh token ends %s, want the grant's %s", exp, ts(g.ExpiresAt))
	}
}

func TestARefreshTokenWorksOnceForItsClientAndAUsedOneEndsTheGrant(t *testing.T) {
	s := open(t)
	u := newUser(t, s, "ann", false)
	now := time.Now()
	g := newGrant(t, s, u, now)
	if _, err := s.RotateOAuthRefresh(ctx, "r-1", "another-app", issue("x"), now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another client's refresh token: %v", err)
	}
	if _, err := s.RotateOAuthRefresh(ctx, "unknown", "app", issue("x"), now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown refresh token: %v", err)
	}
	got, err := s.RotateOAuthRefresh(ctx, "r-1", "app", issue("2"), now.Add(time.Minute))
	must(t, err)
	if got.ID != g.ID {
		t.Fatalf("rotated grant %d, want %d", got.ID, g.ID)
	}
	if _, _, err := s.OAuthAccess(ctx, "a-2", now.Add(time.Minute)); err != nil {
		t.Fatalf("the new access token: %v", err)
	}
	if gone, err := s.RotateOAuthRefresh(ctx, "r-1", "app", issue("3"), now.Add(2*time.Minute)); !errors.Is(err, ErrOAuthReuse) || gone == nil || gone.ID != g.ID {
		t.Fatalf("reuse: %+v, %v", gone, err)
	}
	for _, tok := range []string{"a-1", "a-2"} {
		if _, _, err := s.OAuthAccess(ctx, tok, now.Add(2*time.Minute)); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s survives the reuse: %v", tok, err)
		}
	}
	if _, err := s.RotateOAuthRefresh(ctx, "r-2", "app", issue("4"), now.Add(3*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the newest refresh token survives the reuse: %v", err)
	}
}

func TestANewPasswordEndsTheAppGrantsOfItsUser(t *testing.T) {
	s := open(t)
	ann := newUser(t, s, "ann", false)
	bob := newUser(t, s, "bob", false)
	now := time.Now()
	newGrant(t, s, ann, now)
	other := &OAuthGrant{UserID: bob.ID, ClientID: "app", ClientName: "An app", Resource: "r", Scopes: []string{"kipp:read"}, ExpiresAt: now.Add(time.Hour)}
	must(t, s.CreateOAuthGrant(ctx, other, issue("b"), now))
	must(t, s.ResetPassword(ctx, ann.ID, "new"))
	if gs, err := s.OAuthGrants(ctx, ann.ID, now); err != nil || len(gs) != 0 {
		t.Fatalf("after a reset: %+v, %v", gs, err)
	}
	if gs, err := s.OAuthGrants(ctx, bob.ID, now); err != nil || len(gs) != 1 {
		t.Fatalf("another user's grants: %+v, %v", gs, err)
	}

	newGrant(t, s, ann, now)
	newSession(t, s, "sid", ann.ID)
	must(t, s.ChangePassword(ctx, ann.ID, "newer", "sid", &Session{ID: "sid2", CSRF: "c", ExpiresAt: now.Add(time.Hour)}))
	if gs, err := s.OAuthGrants(ctx, ann.ID, now); err != nil || len(gs) != 0 {
		t.Fatalf("after a change: %+v, %v", gs, err)
	}
}

func TestClientsAreSavedRenewedAndSweptWhenUnused(t *testing.T) {
	s := open(t)
	now := time.Now()
	c := &OAuthClient{ID: "c1", Name: "One", RedirectURIs: []string{"https://a.example/cb", "http://127.0.0.1/cb"}}
	created, err := s.SaveOAuthClient(ctx, c, now)
	must(t, err)
	again, err := s.SaveOAuthClient(ctx, &OAuthClient{ID: "c1", Name: "Renamed", RedirectURIs: []string{"https://a.example/cb"}}, now.Add(time.Hour))
	must(t, err)
	if !created || again {
		t.Fatalf("created %v then %v", created, again)
	}
	got, err := s.OAuthClient(ctx, "c1")
	must(t, err)
	if got.Name != "Renamed" || len(got.RedirectURIs) != 1 || !got.CreatedAt.Equal(now.UTC()) {
		t.Fatalf("%+v", got)
	}
	_, err = s.SaveOAuthClient(ctx, &OAuthClient{ID: "c2", Name: "Two", RedirectURIs: []string{"https://b.example/cb"}}, now)
	must(t, err)
	u := newUser(t, s, "ann", false)
	must(t, s.CreateOAuthGrant(ctx, &OAuthGrant{UserID: u.ID, ClientID: "c2", ClientName: "Two", Resource: "r", Scopes: []string{"kipp:read"}, ExpiresAt: now.Add(200 * 24 * time.Hour)}, issue("g"), now))
	must(t, s.DeleteExpiredOAuth(ctx, now.Add(40*24*time.Hour), now.Add(10*24*time.Hour)))
	if _, err := s.OAuthClient(ctx, "c1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unused client survives: %v", err)
	}
	if _, err := s.OAuthClient(ctx, "c2"); err != nil {
		t.Fatalf("a client with a grant was swept: %v", err)
	}
	if n, err := s.CountOAuthClients(ctx); err != nil || n != 1 {
		t.Fatalf("%d clients, %v", n, err)
	}
}

func TestTheSweepRemovesGrantsAndTokensPastTheirEnd(t *testing.T) {
	s := open(t)
	u := newUser(t, s, "ann", false)
	now := time.Now()
	g := newGrant(t, s, u, now)
	must(t, s.DeleteExpiredOAuth(ctx, now.Add(2*time.Hour), now))
	var n int
	must(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_tokens WHERE kind = 'access'`).Scan(&n))
	if n != 0 {
		t.Fatalf("%d expired access tokens kept", n)
	}
	if _, err := s.OAuthGrant(ctx, u.ID, g.ID); err != nil {
		t.Fatalf("a live grant swept: %v", err)
	}
	must(t, s.DeleteExpiredOAuth(ctx, now.Add(100*24*time.Hour), now))
	if _, err := s.OAuthGrant(ctx, u.ID, g.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an ended grant kept: %v", err)
	}
}

func TestARefreshTokenUsedAgainWithinItsGraceGetsNewTokensAndKeepsTheGrant(t *testing.T) {
	s := open(t)
	u := newUser(t, s, "ann", false)
	now := time.Now()
	g := newGrant(t, s, u, now)
	graced := func(tag string) OAuthIssue { is := issue(tag); is.ReuseGrace = 30 * time.Second; return is }
	_, err := s.RotateOAuthRefresh(ctx, "r-1", "app", graced("2"), now)
	must(t, err)
	got, err := s.RotateOAuthRefresh(ctx, "r-1", "app", graced("3"), now.Add(29*time.Second))
	must(t, err)
	if got.ID != g.ID {
		t.Fatalf("rotated grant %d, want %d", got.ID, g.ID)
	}
	for _, tok := range []string{"a-2", "a-3"} {
		if _, _, err := s.OAuthAccess(ctx, tok, now.Add(30*time.Second)); err != nil {
			t.Fatalf("%s: %v", tok, err)
		}
	}
	if _, err := s.RotateOAuthRefresh(ctx, "r-1", "app", graced("4"), now.Add(29*time.Second)); err != nil {
		t.Fatalf("the grace counts from the first use: %v", err)
	}
	if _, err := s.RotateOAuthRefresh(ctx, "r-1", "app", graced("5"), now.Add(31*time.Second)); !errors.Is(err, ErrOAuthReuse) {
		t.Fatalf("a use past the grace: %v", err)
	}
	if _, _, err := s.OAuthAccess(ctx, "a-3", now.Add(31*time.Second)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a token survives the reuse: %v", err)
	}
}

func TestTheIdlestClientNoGrantNamesIsTheOneRemovedAndNoneWhenAllHaveOne(t *testing.T) {
	s := open(t)
	now := time.Date(2026, 11, 9, 7, 0, 0, 0, time.UTC)
	u, err := s.CreateUser(ctx, "ann", "h", false)
	must(t, err)
	for i, id := range []string{"app", "old", "new"} {
		_, err := s.SaveOAuthClient(ctx, &OAuthClient{ID: id, Name: "name of " + id, RedirectURIs: []string{"https://app.example.com/cb"}}, now.Add(time.Duration(i)*time.Hour))
		must(t, err)
	}
	newGrant(t, s, u, now)
	must(t, s.TouchOAuthClient(ctx, "old", now.Add(5*time.Hour)))
	for _, want := range []string{"new", "old"} {
		gone, err := s.DeleteIdlestOAuthClient(ctx)
		must(t, err)
		if gone.ID != want || gone.Name != "name of "+want {
			t.Fatalf("removed %+v, want %s", gone, want)
		}
	}
	if _, err := s.DeleteIdlestOAuthClient(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("with a grant on every client: %v", err)
	}
	if _, err := s.OAuthClient(ctx, "app"); err != nil {
		t.Fatalf("the client a grant names: %v", err)
	}
}
