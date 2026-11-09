package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

const userUsage = `user add <name> [--admin] [--password-stdin] --data <dir>
       kipp user passwd <name> [--password-stdin] [--revoke-tokens] --data <dir>
       kipp user admin <name> on|off --data <dir>
       kipp user delete <name> --data <dir>
       kipp user grant <name> <job> view,build,cancel [--jobs <dir>] --data <dir>
       kipp user revoke <name> <job> --data <dir>
       kipp user list --data <dir>`

var permNames = []string{"view", "build", "cancel"}

func cmdUser(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlags(userUsage, stderr)
	u := &userCmd{stdin: stdin, stdout: stdout}
	fs.BoolVar(&u.admin, "admin", false, "add: the user may see and do everything")
	fs.BoolVar(&u.fromStdin, "password-stdin", false, "add, passwd: read the password from the first line of stdin: 15 to 256\ncharacters, not a common or leaked one, without the user's name or kipp in it.\nWithout it one is generated and printed once")
	fs.BoolVar(&u.revokeTokens, "revoke-tokens", false, "passwd: revoke the user's tokens too, for an account somebody else may hold")
	fs.StringVar(&u.jobs, "jobs", "", "grant: the jobs `directory`; a job that is not in it is refused.\nWithout it the job's name is not checked against any job file")
	data := fs.String("data", "", "the server's data `directory` (required)")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return exitFor(err)
	}
	return onStore("user", *data, fs.Usage, stderr, u.action(positional))
}

type userCmd struct {
	admin, fromStdin, revokeTokens bool
	jobs                           string
	stdin                          io.Reader
	stdout                         io.Writer
}

// action is the subcommand the arguments name, or nil when they name none.
func (c *userCmd) action(args []string) action {
	if len(args) == 0 {
		return nil
	}
	sub, args := args[0], args[1:]
	switch {
	case sub == "add" && len(args) == 1:
		return func(ctx context.Context, st *store.Store) error { return c.add(ctx, st, args[0]) }
	case sub == "passwd" && len(args) == 1:
		return func(ctx context.Context, st *store.Store) error { return c.passwd(ctx, st, args[0]) }
	case sub == "admin" && len(args) == 2 && (args[1] == "on" || args[1] == "off"):
		return func(ctx context.Context, st *store.Store) error { return c.setAdmin(ctx, st, args[0], args[1] == "on") }
	case sub == "delete" && len(args) == 1:
		return func(ctx context.Context, st *store.Store) error { return c.remove(ctx, st, args[0]) }
	case sub == "grant" && len(args) == 3:
		return func(ctx context.Context, st *store.Store) error {
			return c.grant(ctx, st, args[0], args[1], strings.Split(args[2], ","))
		}
	case sub == "revoke" && len(args) == 2:
		return func(ctx context.Context, st *store.Store) error { return grant(ctx, st, args[0], args[1], nil) }
	case sub == "list" && len(args) == 0:
		return c.list
	}
	return nil
}

func (c *userCmd) add(ctx context.Context, st *store.Store, name string) error {
	if err := auth.CheckUserName(name); err != nil {
		return refused(err.Error())
	}
	if _, err := st.UserByName(ctx, name); err == nil {
		return fmt.Errorf("user %q already exists", name)
	}
	pw, err := c.newPassword(name)
	if err != nil {
		return err
	}
	err = c.setPassword(name, pw, func(hash string) error {
		_, err := st.CreateUser(ctx, name, hash, c.admin)
		return err
	})
	if err != nil {
		return err
	}
	role := "user"
	if c.admin {
		role = "admin"
	}
	return record(ctx, st, "user.add", name, role)
}

// passwd sets name's password and ends their sessions, which somebody
// who had the old one may hold. The password they have already changes
// nothing: a deploy script that sets it on every run must not log its
// owner out, and one stored before the rule for a chosen password is not
// refused by that rule when it is given again.
func (c *userCmd) passwd(ctx context.Context, st *store.Store, name string) error {
	u, err := findUser(ctx, st, name)
	if err != nil {
		return err
	}
	pw, err := c.givenPassword(ctx, u)
	if err != nil {
		return err
	}
	detail := "password unchanged"
	if pw == "" {
		say(c.stdout, "%s: password unchanged", name)
	} else {
		if err := c.setPassword(name, pw, func(hash string) error { return st.ResetPassword(ctx, u.ID, hash) }); err != nil {
			return err
		}
		detail = "sessions ended"
	}
	if c.revokeTokens {
		n, err := st.DeleteUserTokens(ctx, u.ID)
		if err != nil {
			return err
		}
		say(c.stdout, "%s: %d tokens revoked", name, n)
		detail += ", " + strconv.Itoa(n) + " tokens revoked"
	}
	if pw == "" && !c.revokeTokens {
		return nil // nothing changed, so nothing to record
	}
	return record(ctx, st, "user.password.reset", name, detail)
}

// givenPassword is the password passwd sets: a new one, or the one on
// stdin, which is "" when it is the user's password already.
func (c *userCmd) givenPassword(ctx context.Context, u *store.User) (string, error) {
	if !c.fromStdin {
		return c.newPassword(u.Name)
	}
	line, err := stdinLine(c.stdin)
	if err != nil {
		return "", err
	}
	same, err := auth.VerifyPassword(ctx, u.Hash, line)
	if err != nil || same {
		return "", err
	}
	return line, checkChosen(line, u.Name)
}

