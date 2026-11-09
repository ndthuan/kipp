// Package auth is who a request is and what they may do.
package auth

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

// ErrBadLogin is the one answer to a wrong name or a wrong password.
var ErrBadLogin = errors.New("wrong name or password")

// Service answers login and permission questions over the store.
type Service struct {
	st  *store.Store
	ttl time.Duration
	// deviceKey signs the device values a login hands out.
	deviceKey []byte
	throttle  *Throttle
	// builds are the tokens of the builds that run; nil on a service that
	// serves none.
	builds *BuildTokens
}

// New makes a Service whose sessions last ttl, whose device values are
// signed with deviceKey.
func New(st *store.Store, ttl time.Duration, deviceKey []byte) *Service {
	return &Service{st: st, ttl: ttl, deviceKey: deviceKey, throttle: NewThrottle(time.Now)}
}

// UseBuildTokens has the service accept the tokens of the builds in bt as
// bearer tokens, each as the build it was made for.
func (s *Service) UseBuildTokens(bt *BuildTokens) { s.builds = bt }

// SetThrottleClock makes the wrong-password throttle tell time by now, so
// that a test can hold a wait still instead of racing the wall clock. It
// starts a new, empty count.
func (s *Service) SetThrottleClock(now func() time.Time) { s.throttle = NewThrottle(now) }

// Attempt is what a password check knows of where it comes from, beside
// the name: the throttle counts it by these.
type Attempt struct {
	// Devices are the device values the request carried. One made for the
	// name tried makes the attempt that device's, counted on its own.
	Devices []string
	// Local is a request from the server's own machine (api.Local).
	Local bool
	// Peer is the address the request came from, for the log.
	Peer string
	// Client is the client's address when the server counts by it
	// (serve --client-ip-header), and empty otherwise.
	Client string
}

// guard is the counters one attempt is held to.
type guard struct {
	keys []string
	// kept are the keys a right password does not clear: a client
	// address many people may share, which someone who can log in as one
	// name could otherwise clear between guesses at others.
	kept []string
	// device is the value that made the attempt a device's, or "".
	device string
	// reserved: the attempt may take the verify place kept for those
	// whom a flood of strangers must not shut out.
	reserved bool
}

// guardFor picks the counters for an attempt at name. A device's attempt
// counts on the name and the device's id together, and one from the
// server's own machine on the name and the machine: neither is held back
// by what strangers send. Every other attempt counts on the name, shared
// by everyone who tries it, and on the client's address when there is one.
func (s *Service) guardFor(name string, epoch int64, at Attempt) guard {
	lower := strings.ToLower(name)
	for _, v := range at.Devices {
		if id, ok := deviceID(s.deviceKey, lower, epoch, v); ok {
			return guard{keys: []string{"device\x00" + lower + "\x00" + id}, device: v, reserved: true}
		}
	}
	if at.Local {
		return guard{keys: []string{"local\x00" + lower}, reserved: true}
	}
	g := guard{keys: []string{"name\x00" + lower}}
	if at.Client != "" {
		g.kept = []string{"client\x00" + at.Client}
	}
	return g
}

// check verifies pw against hash on g's counters. The attempt is counted
// on them before the password is looked at, and while one of them waits
// it is not looked at at all: the answer is a *ThrottledError. A wrong
// password stays counted and is logged; a right one, or one that was never
// judged, is given back on the counters a right password does not clear
// (the caller clears the others). known is whether a user has the name.
func (s *Service) check(ctx context.Context, name string, known bool, hash, pw string, at Attempt, g guard) (bool, error) {
	all := append(append([]string{}, g.keys...), g.kept...)
	reserve := s.throttle.Reserve
	if g.reserved {
		reserve = s.throttle.ReserveOwn
	}
	wait, counted := reserve(all...)
	if !counted {
		return false, &ThrottledError{RetryAfter: wait}
	}
	ok, err := verifyPassword(ctx, hash, pw, g.reserved)
	if err != nil {
		s.throttle.Refund(all...)
		return false, err
	}
	if ok {
		s.throttle.Refund(g.kept...)
		return true, nil
	}
	// A name no user has may be a password typed in the wrong box: the
	// log gets its length alone.
	args := []any{"name", name}
	if !known {
		args = []any{"unknown_name", true, "name_length", len(name)}
	}
	args = append(args, "peer", at.Peer)
	if at.Client != "" {
		args = append(args, "client", at.Client)
	}
	if g.device != "" {
		args = append(args, "device", true)
	}
	if wait > 0 {
		args = append(args, "throttled_for", wait.String())
	}
	slog.Warn("auth: a wrong password", args...)
	return false, nil
}

