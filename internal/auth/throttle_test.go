package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/ndthuan/kipp/internal/waitfor"
)

// clock is a time that moves only when a test says.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// throttled is a service with an admin whose password is "pw", on a
// throttle that tells the clock's time.
func throttled(t *testing.T) (*Service, *clock) {
	t.Helper()
	svc, st := service(t)
	h, err := HashPassword("pw")
	if err != nil {
		t.Fatal(err)
	}
	addUser(t, st, "admin", h, true)
	addUser(t, st, "bob", h, false)
	c := newClock()
	svc.throttle = NewThrottle(c.Now)
	return svc, c
}

// derives counts the passwords actually verified from here on.
func derives(t *testing.T) *atomic.Int32 {
	t.Helper()
	n := &atomic.Int32{}
	real := deriveKey
	t.Cleanup(func() { deriveKey = real })
	deriveKey = func(pw, salt []byte, passes, memory uint32, threads uint8, keyLen uint32) []byte {
		n.Add(1)
		return argon2.IDKey(pw, salt, passes, memory, threads, keyLen)
	}
	return n
}

// fail counts one wrong password on key, once the clock has moved past
// any wait the key is under, and returns the wait it leaves.
func fail(t *testing.T, th *Throttle, c *clock, key string) time.Duration {
	t.Helper()
	c.advance(th.Wait(key))
	wait, ok := th.Reserve(key)
	if !ok {
		t.Fatalf("%s was refused", key)
	}
	return wait
}

func attempt(t *testing.T, svc *Service, name, pw string, at Attempt) (string, error) {
	t.Helper()
	_, device, err := svc.Login(context.Background(), name, pw, at)
	return device, err
}

func failTimes(t *testing.T, svc *Service, n int, name string, at Attempt) {
	t.Helper()
	for i := range n {
		if _, err := attempt(t, svc, name, "wrong", at); !errors.Is(err, ErrBadLogin) {
			t.Fatalf("wrong password %d for %s: %v", i+1, name, err)
		}
	}
}

func throttledFor(t *testing.T, err error) time.Duration {
	t.Helper()
	var te *ThrottledError
	if !errors.As(err, &te) {
		t.Fatalf("want a throttled refusal, got %v", err)
	}
	return te.RetryAfter
}

func TestFiveWrongPasswordsInARowAreFreeAndTheSixthIsRefusedWithoutBeingChecked(t *testing.T) {
	svc, c := throttled(t)
	n := derives(t)
	failTimes(t, svc, 5, "admin", Attempt{})
	if n.Load() != 5 {
		t.Fatalf("%d passwords verified for five attempts", n.Load())
	}
	// The right password too: it is not looked at.
	if wait := throttledFor(t, func() error { _, err := attempt(t, svc, "admin", "pw", Attempt{}); return err }()); wait != 2*time.Second {
		t.Fatalf("the sixth waits %s, want 2s", wait)
	}
	if n.Load() != 5 {
		t.Fatal("a throttled attempt verified its password")
	}
	c.advance(2 * time.Second)
	failTimes(t, svc, 1, "admin", Attempt{})
	if n.Load() != 6 {
		t.Fatal("the attempt after the wait was not checked")
	}
	if wait := throttledFor(t, func() error { _, err := attempt(t, svc, "ADMIN", "pw", Attempt{}); return err }()); wait != 4*time.Second {
		t.Fatalf("after the sixth failure the wait is %s, want 4s, for the name in any case", wait)
	}
}

func TestTheWaitDoublesWithEachFailureAndStopsAtFifteenMinutes(t *testing.T) {
	c := newClock()
	th := NewThrottle(c.Now)
	var got []time.Duration
	for range 17 {
		got = append(got, fail(t, th, c, "k"))
	}
	want := []time.Duration{0, 0, 0, 0}
	for s := 2 * time.Second; s < maxWait; s *= 2 {
		want = append(want, s)
	}
	for len(want) < 17 {
		want = append(want, 15*time.Minute)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("waits %v, want %v", got, want)
	}
	if th.Wait("k") != 15*time.Minute || th.Wait("other") != 0 {
		t.Fatal(th.Wait("k"), th.Wait("other"))
	}
	c.advance(15 * time.Minute)
	if th.Wait("k") != 0 {
		t.Fatal("the wait outlasted itself")
	}
}

