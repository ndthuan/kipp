package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/store"
)

// SudoFor is how long a session may change credentials and secrets after
// its user gave their password again: long enough for one sitting, short
// enough that a session left open is not a key to everything.
const SudoFor = 10 * time.Minute

// MaxTokens bounds one user's tokens, so that a session in sudo mode
// cannot fill the table.
const MaxTokens = 50

// maxLabel is the longest token label, in characters.
const maxLabel = 100

// TokenDays are the lifetimes a token may be made with, in days; 0 is for
// good.
var TokenDays = []int{0, 30, 90, 365}

// credentials is nil when sess may change credentials or secrets: a
// browser session of u's in sudo mode. A token (nil sess) never may.
func credentials(u *store.User, sess *store.Session) error {
	if sess == nil || sess.UserID != u.ID {
		return ErrForbidden
	}
	if sess.SudoUntil == nil || !time.Now().Before(*sess.SudoUntil) {
		return ErrSudoRequired
	}
	return nil
}

// verify checks u's own password on the counters a login is held to. A
// throttled or busy check comes back as auth says it.
func (s *Service) verify(ctx context.Context, u *store.User, password string, at auth.Attempt) error {
	err := s.deps.Auth.VerifyOwnPassword(ctx, u, password, at)
	if errors.Is(err, auth.ErrBadLogin) {
		return ErrWrongPassword
	}
	return err
}

// record appends an audit row for a change that has been made. A row that
// cannot be written does not undo the change, or hide from its maker a
// password shown once: it is logged loudly instead.
func (s *Service) record(ctx context.Context, actor, action, target, detail string) {
	err := s.deps.Store.AppendAudit(context.WithoutCancel(ctx), store.Audit{At: time.Now(), Actor: actor, Action: action, Target: target, Detail: detail})
	if err != nil {
		s.deps.Logger.Error("audit: a change was made and not recorded", "actor", actor, "action", action, "target", target, "err", err)
	}
}

// Sudo gives u's session leave to change credentials and secrets for
// SudoFor, once u has given their password again. A wrong one counts as a
// failed login does. A token has no session to give it to.
func (s *Service) Sudo(ctx context.Context, u *store.User, sess *store.Session, password string, at auth.Attempt) error {
	if sess == nil || sess.UserID != u.ID {
		return ErrForbidden
	}
	if err := s.verify(ctx, u, password, at); err != nil {
		return err
	}
	until := time.Now().Add(SudoFor)
	if err := s.deps.Store.SetSessionSudo(ctx, sess.ID, until); err != nil {
		return missing(err)
	}
	sess.SudoUntil = &until
	return nil
}

// ChangePassword sets u's password to next, once current is right and
// next is one a person may choose. It needs no sudo, being a password
// check itself. Every other session of u's ends, and this one is replaced
// by the session returned, under a new id: one somebody else held or
// fixed is worth nothing afterwards. Tokens are kept.
func (s *Service) ChangePassword(ctx context.Context, u *store.User, sess *store.Session, current, next, again string, at auth.Attempt) (*store.Session, error) {
	if sess == nil || sess.UserID != u.ID {
		return nil, ErrForbidden
	}
	if next != again {
		return nil, fmt.Errorf("%w: the two new passwords differ", ErrBadInput)
	}
	if err := auth.CheckPassword(next, u.Name, "kipp"); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadInput, err)
	}
	if err := s.verify(ctx, u, current, at); err != nil {
		return nil, err
	}
	hash, err := auth.HashPassword(next)
	if err != nil {
		return nil, err
	}
	renewed, err := s.deps.Auth.ChangePassword(ctx, u.ID, sess.ID, hash)
	if err != nil {
		return nil, missing(err)
	}
	s.record(ctx, u.Name, "password.change", u.Name, "the other sessions ended")
	return renewed, nil
}

// OwnTokens are u's tokens, oldest first; never a token itself.
func (s *Service) OwnTokens(ctx context.Context, u *store.User) ([]*store.Token, error) {
	return s.deps.Store.Tokens(ctx, u.ID)
}

// CreateOwnToken makes a token for u, lasting days (one of TokenDays),
// and returns it: the one time it is seen.
func (s *Service) CreateOwnToken(ctx context.Context, u *store.User, sess *store.Session, label string, days int) (string, *store.Token, error) {
	if err := credentials(u, sess); err != nil {
		return "", nil, err
	}
	return s.createToken(ctx, u.Name, u, label, days)
}