// Login checks the password and opens a session. It also gives the value
// the browser keeps as a device of name's: the one it came with when that
// was made for name, a new one otherwise. A *ThrottledError is a password
// that was not looked at, and so is ErrBusy: too many are being checked.
func (s *Service) Login(ctx context.Context, name, pw string, at Attempt) (*store.Session, string, error) {
	u, err := s.st.UserByName(ctx, name)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, "", err
	}
	// An unknown name and a hash that does not parse cost what a wrong
	// password does: the dummy is verified in their place.
	hash, known, epoch := dummyHash, u != nil, int64(0)
	if u != nil {
		epoch = u.DeviceEpoch
		if _, _, _, _, _, ok := parseHash(u.Hash); ok {
			hash = u.Hash
		} else {
			u = nil
		}
	}
	g := s.guardFor(name, epoch, at)
	ok, err := s.check(ctx, name, known, hash, pw, at, g)
	if err != nil {
		return nil, "", err
	}
	if !ok || u == nil {
		return nil, "", ErrBadLogin
	}
	s.throttle.Clear(g.keys...)
	s.rehash(ctx, u, pw)
	if err := s.st.RecordLogin(ctx, u.ID, time.Now()); err != nil {
		slog.Warn("auth: a login was not recorded as the user's last", "user", u.Name, "err", err)
	}
	device := g.device
	if device == "" {
		if device, err = newDevice(s.deviceKey, name, u.DeviceEpoch); err != nil {
			return nil, "", err
		}
	}
	sess, err := s.openSession(ctx, u.ID)
	if err != nil {
		return nil, "", err
	}
	return sess, device, nil
}

// VerifyOwnPassword checks the password of u, who is logged in already,
// on the counters a login for u's name is held to: asking for it again is
// no way around them. nil is right; otherwise ErrBadLogin, a
// *ThrottledError, ErrBusy or ctx's error.
func (s *Service) VerifyOwnPassword(ctx context.Context, u *store.User, pw string, at Attempt) error {
	hash := u.Hash
	if _, _, _, _, _, ok := parseHash(hash); !ok {
		hash = dummyHash
	}
	g := s.guardFor(u.Name, u.DeviceEpoch, at)
	ok, err := s.check(ctx, u.Name, true, hash, pw, at, g)
	if err != nil {
		return err
	}
	if !ok || hash == dummyHash {
		return ErrBadLogin
	}
	s.throttle.Clear(g.keys...)
	return nil
}

// rehash stores a hash with today's parameters in place of one made with
// others, at the one moment the password is known. A hash that could not
// be stored stays as it was and is tried again at the next login: it is no
// reason to refuse this one.
func (s *Service) rehash(ctx context.Context, u *store.User, pw string) {
	if current(u.Hash) {
		return
	}
	hash, err := HashPassword(pw)
	if err == nil {
		err = s.st.SetUserHash(ctx, u.ID, hash)
	}
	if err != nil {
		slog.Warn("auth: a password's hash was not renewed", "user", u.Name, "err", err)
	}
}

func (s *Service) openSession(ctx context.Context, userID int64) (*store.Session, error) {
	sess, err := s.newSession(userID)
	if err != nil {
		return nil, err
	}
	if err := s.st.CreateSession(ctx, sess); err != nil {
		return nil, err
	}
	return sess, nil
}

