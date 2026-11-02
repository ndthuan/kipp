package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/secretbox"
	"github.com/ndthuan/kipp/internal/store"
)

const secretUsage = `secret set <NAME> [--file <path>|-] [--user <user>] [--scope <scope>]... --data <dir>
                    (a text value or a password is read from stdin; --file sets a file, - reads it from stdin;
                     a scope is global, group:<name> or job:<pattern>, global for a new secret)
       kipp secret scope <NAME> --scope <scope>... --data <dir>
       kipp secret unset <NAME> --data <dir>
       kipp secret list --data <dir>`

func cmdSecret(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlags(secretUsage, stderr)
	data := fs.String("data", "", "the server's data `directory` (required)")
	var in secretInput
	fs.StringVar(&in.file, "file", "", "set a file secret from this `path`; - is stdin")
	fs.StringVar(&in.user, "user", "", "set a user and password secret for this `user`; the password is read from stdin")
	fs.Func("scope", "let only the jobs this `scope` names list the secret; repeatable", func(v string) error {
		in.scopes = append(in.scopes, v)
		return nil
	})
	positional, err := parseArgs(fs, args)
	if err != nil {
		return exitFor(err)
	}
	return onStore("secret", *data, fs.Usage, stderr, secretAction(positional, *data, in, stdin, stdout))
}

// secretInput is what the flags of secret set say of the kind: none is a
// text secret.
type secretInput struct {
	file, user string
	// scopes are the --scope flags; none keeps those of a secret that is
	// set, and is global for a new one.
	scopes []string
}

// secretAction is the subcommand the arguments name, or nil when they
// name none. No subcommand takes a value as an argument, which would stay
// in the shell's history and show in the process list.
func secretAction(args []string, data string, in secretInput, stdin io.Reader, stdout io.Writer) action {
	switch {
	case len(args) == 2 && args[0] == "set":
		return func(ctx context.Context, st *store.Store) error {
			return secretSet(ctx, st, data, args[1], in, stdin, stdout)
		}
	case len(args) == 2 && args[0] == "scope":
		return func(ctx context.Context, st *store.Store) error {
			return secretScope(ctx, st, args[1], in.scopes, stdout)
		}
	case len(args) == 2 && args[0] == "unset":
		return func(ctx context.Context, st *store.Store) error { return secretUnset(ctx, st, args[1]) }
	case len(args) == 1 && args[0] == "list":
		return func(ctx context.Context, st *store.Store) error { return secretList(ctx, st, stdout) }
	}
	return nil
}

func secretSet(ctx context.Context, st *store.Store, data, name string, in secretInput, stdin io.Reader, stdout io.Writer) error {
	if err := config.CheckSecretName(name); err != nil {
		return refused(err.Error())
	}
	kind, user, plain, err := in.read(stdin)
	if err != nil {
		return err
	}
	if in.scopes != nil {
		if err := config.CheckScopes(in.scopes); err != nil {
			return refused(err.Error())
		}
	}
	key, err := secretbox.LoadOrCreateKey(filepath.Join(data, keyFile))
	if err != nil {
		return err
	}
	sealed, err := secretbox.Seal(key, name, plain)
	if err != nil {
		return err
	}
	detail := "replaced"
	if _, err := st.Secret(ctx, name); errors.Is(err, store.ErrNotFound) {
		detail = "new"
	} else if err != nil {
		return err
	}
	if err := st.PutSecretKind(ctx, name, kind, user, sealed, in.scopes); err != nil {
		return err
	}
	say(stdout, "%s: set", name)
	note := ""
	if in.scopes != nil {
		note = " scopes " + strings.Join(in.scopes, ",")
	}
	return record(ctx, st, "secret.set", name, detail+" "+kind+note)
}

// secretScope replaces the scopes of the secret name and leaves its value.
func secretScope(ctx context.Context, st *store.Store, name string, scopes []string, stdout io.Writer) error {
	if err := config.CheckScopes(scopes); err != nil {
		return refused(err.Error())
	}
	if err := st.SetSecretScopes(ctx, name, scopes); errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no secret called %s", name)
	} else if err != nil {
		return err
	}
	say(stdout, "%s: scopes %s", name, strings.Join(scopes, ", "))
	return record(ctx, st, "secret.scope", name, strings.Join(scopes, ","))
}

// read is the secret the flags and stdin give: its kind, its user, and
// what is sealed of it.
func (in secretInput) read(stdin io.Reader) (kind, user string, plain []byte, err error) {
	switch {
	case in.file != "" && in.user != "":
		return "", "", nil, refused("--file and --user set different kinds of secret: give one")
	case in.file != "":
		plain, err = secretFile(in.file, stdin)
		return config.SecretFile, "", plain, err
	case in.user != "":
		if err := config.CheckSecretUser(in.user); err != nil {
			return "", "", nil, refused(err.Error())
		}
		value, err := secretValue(stdin)
		return config.SecretUserPass, in.user, []byte(value), err
	}
	value, err := secretValue(stdin)
	return config.SecretText, "", []byte(value), err
}

// secretFile is the bytes of the file at path, or of stdin for -.
func secretFile(path string, stdin io.Reader) ([]byte, error) {
	r := stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	data, err := io.ReadAll(io.LimitReader(r, config.MaxSecretFile+1))
	if err != nil {
		return nil, err
	}
	if err := config.CheckSecretFile(data); err != nil {
		return nil, refused(err.Error())
	}
	return data, nil
}

// secretValue is all of stdin without the newline that ends it.
func secretValue(stdin io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(stdin, config.MaxSecret+1))
	if err != nil {
		return "", err
	}
	value := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if err := config.CheckSecretValue(value); err != nil {
		return "", refused(err.Error())
	}
	return value, nil
}

func secretUnset(ctx context.Context, st *store.Store, name string) error {
	if _, err := st.Secret(ctx, name); errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no secret called %s", name)
	} else if err != nil {
		return err
	}
	if err := st.DeleteSecret(ctx, name); err != nil {
		return err
	}
	return record(ctx, st, "secret.delete", name, "")
}

func secretList(ctx context.Context, st *store.Store, stdout io.Writer) error {
	infos, err := st.SecretInfos(ctx)
	if err != nil {
		return err
	}
	for _, info := range infos {
		say(stdout, "%s  %s  %s", info.Name, info.Kind, strings.Join(info.Scopes, ","))
	}
	return nil
}
