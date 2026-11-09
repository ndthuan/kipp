package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/store"
	"github.com/ndthuan/kipp/internal/waitfor"
	"golang.org/x/crypto/argon2"
)

func service(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(st, time.Hour, testDeviceKey), st
}

// testDeviceKey signs the device values of a test's logins.
var testDeviceKey = []byte("an obvious test device key 32 b.")

// login is a login from nowhere in particular: no device, not local.
func login(ctx context.Context, svc *Service, name, pw string) (*store.Session, error) {
	sess, _, err := svc.Login(ctx, name, pw, Attempt{})
	return sess, err
}

func verifies(t *testing.T, hash, pw string) bool {
	t.Helper()
	ok, err := VerifyPassword(context.Background(), hash, pw)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func grant(t *testing.T, st *store.Store, id int64, job string, perms []string) {
	t.Helper()
	if err := st.Grant(context.Background(), id, job, perms); err != nil {
		t.Fatal(err)
	}
}

func addToken(t *testing.T, st *store.Store, id int64, label, hash string) {
	t.Helper()
	if _, err := st.CreateToken(context.Background(), id, label, hash); err != nil {
		t.Fatal(err)
	}
}

func addUser(t *testing.T, st *store.Store, name, hash string, admin bool) *store.User {
	t.Helper()
	u, err := st.CreateUser(context.Background(), name, hash, admin)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestAPasswordVerifiesAgainstItsOwnSaltedHashOnly(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil || h[:10] != "$argon2id$" {
		t.Fatal(err, h)
	}
	if !verifies(t, h, "correct horse") || verifies(t, h, "wrong") || verifies(t, "garbage", "x") {
		t.Fatal("verify")
	}
	h2, _ := HashPassword("correct horse")
	if h == h2 {
		t.Fatal("no salt")
	}
}

func TestLoginGivesASessionAndOneErrorForEveryFailure(t *testing.T) {
	svc, st := service(t)
	h, _ := HashPassword("pw")
	addUser(t, st, "admin", h, true)
	sess, err := login(context.Background(), svc, "admin", "pw")
	if err != nil || sess.ID == "" || sess.CSRF == "" || time.Until(sess.ExpiresAt) < 59*time.Minute {
		t.Fatal(err, sess)
	}
	u, s2, err := svc.UserFromSession(context.Background(), sess.ID)
	if err != nil || u.Name != "admin" || s2.CSRF != sess.CSRF {
		t.Fatal(err)
	}
	_, e1 := login(context.Background(), svc, "admin", "nope")
	_, e2 := login(context.Background(), svc, "nobody", "pw")
	if !errors.Is(e1, ErrBadLogin) || !errors.Is(e2, ErrBadLogin) || e1.Error() != e2.Error() {
		t.Fatal(e1, e2)
	}
}

func TestATokenResolvesToItsUserAndBuildOrCancelIncludesView(t *testing.T) {
	svc, st := service(t)
	ctx := context.Background()
	agent := addUser(t, st, "ci-agent", "x", false)
	admin := addUser(t, st, "admin", "x", true)
	grant(t, st, agent.ID, "site-smoke", []string{"view", "build", "cancel"})
	plain, hash, _ := NewToken()
	if HashToken(plain) != hash || len(plain) < 40 {
		t.Fatal("token hash")
	}
	addToken(t, st, agent.ID, "ci", hash)
	u, err := svc.UserFromToken(ctx, plain)
	if err != nil || u.ID != agent.ID {
		t.Fatal(err)
	}
	if _, err := svc.UserFromToken(ctx, "not-a-token"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	for _, c := range []struct {
		u      *store.User
		job    string
		action string
		want   bool
	}{
		{agent, "site-smoke", "view", true}, {agent, "site-smoke", "build", true}, {agent, "site-smoke", "cancel", true},
		{agent, "cleanup", "view", false}, {admin, "cleanup", "cancel", true},
	} {
		got, err := svc.Can(ctx, c.u, c.job, c.action)
		if err != nil || got != c.want {
			t.Errorf("%s %s %s: %v %v", c.u.Name, c.job, c.action, got, err)
		}
	}
	vis, _ := svc.Visible(ctx, agent, []string{"cleanup", "site-smoke"})
	if len(vis) != 1 || vis[0] != "site-smoke" {
		t.Fatal(vis)
	}
}

func TestVerifyPasswordSurvivesHostileHashes(t *testing.T) {
	for _, h := range []string{
		"$argon2id$v=19$m=65536,t=3,p=0$AAAAAAAAAAAAAAAAAAAAAA$AAAA",
		"$argon2id$v=19$m=65536,t=0,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAA",
		"$argon2id$v=19$m=4294967295,t=3,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAA",
		"$argon2id$v=19$m=65536,t=3,p=2$AAAAAAAAAAAAAAAAAAAAAA$",
		"$argon2id$v=18$m=65536,t=3,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"$argon2id$v=19$m=65536,t=3,p=2zzz$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"$argon2i$v=19$m=65536,t=3,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAA",
		"",
	} {
		if verifies(t, h, "x") {
			t.Errorf("accepted %q", h)
		}
	}
}

func TestDummyHashHasTheRealCost(t *testing.T) {
	m, tm, p, salt, key, ok := parseHash(dummyHash)
	if !ok || m != argonMemory || tm != argonTime || p != argonThreads || len(salt) != 16 || len(key) != argonKeyLen {
		t.Fatal("dummy hash does not cost what a real one does", m, tm, p, len(salt), len(key), ok)
	}
}

func TestEmptyPasswordIsRejected(t *testing.T) {
	svc, st := service(t)
	ctx := context.Background()
	h, _ := HashPassword("pw")
	addUser(t, st, "a", h, false)
	if _, err := login(ctx, svc, "a", ""); !errors.Is(err, ErrBadLogin) {
		t.Fatal(err)
	}
}

func TestSessionsExpireAndTokensAreStoredAsHashes(t *testing.T) {
	svc, st := service(t)
	ctx := context.Background()
	h, _ := HashPassword("pw")
	u := addUser(t, st, "a", h, false)
	if err := st.CreateSession(ctx, &store.Session{ID: "old", UserID: u.ID, CSRF: "c", ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.UserFromSession(ctx, "old"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	plain, hash, _ := NewToken()
	addToken(t, st, u.ID, "l", hash)
	if _, err := st.UserByTokenHash(ctx, plain); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("the plain token must not be a lookup key", err)
	}
	if !strings.HasPrefix(plain, "kipp_") || hash == plain {
		t.Fatal(plain, hash)
	}
}

func TestAnUnknownActionOrJobIsRefusedAndAnAdminSeesEveryJob(t *testing.T) {
	svc, st := service(t)
	ctx := context.Background()
	u := addUser(t, st, "viewer", "x", false)
	grant(t, st, u.ID, "j", []string{"view"})
	grant(t, st, u.ID, "k", []string{"view", "cancel"})
	for _, c := range []struct {
		job, action string
		want        bool
	}{
		{"j", "view", true}, {"j", "build", false}, {"j", "cancel", false},
		{"k", "view", true}, {"k", "build", false}, {"k", "cancel", true},
		{"j", "delete", false}, {"j", "", false}, {"nope", "view", false},
	} {
		got, err := svc.Can(ctx, u, c.job, c.action)
		if err != nil || got != c.want {
			t.Errorf("%s %s: %v %v", c.job, c.action, got, err)
		}
	}
	admin := addUser(t, st, "root", "x", true)
	vis, _ := svc.Visible(ctx, admin, []string{"b", "a"})
	if len(vis) != 2 || vis[0] != "b" {
		t.Fatal(vis)
	}
	vis, _ = svc.Visible(ctx, u, []string{"nope", "k", "j"})
	if len(vis) != 2 || vis[0] != "k" || vis[1] != "j" {
		t.Fatal(vis)
	}
}

func TestStoreOutageIsNotABadLogin(t *testing.T) {
	svc, st := service(t)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := login(context.Background(), svc, "admin", "pw")
	if err == nil || errors.Is(err, ErrBadLogin) {
		t.Fatal("outage must not read as a bad login:", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc2, _ := service(t)
	if _, err := login(ctx, svc2, "admin", "pw"); err == nil || errors.Is(err, ErrBadLogin) {
		t.Fatal("cancelled context must not read as a bad login:", err)
	}
}

func TestUnparsableStoredHashIsABadLogin(t *testing.T) {
	svc, st := service(t)
	addUser(t, st, "broken", "garbage", false)
	if _, err := login(context.Background(), svc, "broken", "pw"); !errors.Is(err, ErrBadLogin) {
		t.Fatal(err)
	}
}

func TestVisibleReturnsFreshNonNilSlices(t *testing.T) {
	svc, st := service(t)
	ctx := context.Background()
	admin := addUser(t, st, "root", "x", true)
	u := addUser(t, st, "nobody", "x", false)
	in := []string{"a", "b"}
	out, err := svc.Visible(ctx, admin, in)
	if err != nil || len(out) != 2 {
		t.Fatal(out, err)
	}
	out[0] = "changed"
	if in[0] != "a" {
		t.Fatal("admin got the caller's own slice")
	}
	none, err := svc.Visible(ctx, u, []string{"a"})
	if err != nil || none == nil || len(none) != 0 {
		t.Fatalf("want empty non-nil, got %#v %v", none, err)
	}
}

func TestParseHashIsStrict(t *testing.T) {
	if _, _, _, _, _, ok := parseHash(dummyHash); !ok {
		t.Fatal("dummy hash must parse")
	}
	for _, h := range []string{
		strings.Replace(dummyHash, "v=19", "v=18", 1),
		strings.Replace(dummyHash, "v=19", "v=190", 1),
		strings.Replace(dummyHash, "p=1", "p=1zzz", 1),
	} {
		if _, _, _, _, _, ok := parseHash(h); ok {
			t.Errorf("parsed %q", h)
		}
	}
}

// held replaces the key derivation with one that counts how many run at
// once and holds each until release is closed.
func held(t *testing.T, wait time.Duration) (most *atomic.Int32, entered chan struct{}, release chan struct{}) {
	t.Helper()
	most, entered, release = &atomic.Int32{}, make(chan struct{}, 16), make(chan struct{})
	var now atomic.Int32
	realKey, realWait := deriveKey, verifyWait
	t.Cleanup(func() { deriveKey, verifyWait = realKey, realWait })
	verifyWait = wait
	deriveKey = func(_, _ []byte, _, _ uint32, _ uint8, keyLen uint32) []byte {
		n := now.Add(1)
		for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
		}
		entered <- struct{}{}
		<-release
		now.Add(-1)
		return make([]byte, keyLen)
	}
	return most, entered, release
}

func TestNoMoreThanTwoPasswordsAreVerifiedAtOnceAndTheRestAreToldToComeBack(t *testing.T) {
	svc, st := service(t)
	h, _ := HashPassword("pw")
	addUser(t, st, "admin", h, true)
	addUser(t, st, "broken", "not a hash", false)
	most, entered, release := held(t, 100*time.Millisecond)
	const logins = 9
	results := make(chan error, logins)
	for i := range logins {
		// A known name, an unknown one and one whose hash does not parse:
		// each costs a verification, and each is bounded.
		name := []string{"admin", "nobody", "broken"}[i%3]
		go func() {
			_, err := login(context.Background(), svc, name, "wrong")
			results <- err
		}()
	}
	<-entered
	<-entered
	for range logins - 2 {
		if err := <-results; !errors.Is(err, ErrBusy) {
			t.Fatalf("a login that found no slot: %v", err)
		}
	}
	close(release)
	for range 2 {
		if err := <-results; !errors.Is(err, ErrBadLogin) {
			t.Fatalf("a login that had a slot: %v", err)
		}
	}
	if got := most.Load(); got != 2 {
		t.Fatalf("%d passwords were verified at once, want 2", got)
	}
	if len(entered) != 0 {
		t.Fatal("a login that was told to come back verified its password all the same")
	}
}

func TestALoginWaitingForASlotEndsWhenItsRequestDoes(t *testing.T) {
	svc, _ := service(t)
	_, entered, release := held(t, time.Minute)
	defer close(release)
	for range 2 {
		go func() { _, _ = login(context.Background(), svc, "nobody", "x") }()
		<-entered
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := login(ctx, svc, "nobody", "x")
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(waitfor.Safety):
		t.Fatal("a login whose client left kept waiting")
	}
}

// oldHash is a hash as the parameters before these made it: 64 MiB, three
// passes, two lanes. Data directories hold such hashes.
func oldHash(pw string) string {
	salt := []byte("0123456789abcdef")
	key := argon2.IDKey([]byte(pw), salt, 3, 64*1024, 2, 32)
	return "$argon2id$v=19$m=65536,t=3,p=2$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key)
}

func TestANewHashIsMadeWithTheOWASPRowOf19MiBTwoPassesOneLane(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil || !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatal(err, h)
	}
}

func TestAHashMadeWithTheOldParametersStillVerifies(t *testing.T) {
	h := oldHash("correct horse")
	if !verifies(t, h, "correct horse") || verifies(t, h, "wrong") {
		t.Fatal("a hash of 64 MiB, three passes, two lanes no longer verifies as it did")
	}
}

func TestALoginThatSucceedsOnAnOldHashStoresOneWithTheNewParameters(t *testing.T) {
	svc, st := service(t)
	ctx := context.Background()
	old := oldHash("correct horse")
	u := addUser(t, st, "a", old, false)
	stored := func() string {
		t.Helper()
		got, err := st.UserByID(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.Hash
	}
	if _, err := login(ctx, svc, "a", "wrong"); !errors.Is(err, ErrBadLogin) {
		t.Fatal(err)
	}
	if stored() != old {
		t.Fatal("a wrong password changed the stored hash")
	}
	if _, err := login(ctx, svc, "a", "correct horse"); err != nil {
		t.Fatal(err)
	}
	h := stored()
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") || !verifies(t, h, "correct horse") {
		t.Fatalf("stored %q", h)
	}
	if _, err := login(ctx, svc, "a", "correct horse"); err != nil {
		t.Fatal(err)
	}
	if stored() != h {
		t.Fatal("a hash that already has the new parameters was made again")
	}
}

func TestOnePasswordAtATimeIsVerifiedAgainstAHashThatAsksForMoreMemoryThanANewOne(t *testing.T) {
	svc, st := service(t)
	addUser(t, st, "old", oldHash("pw"), false)
	most, entered, release := held(t, 100*time.Millisecond)
	const logins = 4
	results := make(chan error, logins)
	for range logins {
		go func() {
			_, err := login(context.Background(), svc, "old", "wrong")
			results <- err
		}()
	}
	<-entered
	for range logins - 1 {
		if err := <-results; !errors.Is(err, ErrBusy) {
			t.Fatalf("a login that found the place taken: %v", err)
		}
	}
	// The second of the two places is still there for a hash of today's cost.
	go func() { _, _ = login(context.Background(), svc, "nobody", "x") }()
	<-entered
	close(release)
	if err := <-results; !errors.Is(err, ErrBadLogin) {
		t.Fatalf("the login that had the place: %v", err)
	}
	if got := most.Load(); got != 2 {
		t.Fatalf("%d passwords were verified at once, want the old hash and one new", got)
	}
}

func TestAChangedPasswordsSessionHasANewIdAndCSRFTokenAndTheOldOneStopsWorking(t *testing.T) {
	ctx := context.Background()
	svc, st := service(t)
	hash, err := HashPassword("an obvious test password")
	if err != nil {
		t.Fatal(err)
	}
	addUser(t, st, "alice", hash, false)
	old, err := login(ctx, svc, "alice", "an obvious test password")
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Minute).UTC().Truncate(time.Second)
	if err := st.SetSessionSudo(ctx, old.ID, until); err != nil {
		t.Fatal(err)
	}
	next, err := svc.ChangePassword(ctx, old.UserID, old.ID, hash)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == old.ID || next.CSRF == old.CSRF || next.UserID != old.UserID {
		t.Fatalf("not a new session of the same user: %+v from %+v", next, old)
	}
	if _, err := st.Session(ctx, old.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the old session: %v", err)
	}
	got, err := st.Session(ctx, next.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SudoUntil == nil || !got.SudoUntil.Equal(until) {
		t.Fatalf("sudo was not kept: %v", got.SudoUntil)
	}
	if _, err := svc.ChangePassword(ctx, old.UserID, old.ID, hash); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a session already replaced: %v", err)
	}
}

// logged gathers what the default logger writes from here on.
func logged(t *testing.T) *strings.Builder {
	t.Helper()
	var mu sync.Mutex
	b := &strings.Builder{}
	before := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return b.Write(p)
	}), nil)))
	t.Cleanup(func() { slog.SetDefault(before) })
	return b
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestAWrongPasswordLogsTheNameOnlyWhenAUserHasIt(t *testing.T) {
	svc, _ := throttled(t)
	log := logged(t)
	failTimes(t, svc, 1, "admin", Attempt{})
	failTimes(t, svc, 1, "Tr0ub4dor&3 typed in the name box", Attempt{})
	got := log.String()
	if !strings.Contains(got, "name=admin") {
		t.Fatalf("a known name was not logged:\n%s", got)
	}
	if strings.Contains(got, "Tr0ub4dor") {
		t.Fatalf("a name no user has reached the log:\n%s", got)
	}
	if !strings.Contains(got, "unknown_name=true name_length=33") {
		t.Fatalf("an unknown name was not logged as one:\n%s", got)
	}
}