// newSession is a session not yet stored, with a new id and CSRF token
// and the full lifetime.
func (s *Service) newSession(userID int64) (*store.Session, error) {
	id, err := NewID()
	if err != nil {
		return nil, err
	}
	csrf, err := NewID()
	if err != nil {
		return nil, err
	}
	return &store.Session{ID: id, UserID: userID, CSRF: csrf, ExpiresAt: time.Now().Add(s.ttl)}, nil
}

// ChangePassword gives userID, in their live session sessID, the password
// whose hash is hash, ends their other sessions and returns the session
// that takes sessID's place, with a new id and CSRF token and the full
// lifetime, its sudo kept, all in one change: a session id somebody else
// learned or fixed is worth nothing after it. A session that ended
// meanwhile is store.ErrNotFound and changes nothing.
func (s *Service) ChangePassword(ctx context.Context, userID int64, sessID, hash string) (*store.Session, error) {
	next, err := s.newSession(userID)
	if err != nil {
		return nil, err
	}
	if err := s.st.ChangePassword(ctx, userID, hash, sessID, next); err != nil {
		return nil, err
	}
	return next, nil
}

// UserFromSession resolves a session cookie.
func (s *Service) UserFromSession(ctx context.Context, id string) (*store.User, *store.Session, error) {
	sess, err := s.st.Session(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	u, err := s.st.UserByID(ctx, sess.UserID)
	if err != nil {
		return nil, nil, err
	}
	return u, sess, nil
}

// UserFromToken resolves a bearer token: a build's token is the build, a
// principal who is never an admin and holds its grants and nothing else;
// any other is a user's.
func (s *Service) UserFromToken(ctx context.Context, plain string) (*store.User, error) {
	if isBuildToken(plain) {
		if s.builds == nil {
			return nil, store.ErrNotFound
		}
		return s.builds.user(plain)
	}
	return s.st.UserByTokenHash(ctx, HashToken(plain))
}

// Can says whether u may do action (view, build, cancel) on job. A child
// of a multibranch job is granted by its parent's grant: the parent is
// found in the rows the store keeps of children, as long as a build of the
// child is, and never from the name, since a job of a file may hold a dot.
// A repo of an owner job, and every child of one, is granted by the owner
// job's grant the same way.
func (s *Service) Can(ctx context.Context, u *store.User, job, action string) (bool, error) {
	if u.Admin {
		return true, nil
	}
	perms, err := s.grants(ctx, u)
	if err != nil {
		return false, err
	}
	p := perms[job]
	if strings.Contains(job, ".") {
		parent, err := s.st.ParentOf(ctx, job)
		if err != nil {
			return false, err
		}
		folder, err := s.st.FolderOf(ctx, job)
		if err != nil {
			return false, err
		}
		p = union(union(p, perms[parent]), perms[folder])
	}
	switch action {
	case "view":
		return mayView(p), nil
	case "build":
		return p.Build, nil
	case "cancel":
		return p.Cancel, nil
	}
	return false, nil
}

// grants are what u may do by job: a build's token holds what it was made
// with, and a user what the store grants them.
func (s *Service) grants(ctx context.Context, u *store.User) (map[string]store.Perm, error) {
	if u.Build != nil {
		return u.Build.Perms, nil
	}
	return s.st.Permissions(ctx, u.ID)
}

// mayView: whoever may build or cancel a job may see it.
func mayView(p store.Perm) bool { return p.View || p.Build || p.Cancel }

// Visible filters job names down to the ones u may view, keeping order.
func (s *Service) Visible(ctx context.Context, u *store.User, names []string) ([]string, error) {
	if u.Admin {
		return append([]string{}, names...), nil
	}
	perms, err := s.grants(ctx, u)
	if err != nil {
		return nil, err
	}
	parents, err := s.st.Parents(ctx)
	if err != nil {
		return nil, err
	}
	folders, err := s.st.Folders(ctx)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, n := range names {
		if mayView(perms[n]) || mayView(perms[parents[n]]) || mayView(perms[folders[n]]) {
			out = append(out, n)
		}
	}
	return out, nil
}

// union is what either grant allows.
func union(a, b store.Perm) store.Perm {
	return store.Perm{View: a.View || b.View, Build: a.Build || b.Build, Cancel: a.Cancel || b.Cancel}
}