// RevokeOwnToken revokes one of u's tokens; another user's is ErrNotFound.
func (s *Service) RevokeOwnToken(ctx context.Context, u *store.User, sess *store.Session, id int64) error {
	if err := credentials(u, sess); err != nil {
		return err
	}
	t, err := s.deps.Store.Token(ctx, id)
	if err != nil {
		return missing(err)
	}
	if t.UserID != u.ID {
		return ErrNotFound
	}
	return s.revokeToken(ctx, u.Name, u.Name, t)
}

// EndOtherSessions logs u out everywhere but sess. It needs no sudo, for
// it only takes access away; from a token, which has no session, it ends
// every session of u's.
func (s *Service) EndOtherSessions(ctx context.Context, u *store.User, sess *store.Session) error {
	n, err := s.CountOtherSessions(ctx, u, sess)
	if err != nil {
		return err
	}
	except := ""
	if sess != nil {
		except = sess.ID
	}
	if err := s.deps.Store.DeleteUserSessions(ctx, u.ID, except); err != nil {
		return err
	}
	s.record(ctx, u.Name, "sessions.end", u.Name, strconv.Itoa(n)+" other sessions")
	return nil
}

// CountOtherSessions is how many live sessions u has beside sess.
func (s *Service) CountOtherSessions(ctx context.Context, u *store.User, sess *store.Session) (int, error) {
	n, err := s.deps.Store.CountSessions(ctx, u.ID)
	if err != nil {
		return 0, err
	}
	if sess != nil && n > 0 {
		n--
	}
	return n, nil
}

// createToken makes a token for owner as actor, after the checks on its
// label, lifetime and the owner's count.
func (s *Service) createToken(ctx context.Context, actor string, owner *store.User, label string, days int) (string, *store.Token, error) {
	label, err := tokenLabel(label)
	if err != nil {
		return "", nil, err
	}
	expires, err := TokenExpiry(days, time.Now())
	if err != nil {
		return "", nil, err
	}
	n, err := s.deps.Store.CountTokens(ctx, owner.ID)
	if err != nil {
		return "", nil, err
	}
	if n >= MaxTokens {
		return "", nil, fmt.Errorf("%w: a user has at most %d tokens; revoke one first", ErrConflict, MaxTokens)
	}
	plain, hash, err := auth.NewToken()
	if err != nil {
		return "", nil, err
	}
	t, err := s.deps.Store.CreateTokenUntil(ctx, owner.ID, label, hash, expires)
	if err != nil {
		return "", nil, err
	}
	s.record(ctx, actor, "token.create", owner.Name, TokenCreated(t))
	return plain, t, nil
}

// TokenExpiry is when a token made at now to last days, one of TokenDays,
// stops working: nil for never.
func TokenExpiry(days int, now time.Time) (*time.Time, error) {
	if !slices.Contains(TokenDays, days) {
		return nil, fmt.Errorf("%w: a token lasts for good or 30, 90 or 365 days", ErrBadInput)
	}
	if days == 0 {
		return nil, nil
	}
	return new(now.AddDate(0, 0, days)), nil
}

// TokenCreated is the audit's detail of a new token: what names it and
// how long it lasts.
func TokenCreated(t *store.Token) string {
	lasts := "never expires"
	if t.ExpiresAt != nil {
		lasts = "expires " + t.ExpiresAt.UTC().Format(time.DateOnly)
	}
	return TokenDetail(t) + ", " + lasts
}

func (s *Service) revokeToken(ctx context.Context, actor, owner string, t *store.Token) error {
	if err := s.deps.Store.DeleteToken(ctx, t.ID); err != nil {
		return err
	}
	s.record(ctx, actor, "token.revoke", owner, TokenDetail(t))
	return nil
}

// TokenDetail names a token in the audit by what a person sees of it.
func TokenDetail(t *store.Token) string {
	return fmt.Sprintf("label %q, id %d", t.Label, t.ID)
}

// tokenLabel is a label as it is kept, or why it may not be one: 1 to 100
// characters once the spaces around it are gone, none of them a control
// character, which a page or a log line would carry out of its place.
func tokenLabel(label string) (string, error) {
	label = strings.TrimSpace(label)
	n := utf8.RuneCountInString(label)
	switch {
	case !utf8.ValidString(label):
		return "", fmt.Errorf("%w: a label is text (UTF-8)", ErrBadInput)
	case n == 0 || n > maxLabel:
		return "", fmt.Errorf("%w: a label is 1 to %d characters", ErrBadInput, maxLabel)
	case strings.ContainsFunc(label, unicode.IsControl):
		return "", fmt.Errorf("%w: a label holds no control characters", ErrBadInput)
	}
	return label, nil
}
