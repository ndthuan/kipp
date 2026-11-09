package api

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/store"
)

// administers is nil when u may change other people's accounts, grants
// and tokens, and the secrets: an admin, in a browser session in sudo mode.
func administers(u *store.User, sess *store.Session) error {
	if !u.Admin {
		return ErrForbidden
	}
	return credentials(u, sess)
}

// someone is the user called name, or ErrNotFound.
func (s *Service) someone(ctx context.Context, name string) (*store.User, error) {
	who, err := s.deps.Store.UserByName(ctx, name)
	return who, missing(err)
}

// lastAdmin is the refusal of a change that would leave no admin.
func lastAdmin(name string) error {
	return fmt.Errorf("%w: %s is the last admin; make another user an admin first", ErrConflict, name)
}

// AddUser makes a user and returns their generated password, the one time
// it is seen: admins do not type other people's passwords.
func (s *Service) AddUser(ctx context.Context, u *store.User, sess *store.Session, name string, admin bool) (string, error) {
	if err := administers(u, sess); err != nil {
		return "", err
	}
	if err := auth.CheckUserName(name); err != nil {
		return "", fmt.Errorf("%w: %w", ErrBadInput, err)
	}
	if _, err := s.deps.Store.UserByName(ctx, name); err == nil {
		return "", fmt.Errorf("%w: user %q already exists", ErrConflict, name)
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	pw, hash, err := generated()
	if err != nil {
		return "", err
	}
	if _, err := s.deps.Store.CreateUser(ctx, name, hash, admin); err != nil {
		return "", err
	}
	role := "user"
	if admin {
		role = "admin"
	}
	s.record(ctx, u.Name, "user.add", name, role)
	return pw, nil
}

// generated is a new password, as kipp user add makes one, and its hash.
func generated() (string, string, error) {
	pw, err := auth.NewID()
	if err != nil {
		return "", "", err
	}
	hash, err := auth.HashPassword(pw)
	return pw, hash, err
}

// SetAdmin turns name's admin on or off. Turning off the last admin is
// ErrConflict.
func (s *Service) SetAdmin(ctx context.Context, u *store.User, sess *store.Session, name string, on bool) error {
	if err := administers(u, sess); err != nil {
		return err
	}
	who, err := s.someone(ctx, name)
	if err != nil {
		return err
	}
	err = s.deps.Store.SetAdmin(ctx, who.ID, on)
	switch {
	case errors.Is(err, store.ErrLastAdmin):
		return lastAdmin(name)
	case err != nil:
		return missing(err)
	}
	state := "off"
	if on {
		state = "on"
	}
	s.record(ctx, u.Name, "user.admin", name, state)
	return nil
}

// ResetPassword gives name a generated password, returned the one time it
// is seen, and ends all their sessions; with revokeTokens their tokens go
// too, for an account somebody else may hold.
func (s *Service) ResetPassword(ctx context.Context, u *store.User, sess *store.Session, name string, revokeTokens bool) (string, error) {
	if err := administers(u, sess); err != nil {
		return "", err
	}
	who, err := s.someone(ctx, name)
	if err != nil {
		return "", err
	}
	pw, hash, err := generated()
	if err != nil {
		return "", err
	}
	if err := s.deps.Store.ResetPassword(ctx, who.ID, hash); err != nil {
		return "", missing(err)
	}
	detail := "sessions ended"
	if revokeTokens {
		n, err := s.deps.Store.DeleteUserTokens(ctx, who.ID)
		if err != nil {
			return "", err
		}
		detail += ", " + strconv.Itoa(n) + " tokens revoked"
	}
	s.record(ctx, u.Name, "user.password.reset", name, detail)
	return pw, nil
}

// DeleteUser removes name with their grants, tokens and sessions. An
// admin does not delete themselves here, so that a slip cannot lock them
// out; the last admin is never deleted.
func (s *Service) DeleteUser(ctx context.Context, u *store.User, sess *store.Session, name string) error {
	if err := administers(u, sess); err != nil {
		return err
	}
	if name == u.Name {
		return fmt.Errorf("%w: you cannot delete yourself; another admin or the CLI does that", ErrConflict)
	}
	who, err := s.someone(ctx, name)
	if err != nil {
		return err
	}
	err = s.deps.Store.DeleteUser(ctx, who.ID)
	switch {
	case errors.Is(err, store.ErrLastAdmin):
		return lastAdmin(name)
	case err != nil:
		return missing(err)
	}
	s.record(ctx, u.Name, "user.delete", name, "")
	return nil
}

// SetGrants replaces what name may do on the loaded jobs with perms. A job
// that is not loaded may not be granted; a grant on a job no longer
// loaded is left as it is, to be removed by RemoveGoneGrant, so that a
// form drawn from the loaded jobs neither keeps nor drops it unseen.
func (s *Service) SetGrants(ctx context.Context, u *store.User, sess *store.Session, name string, perms map[string]store.Perm) error {
	if err := administers(u, sess); err != nil {
		return err
	}
	loaded := s.deps.Dispatch.Snapshot().Jobs
	for _, job := range slices.Sorted(maps.Keys(perms)) {
		if loaded[job] == nil {
			return fmt.Errorf("%w: no job called %q is loaded", ErrBadInput, job)
		}
	}
	who, err := s.someone(ctx, name)
	if err != nil {
		return err
	}
	had, err := s.deps.Store.Permissions(ctx, who.ID)
	if err != nil {
		return err
	}
	next := map[string]store.Perm{}
	for job, p := range had {
		if loaded[job] == nil {
			next[job] = p
		}
	}
	given := map[string]store.Perm{}
	for job, p := range perms {
		if p != (store.Perm{}) {
			next[job], given[job] = p, p
		}
	}
	if err := s.deps.Store.ReplaceGrants(ctx, who.ID, next); err != nil {
		if _, ok := errors.AsType[*store.NeedsViewError](err); ok {
			return fmt.Errorf("%w: %w", ErrBadInput, err)
		}
		return missing(err)
	}
	s.record(ctx, u.Name, "grants.set", name, GrantsDetail(given))
	return nil
}

// GrantsDetail is a set of grants as the audit shows it: job by job in
// name order, "none" for none at all or for a job's that were taken away.
func GrantsDetail(perms map[string]store.Perm) string {
	if len(perms) == 0 {
		return "none"
	}
	var parts []string
	for _, job := range slices.Sorted(maps.Keys(perms)) {
		names := strings.Join(perms[job].Names(), ",")
		if names == "" {
			names = "none"
		}
		parts = append(parts, job+": "+names)
	}
	return strings.Join(parts, "; ")
}

// RemoveGoneGrant takes away name's grant on a job that is no longer
// loaded. A loaded job's grants are changed with SetGrants.
func (s *Service) RemoveGoneGrant(ctx context.Context, u *store.User, sess *store.Session, name, job string) error {
	if err := administers(u, sess); err != nil {
		return err
	}
	if s.deps.Dispatch.Snapshot().Jobs[job] != nil {
		return fmt.Errorf("%w: %q is loaded; its grants are set with the others", ErrConflict, job)
	}
	who, err := s.someone(ctx, name)
	if err != nil {
		return err
	}
	had, err := s.deps.Store.Permissions(ctx, who.ID)
	if err != nil {
		return err
	}
	if _, ok := had[job]; !ok {
		return ErrNotFound
	}
	if err := s.deps.Store.Grant(ctx, who.ID, job, nil); err != nil {
		return err
	}
	s.record(ctx, u.Name, "grant.remove", name, job)
	return nil
}

// CreateToken makes a token for name, such as a service account's,
// lasting days (one of TokenDays), and returns it: the one time it is seen.
func (s *Service) CreateToken(ctx context.Context, u *store.User, sess *store.Session, name, label string, days int) (string, *store.Token, error) {
	if err := administers(u, sess); err != nil {
		return "", nil, err
	}
	who, err := s.someone(ctx, name)
	if err != nil {
		return "", nil, err
	}
	return s.createToken(ctx, u.Name, who, label, days)
}

// RevokeToken revokes any user's token.
func (s *Service) RevokeToken(ctx context.Context, u *store.User, sess *store.Session, id int64) error {
	if err := administers(u, sess); err != nil {
		return err
	}
	t, err := s.deps.Store.Token(ctx, id)
	if err != nil {
		return missing(err)
	}
	owner, err := s.deps.Store.UserByID(ctx, t.UserID)
	if err != nil {
		return missing(err)
	}
	return s.revokeToken(ctx, u.Name, owner.Name, t)
}

// SetSecret seals a one-line value under name and stores it, in place of
// one set before.
func (s *Service) SetSecret(ctx context.Context, u *store.User, sess *store.Session, name, value string) error {
	return s.SetSecretOf(ctx, u, sess, SecretInput{Name: name, Kind: config.SecretText, Value: value})
}

// SecretInput is a secret to set: its kind, and what the kind holds. Value
// is a text secret's value or a userpass secret's password.
type SecretInput struct {
	Name, Kind string
	Value      string
	User       string
	Data       []byte
	// Scopes replace those of a secret set before; nil keeps them, and a
	// new secret is global.
	Scopes []string
}

// check says why the secret may not be stored, or nil. The refusal never
// holds a value.
func (in SecretInput) check() error {
	if err := config.CheckSecretName(in.Name); err != nil {
		return err
	}
	if in.Scopes != nil {
		if err := config.CheckScopes(in.Scopes); err != nil {
			return err
		}
	}
	switch in.Kind {
	case config.SecretFile:
		return config.CheckSecretFile(in.Data)
	case config.SecretUserPass:
		return errors.Join(config.CheckSecretUser(in.User), config.CheckSecretValue(in.Value))
	case config.SecretText:
		return config.CheckSecretValue(in.Value)
	}
	return fmt.Errorf("%q is not a kind of secret: %s, %s or %s", in.Kind, config.SecretText, config.SecretFile, config.SecretUserPass)
}

// sealed is what is stored of the secret, sealed under its name.
func (in SecretInput) sealed(seal func(name string, plain []byte) ([]byte, error)) ([]byte, error) {
	if in.Kind == config.SecretFile {
		return seal(in.Name, in.Data)
	}
	return seal(in.Name, []byte(in.Value))
}

// SetSecretOf seals the secret and stores it, in place of one set before,
// whatever its kind was. Builds that have started keep the value they
// started with. Neither the audit nor an error holds a value.
func (s *Service) SetSecretOf(ctx context.Context, u *store.User, sess *store.Session, in SecretInput) error {
	if err := administers(u, sess); err != nil {
		return err
	}
	if err := in.check(); err != nil {
		return fmt.Errorf("%w: %w", ErrBadInput, err)
	}
	detail := "replaced"
	if _, err := s.deps.Store.Secret(ctx, in.Name); errors.Is(err, store.ErrNotFound) {
		detail = "new"
	} else if err != nil {
		return err
	}
	sealed, err := in.sealed(s.deps.Seal)
	if err != nil {
		return fmt.Errorf("secret %s was not sealed: %w", in.Name, err)
	}
	user := ""
	if in.Kind == config.SecretUserPass {
		user = in.User
	}
	if err := s.deps.Store.PutSecretKind(ctx, in.Name, in.Kind, user, sealed, in.Scopes); err != nil {
		return err
	}
	s.record(ctx, u.Name, "secret.set", in.Name, detail+" "+in.Kind+scopeNote(in.Scopes))
	return nil
}

// scopeNote is what an audit row says of scopes that were set with a
// secret: nothing when they were left as they were.
func scopeNote(scopes []string) string {
	if scopes == nil {
		return ""
	}
	return " scopes " + strings.Join(scopes, ",")
}

// SetSecretScopes replaces the scopes of the secret name and leaves its
// value as it is.
func (s *Service) SetSecretScopes(ctx context.Context, u *store.User, sess *store.Session, name string, scopes []string) error {
	if err := administers(u, sess); err != nil {
		return err
	}
	if err := config.CheckScopes(scopes); err != nil {
		return fmt.Errorf("%w: %w", ErrBadInput, err)
	}
	if err := s.deps.Store.SetSecretScopes(ctx, name, scopes); errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	s.record(ctx, u.Name, "secret.scope", name, strings.Join(scopes, ","))
	return nil
}

// DeleteSecret removes the secret name.
func (s *Service) DeleteSecret(ctx context.Context, u *store.User, sess *store.Session, name string) error {
	if err := administers(u, sess); err != nil {
		return err
	}
	if _, err := s.deps.Store.Secret(ctx, name); err != nil {
		return missing(err)
	}
	if err := s.deps.Store.DeleteSecret(ctx, name); err != nil {
		return err
	}
	s.record(ctx, u.Name, "secret.delete", name, "")
	return nil
}

// Reload re-reads the job files, for an admin, token or browser: it
// changes nothing the files do not already say. A refused reload is
// recorded too, and its error is the reason.
func (s *Service) Reload(ctx context.Context, u *store.User) error {
	if !u.Admin {
		return ErrForbidden
	}
	if err := s.deps.Reload(ctx); err != nil {
		s.record(ctx, u.Name, "reload", "", "refused")
		return err
	}
	s.record(ctx, u.Name, "reload", "", "taken, "+strconv.Itoa(len(s.deps.Dispatch.Snapshot().Names))+" jobs")
	return nil
}

// Quiet is whether the server is quiet and what still runs, for an admin.
func (s *Service) Quiet(ctx context.Context, u *store.User) (dispatch.QuietState, error) {
	if !u.Admin {
		return dispatch.QuietState{}, ErrForbidden
	}
	return s.deps.Dispatch.QuietState(ctx)
}

// Quieting is whether the server is quiet, for the banner every user sees.
func (s *Service) Quieting() bool { return s.deps.Dispatch.Quiet() }

// SetQuiet makes the server quiet or ends it (dispatch.SetQuiet). Like a
// reload it is an admin's, token or browser alike: up.sh makes the server
// quiet from a token it makes for the purpose.
func (s *Service) SetQuiet(ctx context.Context, u *store.User, on bool) (dispatch.QuietState, error) {
	if !u.Admin {
		return dispatch.QuietState{}, ErrForbidden
	}
	st, err := s.deps.Dispatch.SetQuiet(ctx, on)
	if err != nil {
		return st, err
	}
	word := "off"
	if on {
		word = "on"
	}
	s.record(ctx, u.Name, "quiet", "", word)
	return st, nil
}

// AdministersApps is nil when u may set up a GitHub App: an admin in sudo
// mode, as for a secret, because the App's webhook secret is stored.
func (s *Service) AdministersApps(u *store.User, sess *store.Session) error {
	return administers(u, sess)
}

// RecordApp appends the audit row of a GitHub App made from its manifest.
// The App's ID and slug are not secret; its key and secrets are never
// passed here.
func (s *Service) RecordApp(ctx context.Context, u *store.User, name, detail string) {
	s.record(ctx, u.Name, "app.create", name, detail)
}