func TestARightPasswordClearsItsNamesRun(t *testing.T) {
	svc, _ := throttled(t)
	failTimes(t, svc, 4, "admin", Attempt{})
	if _, err := attempt(t, svc, "admin", "pw", Attempt{}); err != nil {
		t.Fatal(err)
	}
	failTimes(t, svc, 5, "admin", Attempt{})
	if _, err := attempt(t, svc, "admin", "pw", Attempt{}); throttledFor(t, err) != 2*time.Second {
		t.Fatal(err)
	}
}

func TestAKeyIsForgottenADayAfterItsLastFailure(t *testing.T) {
	c := newClock()
	th := NewThrottle(c.Now)
	for range 8 {
		fail(t, th, c, "k")
	}
	c.advance(forgetAfter)
	if th.Wait("k") != 0 || fail(t, th, c, "k") != 0 {
		t.Fatal("a run a day old still counted")
	}
}

func TestTheTableKeepsTenThousandKeysAndDropsTheColdOneThatFailedLongestAgoNeverAHotOne(t *testing.T) {
	c := newClock()
	th := NewThrottle(c.Now)
	for range freeFailures + 1 {
		fail(t, th, c, "past the free ones")
	}
	for range freeFailures {
		fail(t, th, c, "waiting")
	}
	for i := range throttleKeys - 2 {
		th.Reserve(fmt.Sprint("filler-", i))
	}
	if len(th.keys) != throttleKeys || th.order.Len() != throttleKeys {
		t.Fatal(len(th.keys), th.order.Len())
	}
	// The two hot keys failed longest ago; the cold one after them goes.
	th.Reserve("one more")
	if th.keys["filler-0"] != nil || th.keys["waiting"] == nil || th.keys["past the free ones"] == nil {
		t.Fatal("the key dropped was not the cold one that failed longest ago")
	}
	c.advance(th.Wait("waiting"))
	// Its wait over, a key within its free failures is cold again.
	th.Reserve("and another")
	if th.keys["waiting"] != nil || th.keys["past the free ones"] == nil {
		t.Fatal("a key past its free failures was dropped, or a cold one kept")
	}
	if len(th.keys) != throttleKeys || th.order.Len() != throttleKeys {
		t.Fatal(len(th.keys), th.order.Len())
	}
}

func TestATableFullOfHotKeysRefusesANewOneRatherThanDropOne(t *testing.T) {
	c := newClock()
	th := NewThrottle(c.Now)
	th.limit = 3
	for i := range th.limit {
		for range freeFailures + 1 {
			fail(t, th, c, fmt.Sprint("hot-", i))
		}
	}
	c.advance(maxWait)
	wait, ok := th.Reserve("new")
	if ok || wait <= 0 {
		t.Fatalf("a new key in a full table: %s, %v", wait, ok)
	}
	if th.keys["new"] != nil || len(th.keys) != th.limit {
		t.Fatal("the refused key was counted")
	}
	// Strangers cannot make a device's or the server's own key, so those
	// are counted past the limit, in a room of their own.
	if _, ok := th.ReserveOwn("device\x00hot-0\x00id"); !ok {
		t.Fatal("a device's key was refused")
	}
	for i := range th.limit {
		if th.keys[fmt.Sprint("hot-", i)] == nil {
			t.Fatal("a hot key was dropped")
		}
	}
}

func TestAFloodOfMadeUpNamesDoesNotClearAThrottledNamesCount(t *testing.T) {
	svc, _ := throttled(t)
	svc.throttle.limit = 8
	failTimes(t, svc, freeFailures, "admin", Attempt{})
	for i := range 2 * svc.throttle.limit {
		failTimes(t, svc, 1, fmt.Sprint("made-up-", i), Attempt{})
	}
	if _, err := attempt(t, svc, "admin", "pw", Attempt{}); throttledFor(t, err) == 0 {
		t.Fatal("the flood cleared the name's count")
	}
}