// newPassword is the password from stdin, held to the rule, or a new one.
func (c *userCmd) newPassword(name string) (string, error) {
	if !c.fromStdin {
		return auth.NewID()
	}
	line, err := stdinLine(c.stdin)
	if err != nil {
		return "", err
	}
	return line, checkChosen(line, name)
}

// setPassword stores the hash of pw and says so; a generated one is
// printed: the one time anybody sees it.
func (c *userCmd) setPassword(name, pw string, store func(hash string) error) error {
	generated := !c.fromStdin
	hash, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	if err := store(hash); err != nil {
		return err
	}
	if generated {
		say(c.stdout, "%s password: %s", name, pw)
	} else {
		say(c.stdout, "%s: password set", name)
	}
	return nil
}

// stdinLine is the password on stdin's first line.
func stdinLine(stdin io.Reader) (string, error) {
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", refused("no password on stdin")
	}
	return line, nil
}

// checkChosen holds a password a person gives to the rule for one they
// choose; a generated one is 43 random characters and needs none.
func checkChosen(pw, name string) error {
	if err := auth.CheckPassword(pw, name, "kipp"); err != nil {
		return refused(err.Error())
	}
	return nil
}

// setAdmin turns name's admin on or off. The last admin stays one, for the
// reason remove keeps them.
func (c *userCmd) setAdmin(ctx context.Context, st *store.Store, name string, on bool) error {
	u, err := findUser(ctx, st, name)
	if err != nil {
		return err
	}
	err = st.SetAdmin(ctx, u.ID, on)
	if errors.Is(err, store.ErrLastAdmin) {
		return fmt.Errorf("%s is the last admin; make another user an admin first", name)
	}
	if err != nil {
		return err
	}
	state, says := "off", "not an admin"
	if on {
		state, says = "on", "admin"
	}
	say(c.stdout, "%s: %s", name, says)
	return record(ctx, st, "user.admin", name, state)
}

// remove deletes a user with their grants, tokens and sessions. The last
// admin stays: with nobody to see every job, the server's own machine
// would be the one way back in.
func (c *userCmd) remove(ctx context.Context, st *store.Store, name string) error {
	u, err := findUser(ctx, st, name)
	if err != nil {
		return err
	}
	err = st.DeleteUser(ctx, u.ID)
	if errors.Is(err, store.ErrLastAdmin) {
		return fmt.Errorf("%s is the last admin; add another before deleting this one", name)
	}
	if err != nil {
		return err
	}
	say(c.stdout, "%s: deleted", name)
	return record(ctx, st, "user.delete", name, "")
}

// grant is the grant of the command line: for a name a job can have, and,
// when the job files are given, for a job that is in them. A grant on a
// name that is no job's does nothing and says nothing, which is how a
// mistyped one goes unnoticed.
func (c *userCmd) grant(ctx context.Context, st *store.Store, name, job string, perms []string) error {
	if !config.ValidName(job) {
		return refused(fmt.Sprintf("%q is not a name a job can have: lower-case letters, digits, dot, dash and underscore only", job))
	}
	if c.jobs != "" {
		snap, err := config.Load(c.jobs)
		if err != nil {
			return fmt.Errorf("the jobs directory has errors:\n%w", err)
		}
		if snap.Jobs[job] == nil {
			return fmt.Errorf("no job called %q in %s; nothing was granted (without --jobs a grant is taken ahead of its job)", job, c.jobs)
		}
	}
	if err := grant(ctx, st, name, job, perms); err != nil {
		return err
	}
	if c.jobs == "" {
		say(c.stdout, "granted; %q was not checked against the job files (--jobs <dir> checks it)", job)
	}
	return nil
}

// grant replaces what the user may do on the job; no perms is a revoke.
func grant(ctx context.Context, st *store.Store, name, job string, perms []string) error {
	for _, p := range perms {
		if !slices.Contains(permNames, p) {
			return refused(fmt.Sprintf("%q is not view, build or cancel", p))
		}
	}
	u, err := findUser(ctx, st, name)
	if err != nil {
		return err
	}
	before, err := st.Permissions(ctx, u.ID)
	if err != nil {
		return err
	}
	slices.Sort(perms)
	if err := st.Grant(ctx, u.ID, job, slices.Compact(perms)); err != nil {
		if _, ok := errors.AsType[*store.NeedsViewError](err); ok {
			return refused("build and cancel need view too: " + job + " has no view")
		}
		return err
	}
	now, err := st.Permissions(ctx, u.ID)
	if err != nil {
		return err
	}
	// A deploy script grants the same set on every run; only a change is
	// worth a row, or the audit fills with runs that did nothing.
	if now[job] == before[job] {
		return nil
	}
	return record(ctx, st, "grants.set", name, api.GrantsDetail(map[string]store.Perm{job: now[job]}))
}

func (c *userCmd) list(ctx context.Context, st *store.Store) error {
	users, err := st.Users(ctx)
	if err != nil {
		return err
	}
	for _, u := range users {
		role := "user"
		if u.Admin {
			role = "admin"
		}
		say(c.stdout, "%s  %s", u.Name, role)
		perms, err := st.Permissions(ctx, u.ID)
		if err != nil {
			return err
		}
		for _, job := range slices.Sorted(maps.Keys(perms)) {
			say(c.stdout, "%s  %s: %s", u.Name, job, strings.Join(granted(perms[job]), ","))
		}
	}
	return nil
}

func granted(p store.Perm) []string {
	var names []string
	for i, on := range []bool{p.View, p.Build, p.Cancel} {
		if on {
			names = append(names, permNames[i])
		}
	}
	return names
}
