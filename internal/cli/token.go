package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"time"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/store"
)

const tokenUsage = `token create <user> --label <text> [--expires 30|90|365] --data <dir>
       kipp token revoke <id> --data <dir>
       kipp token list <user> --data <dir>`

func cmdToken(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	fs := newFlags(tokenUsage, stderr)
	label := fs.String("label", "", "create: what the token is for (required)")
	expires := fs.String("expires", "", "create: the `days` the token works for, 30, 90 or 365; without it, for good")
	data := fs.String("data", "", "the server's data `directory` (required)")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return exitFor(err)
	}
	return onStore("token", *data, fs.Usage, stderr, tokenAction(positional, *label, *expires, stdout))
}

// tokenAction is the subcommand the arguments name, or nil when they name none.
func tokenAction(args []string, label, expires string, stdout io.Writer) action {
	if len(args) != 2 {
		return nil
	}
	switch sub, arg := args[0], args[1]; {
	case sub == "create" && label != "":
		return func(ctx context.Context, st *store.Store) error {
			days, err := tokenDays(expires)
			if err != nil {
				return err
			}
			return tokenCreate(ctx, st, arg, label, days, stdout)
		}
	case sub == "revoke":
		return func(ctx context.Context, st *store.Store) error { return tokenRevoke(ctx, st, arg) }
	case sub == "list":
		return func(ctx context.Context, st *store.Store) error { return tokenList(ctx, st, arg, stdout) }
	}
	return nil
}

// tokenDays is the lifetime --expires asks for: 0, for good, when it is
// not given. Given, it is one of api.TokenDays other than 0, which would
// read as a token that expires at once.
func tokenDays(expires string) (int, error) {
	if expires == "" {
		return 0, nil
	}
	days, err := strconv.Atoi(expires)
	if err != nil || days == 0 || !slices.Contains(api.TokenDays, days) {
		return 0, refused(fmt.Sprintf("--expires %s: a token lasts 30, 90 or 365 days, or for good without --expires", expires))
	}
	return days, nil
}

// tokenCreate prints the token: the one time anybody sees it, since the
// database keeps its hash.
func tokenCreate(ctx context.Context, st *store.Store, user, label string, days int, stdout io.Writer) error {
	u, err := findUser(ctx, st, user)
	if err != nil {
		return err
	}
	until, err := api.TokenExpiry(days, time.Now())
	if err != nil {
		return refused(err.Error())
	}
	plain, hash, err := auth.NewToken()
	if err != nil {
		return err
	}
	t, err := st.CreateTokenUntil(ctx, u.ID, label, hash, until)
	if err != nil {
		return err
	}
	say(stdout, "token for %s (%s), shown once:\n%s", u.Name, label, plain)
	return record(ctx, st, "token.create", u.Name, api.TokenCreated(t))
}

func tokenRevoke(ctx context.Context, st *store.Store, arg string) error {
	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		return refused("the id is the number kipp token list prints, not the token")
	}
	t, err := st.Token(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no token with id %d", id)
	}
	if err != nil {
		return err
	}
	owner, err := st.UserByID(ctx, t.UserID)
	if err != nil {
		return err
	}
	if err := st.DeleteToken(ctx, id); err != nil {
		return err
	}
	return record(ctx, st, "token.revoke", owner.Name, api.TokenDetail(t))
}

func tokenList(ctx context.Context, st *store.Store, user string, stdout io.Writer) error {
	u, err := findUser(ctx, st, user)
	if err != nil {
		return err
	}
	tokens, err := st.Tokens(ctx, u.ID)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, t := range tokens {
		used := "never used"
		if t.LastUsedAt != nil {
			used = "last used " + t.LastUsedAt.Local().Format("2006-01-02 15:04")
		}
		lasts := "never expires"
		switch {
		case t.ExpiresAt == nil:
		case t.ExpiresAt.After(now):
			lasts = "expires " + t.ExpiresAt.Local().Format(time.DateOnly)
		default:
			lasts = "expired " + t.ExpiresAt.Local().Format(time.DateOnly)
		}
		say(stdout, "%d  %s  created %s  %s  %s", t.ID, t.Label, t.CreatedAt.Local().Format(time.DateOnly), used, lasts)
	}
	return nil
}