func TestADeviceThatLoggedInAsANameLogsInWhileStrangersHaveThrottledIt(t *testing.T) {
	svc, _ := throttled(t)
	device, err := attempt(t, svc, "admin", "pw", Attempt{})
	if err != nil || device == "" {
		t.Fatal(err, device)
	}
	failTimes(t, svc, 5, "admin", Attempt{})
	if _, err := attempt(t, svc, "admin", "pw", Attempt{}); throttledFor(t, err) == 0 {
		t.Fatal("not throttled")
	}
	again, err := attempt(t, svc, "admin", "pw", Attempt{Devices: []string{"junk", device}})
	if err != nil {
		t.Fatalf("the owner's own browser was shut out: %v", err)
	}
	if again != device {
		t.Fatal("a device that was still good was replaced")
	}
	// The device's own wrong passwords count, on its own counter.
	failTimes(t, svc, 5, "admin", Attempt{Devices: []string{device}})
	if _, err := attempt(t, svc, "admin", "pw", Attempt{Devices: []string{device}}); throttledFor(t, err) != 2*time.Second {
		t.Fatal(err)
	}
}

func TestADeviceMadeForAnotherNameOrForgedCountsAsNone(t *testing.T) {
	svc, _ := throttled(t)
	bobs, err := attempt(t, svc, "bob", "pw", Attempt{})
	if err != nil {
		t.Fatal(err)
	}
	failTimes(t, svc, 5, "admin", Attempt{})
	id, _, _ := cut(bobs)
	forged := []string{
		bobs,         // another user's
		id + ".AAAA", // its id with a made-up MAC
		id + "." + deviceMAC([]byte("another server's key, 32 bytes."), "admin", id, 0), // signed elsewhere
		id + "." + deviceMAC(testDeviceKey, "admin", id, 1),                             // another epoch
		"." + deviceMAC(testDeviceKey, "admin", "", 0),
		"no dot at all",
	}
	for _, v := range forged {
		if _, err := attempt(t, svc, "admin", "pw", Attempt{Devices: []string{v}}); throttledFor(t, err) == 0 {
			t.Fatalf("%q let a throttled name in", v)
		}
	}
	// What it gets on a good login is a device of its own, not the one it sent.
	if dev, err := attempt(t, svc, "bob", "pw", Attempt{Devices: []string{forged[1]}}); err != nil || dev == forged[1] {
		t.Fatal(err, dev)
	}
}

func cut(v string) (id, mac string, ok bool) {
	for i := range len(v) {
		if v[i] == '.' {
			return v[:i], v[i+1:], true
		}
	}
	return v, "", false
}

func TestALocalAttemptIsNotHeldBackByTheSharedCountAndHasOneOfItsOwn(t *testing.T) {
	svc, _ := throttled(t)
	failTimes(t, svc, 5, "admin", Attempt{})
	if _, err := attempt(t, svc, "admin", "pw", Attempt{Local: true}); err != nil {
		t.Fatalf("the server's own machine was shut out: %v", err)
	}
	failTimes(t, svc, 5, "bob", Attempt{Local: true})
	if _, err := attempt(t, svc, "bob", "pw", Attempt{Local: true}); throttledFor(t, err) == 0 {
		t.Fatal("the local counter did not hold")
	}
	if _, err := attempt(t, svc, "bob", "pw", Attempt{}); err != nil {
		t.Fatalf("local failures throttled the shared count: %v", err)
	}
}

func TestAClientAddressIsCountedAcrossTheNamesItTries(t *testing.T) {
	svc, _ := throttled(t)
	at := Attempt{Peer: "127.0.0.1:5000", Client: "203.0.113.9"}
	for i := range 5 {
		failTimes(t, svc, 1, fmt.Sprint("guess-", i), at)
	}
	if _, err := attempt(t, svc, "admin", "pw", at); throttledFor(t, err) == 0 {
		t.Fatal("a sixth name from the same client was checked")
	}
	if _, err := attempt(t, svc, "admin", "pw", Attempt{Peer: "127.0.0.1:5000", Client: "198.51.100.7"}); err != nil {
		t.Fatalf("another client was held back: %v", err)
	}
	// A right password does not clear the client's count: one who can log
	// in as someone would otherwise clear it between guesses.
	if _, err := attempt(t, svc, "bob", "pw", Attempt{Client: "203.0.113.9"}); throttledFor(t, err) == 0 {
		t.Fatal("the client's count was cleared")
	}
}

func TestAWrongPasswordForOnesOwnAccountCountsAsAFailedLogin(t *testing.T) {
	svc, _ := throttled(t)
	ctx := context.Background()
	u, err := svc.st.UserByName(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if err := svc.VerifyOwnPassword(ctx, u, "wrong", Attempt{}); !errors.Is(err, ErrBadLogin) {
			t.Fatal(err)
		}
	}
	failTimes(t, svc, 1, "admin", Attempt{})
	if err := svc.VerifyOwnPassword(ctx, u, "pw", Attempt{}); throttledFor(t, err) == 0 {
		t.Fatal("not throttled")
	}
	if _, err := attempt(t, svc, "admin", "pw", Attempt{}); throttledFor(t, err) == 0 {
		t.Fatal("the login was not throttled by the account page's failures")
	}
	if err := svc.VerifyOwnPassword(ctx, u, "pw", Attempt{Local: true}); err != nil {
		t.Fatal(err)
	}
}

func TestALoginIsRecordedAsTheUsersLast(t *testing.T) {
	svc, _ := throttled(t)
	ctx := context.Background()
	before := time.Now().Add(-time.Second)
	if _, err := attempt(t, svc, "admin", "pw", Attempt{}); err != nil {
		t.Fatal(err)
	}
	u, err := svc.st.UserByName(ctx, "admin")
	if err != nil || u.LastLoginAt == nil || u.LastLoginAt.Before(before) {
		t.Fatal(err, u.LastLoginAt)
	}
	b, _ := svc.st.UserByName(ctx, "bob")
	if b.LastLoginAt != nil {
		t.Fatal("a user who did not log in has a last login")
	}
}

func TestADeviceOrLocalAttemptIsVerifiedWhileStrangersHoldBothSharedPlaces(t *testing.T) {
	svc, _ := throttled(t)
	device, err := attempt(t, svc, "admin", "pw", Attempt{})
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range []Attempt{{Devices: []string{device}}, {Local: true}} {
		most, entered, release := held(t, 100*time.Millisecond)
		strangers := make(chan error, 3)
		for range 2 {
			go func() { _, err := attempt(t, svc, "nobody", "x", Attempt{}); strangers <- err }()
			<-entered
		}
		go func() { _, err := attempt(t, svc, "nobody", "x", Attempt{}); strangers <- err }()
		if err := <-strangers; !errors.Is(err, ErrBusy) {
			t.Fatalf("a third stranger: %v", err)
		}
		done := make(chan error, 1)
		go func() { _, err := attempt(t, svc, "admin", "wrong", at); done <- err }()
		select {
		case <-entered:
		case err := <-done:
			t.Fatalf("%+v found no place: %v", at, err)
		}
		close(release)
		for _, c := range []chan error{strangers, strangers, done} {
			if err := <-c; !errors.Is(err, ErrBadLogin) {
				t.Fatal(err)
			}
		}
		if got := most.Load(); got != 3 {
			t.Fatalf("%d verified at once, want 3", got)
		}
	}
}

func TestManyWrongPasswordsSentAtOnceForANameVerifyNoMoreThanTheFreeOnes(t *testing.T) {
	svc, _ := throttled(t)
	var derived atomic.Int32
	release := make(chan struct{})
	realKey, realWait := deriveKey, verifyWait
	t.Cleanup(func() { deriveKey, verifyWait = realKey, realWait })
	verifyWait = 10 * time.Second
	deriveKey = func(_, _ []byte, _, _ uint32, _ uint8, keyLen uint32) []byte {
		derived.Add(1)
		<-release
		return make([]byte, keyLen)
	}
	const logins = 100
	results := make(chan error, logins)
	for range logins {
		go func() { _, err := attempt(t, svc, "admin", "wrong", Attempt{}); results <- err }()
	}
	// The derivations are held until the rest have their answer: each of
	// them was in flight while the others were let in or refused. A throttle
	// that let more through answers none of those before the release, so
	// the timeout lets the test end and fail rather than hang.
	var throttledN, bad int
	timeout := time.After(waitfor.Safety)
	for range logins - freeFailures {
		select {
		case err := <-results:
			if wait := throttledFor(t, err); wait <= 0 {
				t.Fatalf("throttled for %s", wait)
			}
			throttledN++
		case <-timeout:
		}
	}
	close(release)
	for range logins - throttledN {
		if err := <-results; errors.Is(err, ErrBadLogin) {
			bad++
		}
	}
	if got := derived.Load(); got > freeFailures {
		t.Fatalf("%d passwords were verified for one name at once, want at most %d", got, freeFailures)
	}
	if throttledN != logins-freeFailures || bad != freeFailures {
		t.Fatalf("%d throttled and %d wrong, want %d and %d", throttledN, bad, logins-freeFailures, freeFailures)
	}
}

func TestAnAttemptGivenBackNoLongerCountsInTheRun(t *testing.T) {
	c := newClock()
	th := NewThrottle(c.Now)
	for range freeFailures - 1 {
		fail(t, th, c, "k")
	}
	if wait, ok := th.Reserve("k"); !ok || wait != 2*time.Second {
		t.Fatal(wait, ok)
	}
	th.Refund("k")
	if th.Wait("k") != 0 {
		t.Fatal("a refunded attempt left a wait")
	}
	if wait, ok := th.Reserve("k"); !ok || wait != 2*time.Second {
		t.Fatalf("the run counts %s, %v after a refund", wait, ok)
	}
	for range freeFailures {
		th.Refund("k")
	}
	if len(th.keys) != 0 || th.order.Len() != 0 {
		t.Fatal("a key with nothing counted kept its entry")
	}
}

func TestADeviceLogsInWhileStrangersHaveFilledTheTable(t *testing.T) {
	svc, _ := throttled(t)
	svc.throttle.limit = 2
	device, err := attempt(t, svc, "admin", "pw", Attempt{})
	if err != nil {
		t.Fatal(err)
	}
	failTimes(t, svc, freeFailures, "made-up-1", Attempt{})
	failTimes(t, svc, freeFailures, "made-up-2", Attempt{})
	if _, err := attempt(t, svc, "bob", "pw", Attempt{}); throttledFor(t, err) == 0 {
		t.Fatal("a new name found room in a full table")
	}
	if _, err := attempt(t, svc, "admin", "pw", Attempt{Devices: []string{device}}); err != nil {
		t.Fatalf("the owner's browser was shut out by a full table: %v", err)
	}
	if _, err := attempt(t, svc, "bob", "pw", Attempt{Local: true}); err != nil {
		t.Fatalf("the server's own machine was shut out by a full table: %v", err)
	}
}

// A device is one that logged in with the password; after a new one, a
// browser that may have been somebody else's is a stranger again.
func TestADeviceFromBeforeANewPasswordCountsAsNoneAfterIt(t *testing.T) {
	svc, _ := throttled(t)
	device, err := attempt(t, svc, "admin", "pw", Attempt{})
	if err != nil {
		t.Fatal(err)
	}
	u, err := svc.st.UserByName(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.st.ResetPassword(context.Background(), u.ID, u.Hash); err != nil {
		t.Fatal(err)
	}
	failTimes(t, svc, freeFailures, "admin", Attempt{})
	if _, err := attempt(t, svc, "admin", "pw", Attempt{Devices: []string{device}}); throttledFor(t, err) == 0 {
		t.Fatal("a device from before the new password still counted")
	}
	if dev, err := attempt(t, svc, "admin", "pw", Attempt{Local: true, Devices: []string{device}}); err != nil || dev == device {
		t.Fatalf("a login after the new password kept the old device: %v", err)
	}
}
