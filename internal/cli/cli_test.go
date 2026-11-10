package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/secretbox"
	"github.com/ndthuan/kipp/internal/store"
	"github.com/ndthuan/kipp/internal/waitfor"
)

// asKipp makes the test binary behave as kipp itself, for the tests that
// need a real process to signal.
const asKipp = "KIPP_TEST_AS_KIPP"

func TestMain(m *testing.M) {
	if os.Getenv(asKipp) == "1" {
		os.Exit(Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	tailEvery = 20 * time.Millisecond
	os.Exit(m.Run())
}

// syncBuffer is a buffer a command writes while the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func verifies(t *testing.T, hash, pw string) bool {
	t.Helper()
	ok, err := auth.VerifyPassword(context.Background(), hash, pw)
	must(t, err)
	return ok
}

// kipp runs one command in this process.
func kipp(stdin string, args ...string) (code int, out, errOut string) {
	var o, e bytes.Buffer
	code = Main(args, strings.NewReader(stdin), &o, &e)
	return code, o.String(), e.String()
}

// ok runs a command that must succeed and returns what it printed.
func ok(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	code, out, errOut := kipp(stdin, args...)
	if code != 0 {
		t.Fatalf("kipp %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), code, out, errOut)
	}
	return out
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func eq[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

func contains(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Fatalf("%q is not in %q", sub, s)
		}
	}
}

func lacks(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			t.Fatalf("%q is in %q", sub, s)
		}
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	waitfor.Until(t, what, ok)
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0o750))
	must(t, os.WriteFile(path, []byte(content), mode))
	must(t, os.Chmod(path, mode)) // the mode asked for, whatever the umask of who runs the tests
}

// newData is a data directory as a server leaves it: with a database and
// nothing in it. Only serve makes one, and a test of another command does
// not need a server for it.
func newData(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	st, err := openStore(dir, true)
	must(t, err)
	must(t, st.Close())
	return dir
}

func openDB(t *testing.T, data string) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(data, "db.sqlite"))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestVersionPrintsAndExitsZero(t *testing.T) {
	code, out, errOut := kipp("", "version")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if !strings.HasPrefix(out, "kipp ") {
		t.Fatalf("stdout %q", out)
	}
}

func TestUnknownCommandExitsTwoWithUsage(t *testing.T) {
	code, _, errOut := kipp("", "frobnicate")
	eq(t, "exit", code, 2)
	contains(t, errOut, `no command called "frobnicate"`, "serve")
}

func TestNoCommandOrAskingForHelpPrintsTheUsageAndIsNotAFailure(t *testing.T) {
	for _, args := range [][]string{nil, {"help"}, {"-h"}, {"--help"}} {
		code, out, errOut := kipp("", args...)
		what := strings.Join(args, " ")
		eq(t, what+": exit", code, 0)
		eq(t, what+": stdout", out, usage)
		eq(t, what+": stderr", errOut, "")
	}
}

func TestEveryCommandInTheUsageExists(t *testing.T) {
	for _, name := range []string{"serve", "check", "migrate", "reload", "quiet", "upgrade", "run", "user", "token", "secret", "engine", "runner", "metrics", "cache", "version"} {
		if commands[name] == nil {
			t.Errorf("no command %q", name)
		}
		contains(t, usage, "\n  "+name+" ")
	}
	eq(t, "commands", len(commands), 15)
}

func TestHelpIsNotAFailureAndABadFlagIs(t *testing.T) {
	for _, cmd := range []string{"serve", "check", "migrate", "run", "reload", "quiet", "upgrade", "user", "token", "secret", "metrics", "cache", "version"} {
		code, out, errOut := kipp("", cmd, "-h")
		eq(t, cmd+" -h", code, 0)
		contains(t, out+errOut, "usage: kipp "+cmd)
		code, _, errOut = kipp("", cmd, "--no-such-flag")
		eq(t, cmd+" with a bad flag", code, 2)
		contains(t, errOut, "no-such-flag")
	}
}

func TestCheckPassesTheSamplesAndRefusesABadDir(t *testing.T) {
	code, out, errOut := kipp("", "check", "../../testdata/jobs")
	if code != 0 || out != "ok: 3 jobs\n" {
		t.Fatal(code, out, errOut)
	}
	bad := t.TempDir()
	write(t, filepath.Join(bad, "x.yml"), "name: y\n", 0o600)
	write(t, filepath.Join(bad, "z.yml"), "name: z\nstages: {}\n", 0o600)
	code, out, errOut = kipp("", "check", bad)
	eq(t, "exit", code, 1)
	eq(t, "stdout", out, "")
	contains(t, errOut, "x.yml", "z.yml")

	code, _, errOut = kipp("", "check", filepath.Join(bad, "missing"))
	eq(t, "a directory that is not there", code, 1)
	contains(t, errOut, "missing")
	code, _, errOut = kipp("", "check")
	eq(t, "no directory", code, 2)
	contains(t, errOut, "usage: kipp check")
}

func TestFlagsMayStandAnywhereAmongTheArguments(t *testing.T) {
	data := newData(t)
	ok(t, "a password long enough\n", "user", "--data", data, "add", "--password-stdin", "before")
	ok(t, "a password long enough\n", "user", "add", "after", "--password-stdin", "--data", data)
	contains(t, ok(t, "", "user", "list", "--data", data), "after  user\n", "before  user\n")
}

func TestUsersAndTheirPermissions(t *testing.T) {
	data := newData(t)
	out := ok(t, "", "user", "add", "admin", "--admin", "--data", data)
	generated, found := strings.CutPrefix(strings.TrimSpace(out), "admin password: ")
	if !found || len(generated) < 20 {
		t.Fatalf("a generated password is printed once: %q", out)
	}
	st := openDB(t, data)
	ctx := context.Background()
	hashOf := func(name string) string {
		u, err := st.UserByName(ctx, name)
		must(t, err)
		return u.Hash
	}
	eq(t, "the generated password logs in", verifies(t, hashOf("admin"), generated), true)

	out = ok(t, "from stdin, long enough\n", "user", "add", "runner", "--data", data, "--password-stdin")
	lacks(t, out, "from stdin, long enough")
	eq(t, "the given password logs in", verifies(t, hashOf("runner"), "from stdin, long enough"), true)

	ok(t, "", "user", "grant", "runner", "deploy", "view,cancel,build,build", "--data", data)
	ok(t, "", "user", "grant", "runner", "other", "view", "--data", data)
	eq(t, "list", ok(t, "", "user", "list", "--data", data),
		"admin  admin\nrunner  user\nrunner  deploy: view,build,cancel\nrunner  other: view\n")
	ok(t, "", "user", "revoke", "runner", "other", "--data", data)
	lacks(t, ok(t, "", "user", "list", "--data", data), "other")

	ok(t, "changed it to a new one\n", "user", "passwd", "runner", "--password-stdin", "--data", data)
	eq(t, "the old password", verifies(t, hashOf("runner"), "from stdin, long enough"), false)
	eq(t, "the new password", verifies(t, hashOf("runner"), "changed it to a new one"), true)
	out = ok(t, "", "user", "passwd", "runner", "--data", data)
	generated, _ = strings.CutPrefix(strings.TrimSpace(out), "runner password: ")
	eq(t, "a generated replacement", verifies(t, hashOf("runner"), generated), true)

	for what, c := range map[string]struct {
		stdin string
		args  []string
		code  int
		says  string
	}{
		"a second user of the same name":  {"", []string{"user", "add", "runner"}, 1, `"runner" already exists`},
		"a password that is not there":    {"\n", []string{"user", "add", "nobody", "--password-stdin"}, 2, "no password on stdin"},
		"a password of 14 characters":     {"fourteen chars\n", []string{"user", "add", "nobody", "--password-stdin"}, 2, "at least 15 characters"},
		"a new password too short":        {"short\n", []string{"user", "passwd", "runner", "--password-stdin"}, 2, "at least 15 characters"},
		"a common password":               {"1q2w3e4r5t6y7u8i\n", []string{"user", "add", "nobody", "--password-stdin"}, 2, "common or leaked"},
		"the user's name in the password": {"Runner, long enough\n", []string{"user", "passwd", "runner", "--password-stdin"}, 2, `"runner"`},
		"kipp in the password":            {"my kipp password\n", []string{"user", "add", "nobody", "--password-stdin"}, 2, `"kipp"`},
		"a name a job could not have":     {"", []string{"user", "add", "No Body"}, 2, `"No Body" is not a name a user can have`},
		"a name too long":                 {"", []string{"user", "add", strings.Repeat("n", 65)}, 2, "is not a name a user can have"},
		"a permission that is not one":    {"", []string{"user", "grant", "runner", "x", "view,admin"}, 2, `"admin" is not view, build or cancel`},
		"a grant to nobody":               {"", []string{"user", "grant", "ghost", "x", "view"}, 1, `no user called "ghost"`},
		"a password for nobody":           {"", []string{"user", "passwd", "ghost"}, 1, `no user called "ghost"`},
		"a subcommand that is not one":    {"", []string{"user", "rename", "runner"}, 2, "usage: kipp user"},
		"no subcommand":                   {"", []string{"user"}, 2, "usage: kipp user"},
	} {
		code, _, errOut := kipp(c.stdin, append(c.args, "--data", data)...)
		eq(t, what, code, c.code)
		contains(t, errOut, c.says)
	}
	code, _, errOut := kipp("", "user", "list")
	eq(t, "no data directory", code, 2)
	contains(t, errOut, "--data is required")
	lacks(t, ok(t, "", "user", "list", "--data", data), "nobody", "ghost", "No Body", "nnnn")
	eq(t, "a refused password changed nothing", verifies(t, hashOf("runner"), generated), true)
	ok(t, "fifteen chars!!\n", "user", "passwd", "runner", "--password-stdin", "--data", data)
}

func TestAUserWhoseNameANewUserCouldNotHaveStillWorksForEveryOtherSubcommand(t *testing.T) {
	data := newData(t)
	st := openDB(t, data)
	_, err := st.CreateUser(context.Background(), "Old Name", "", false)
	must(t, err)
	must(t, st.Close())
	ok(t, "", "user", "grant", "Old Name", "deploy", "view", "--data", data)
	contains(t, ok(t, "", "user", "list", "--data", data), "Old Name  user\n", "Old Name  deploy: view\n")
	ok(t, "a new password, long\n", "user", "passwd", "Old Name", "--password-stdin", "--data", data)
	ok(t, "", "token", "create", "Old Name", "--label", "ci", "--data", data)
	ok(t, "", "user", "revoke", "Old Name", "deploy", "--data", data)
	ok(t, "", "user", "delete", "Old Name", "--data", data)
	lacks(t, ok(t, "", "user", "list", "--data", data), "Old Name")
}

func TestTokensAreShownOnceAndRevokedById(t *testing.T) {
	data := newData(t)
	ok(t, "a password long enough\n", "user", "add", "runner", "--password-stdin", "--data", data)
	out := ok(t, "", "token", "create", "runner", "--label", "ci", "--data", data)
	token := strings.TrimSpace(out[strings.LastIndex(out, "kipp_"):])
	st := openDB(t, data)
	u, err := st.UserByTokenHash(context.Background(), auth.HashToken(token))
	must(t, err)
	eq(t, "the token's user", u.Name, "runner")

	list := ok(t, "", "token", "list", "runner", "--data", data)
	contains(t, list, "1  ci  created ", "last used ")
	lacks(t, list, token, auth.HashToken(token))

	ok(t, "", "token", "revoke", "1", "--data", data)
	if _, err := st.UserByTokenHash(context.Background(), auth.HashToken(token)); err == nil {
		t.Fatal("a revoked token still works")
	}
	eq(t, "list after revoke", ok(t, "", "token", "list", "runner", "--data", data), "")

	for what, c := range map[string]struct {
		args []string
		code int
		says string
	}{
		"a token nobody holds":       {[]string{"token", "revoke", "1"}, 1, "no token with id 1"},
		"an id that is not a number": {[]string{"token", "revoke", "kipp_x"}, 2, "the id is the number"},
		"a token with no label":      {[]string{"token", "create", "runner"}, 2, "usage: kipp token"},
		"a token for nobody":         {[]string{"token", "create", "ghost", "--label", "x"}, 1, `no user called "ghost"`},
		"a list for nobody":          {[]string{"token", "list", "ghost"}, 1, `no user called "ghost"`},
	} {
		code, _, errOut := kipp("", append(c.args, "--data", data)...)
		eq(t, what, code, c.code)
		contains(t, errOut, c.says)
	}
}

func TestSecretsAreSealedAndListedByNameOnly(t *testing.T) {
	data := newData(t)
	const value = "longenoughvalue"
	out := ok(t, value+"\n", "secret", "set", "TOKEN", "--data", data)
	lacks(t, out, value)
	ok(t, "another value", "secret", "set", "lower_case", "--data", data)
	eq(t, "list", ok(t, "", "secret", "list", "--data", data), "TOKEN  text  global\nlower_case  text  global\n")

	st := openDB(t, data)
	sealed, err := st.Secret(context.Background(), "TOKEN")
	must(t, err)
	if bytes.Contains(sealed, []byte(value)) {
		t.Fatal("the database holds the secret in the clear")
	}
	key, err := secretbox.LoadOrCreateKey(filepath.Join(data, "secret.key"))
	must(t, err)
	plain, err := secretbox.Open(key, "TOKEN", sealed)
	must(t, err)
	eq(t, "what the server's key opens", string(plain), value)

	ok(t, "", "secret", "unset", "TOKEN", "--data", data)
	eq(t, "list after unset", ok(t, "", "secret", "list", "--data", data), "lower_case  text  global\n")

	for what, c := range map[string]struct {
		stdin string
		args  []string
		code  int
		says  string
	}{
		"a short value":              {"abc\n", []string{"secret", "set", "SHORT"}, 2, "at least 8 characters"},
		"a value of two lines":       {"first line\nsecond line\n", []string{"secret", "set", "TWO"}, 2, "one line"},
		"a name no shell accepts":    {value, []string{"secret", "set", "NOT-A-NAME"}, 2, `"NOT-A-NAME"`},
		"a value as an argument":     {"", []string{"secret", "set", "ARG", value}, 2, "usage: kipp secret"},
		"a secret that is not there": {"", []string{"secret", "unset", "TOKEN"}, 1, "no secret called TOKEN"},
	} {
		code, out, errOut := kipp(c.stdin, append(c.args, "--data", data)...)
		eq(t, what, code, c.code)
		contains(t, errOut, c.says)
		lacks(t, out+errOut, value, "abc", "first line")
	}
	eq(t, "nothing refused was stored", ok(t, "", "secret", "list", "--data", data), "lower_case  text  global\n")
}

func TestTheDatabaseIsItsOwnersOnly(t *testing.T) {
	data := newData(t)
	ok(t, "a password long enough\n", "user", "add", "runner", "--password-stdin", "--data", data)
	files, err := filepath.Glob(filepath.Join(data, "db.sqlite*"))
	must(t, err)
	if len(files) == 0 {
		t.Fatal("no database")
	}
	for _, f := range files {
		info, err := os.Stat(f)
		must(t, err)
		eq(t, filepath.Base(f), info.Mode().Perm(), os.FileMode(0o600))
	}
	code, _, errOut := kipp("", "user", "list", "--data", filepath.Join(data, "typo"))
	eq(t, "a data directory that is not there", code, 1)
	contains(t, errOut, "typo")
	if _, err := os.Stat(filepath.Join(data, "typo")); err == nil {
		t.Fatal("a mistyped --data made a new data directory")
	}
}

func TestAUserIsDeletedWithAllTheyHeldAndTheOnlyAdminLeftNever(t *testing.T) {
	data := newData(t)
	ok(t, "", "user", "add", "admin", "--admin", "--data", data)
	ok(t, "", "user", "add", "second", "--admin", "--data", data)
	ok(t, "", "user", "add", "runner", "--data", data)
	ok(t, "", "user", "grant", "runner", "smoke", "view,build", "--data", data)
	ok(t, "", "token", "create", "runner", "--label", "ci", "--data", data)
	st := openDB(t, data)
	ctx := context.Background()
	runner, err := st.UserByName(ctx, "runner")
	must(t, err)

	eq(t, "what a delete says", ok(t, "", "user", "delete", "runner", "--data", data), "runner: deleted\n")
	lacks(t, ok(t, "", "user", "list", "--data", data), "runner")
	tokens, err := st.Tokens(ctx, runner.ID)
	must(t, err)
	perms, err := st.Permissions(ctx, runner.ID)
	must(t, err)
	eq(t, "what is left of the user's tokens and grants", len(tokens)+len(perms), 0)

	// One admin of two may go; the one that is left may not.
	ok(t, "", "user", "delete", "second", "--data", data)
	for what, c := range map[string]struct {
		name, says string
	}{
		"the last admin": {"admin", "admin is the last admin"},
		"nobody":         {"ghost", `no user called "ghost"`},
	} {
		code, _, errOut := kipp("", "user", "delete", c.name, "--data", data)
		eq(t, what, code, 1)
		contains(t, errOut, c.says)
	}
	eq(t, "the list after it all", ok(t, "", "user", "list", "--data", data), "admin  admin\n")
}

func TestAGrantNamesWhatAJobCouldBeCalledAndIsCheckedAgainstTheJobFilesWhenAsked(t *testing.T) {
	data, jobs := newData(t), t.TempDir()
	write(t, filepath.Join(jobs, "smoke.yml"), "name: smoke\nstages:\n  s: {run: 'true'}\n", 0o600)
	ok(t, "", "user", "add", "runner", "--data", data)

	code, _, errOut := kipp("", "user", "grant", "runner", "Not A Job", "view", "--data", data)
	eq(t, "a name no job can have", code, 2)
	contains(t, errOut, `"Not A Job"`, "lower-case letters, digits")

	lacks(t, ok(t, "", "user", "grant", "runner", "smoke", "view", "--data", data, "--jobs", jobs), "not checked")
	code, _, errOut = kipp("", "user", "grant", "runner", "smok", "view", "--data", data, "--jobs", jobs)
	eq(t, "a job that is not in the job files", code, 1)
	contains(t, errOut, `no job called "smok" in `+jobs)

	// Without the job files the grant is taken, and says it was not checked.
	contains(t, ok(t, "", "user", "grant", "runner", "later", "view,build", "--data", data), "not checked against the job files")
	eq(t, "list", ok(t, "", "user", "list", "--data", data), "runner  user\nrunner  later: view,build\nrunner  smoke: view\n")

	write(t, filepath.Join(jobs, "bad.yml"), "name: other\n", 0o600)
	code, _, errOut = kipp("", "user", "grant", "runner", "smoke", "view,build", "--data", data, "--jobs", jobs)
	eq(t, "job files that do not load", code, 1)
	contains(t, errOut, "bad.yml")

	// A grant made before names were checked can still be taken back.
	st := openDB(t, data)
	runner, err := st.UserByName(context.Background(), "runner")
	must(t, err)
	must(t, st.Grant(context.Background(), runner.ID, "Not A Job", []string{"view"}))
	ok(t, "", "user", "revoke", "runner", "Not A Job", "--data", data)
	lacks(t, ok(t, "", "user", "list", "--data", data), "Not A Job")
}

// One job is "1 job". Every other count keeps the form scripts match.
func TestAJobCountIsSingularOnlyForOne(t *testing.T) {
	for n, want := range map[int]string{0: "0 jobs", 1: "1 job", 2: "2 jobs", 11: "11 jobs"} {
		eq(t, "count", jobCount(n), want)
	}
}

func TestAFileAndAUserAndPasswordAreSecretsToo(t *testing.T) {
	data := newData(t)
	pemFile := filepath.Join(t.TempDir(), "id.pem")
	bytesIn := []byte("\x00\xff binary key\nline two of the key\n")
	must(t, os.WriteFile(pemFile, bytesIn, 0o600))
	ok(t, "", "secret", "set", "KEYFILE", "--file", pemFile, "--data", data)
	ok(t, "from stdin\x00bytes", "secret", "set", "STDINFILE", "--file", "-", "--data", data)
	ok(t, "a password long enough\n", "secret", "set", "LOGIN", "--user", "deployer", "--data", data)
	ok(t, "a value long enough", "secret", "set", "PLAIN", "--data", data)
	eq(t, "list", ok(t, "", "secret", "list", "--data", data), "KEYFILE  file  global\nLOGIN  userpass  global\nPLAIN  text  global\nSTDINFILE  file  global\n")

	st := openDB(t, data)
	key, err := secretbox.LoadOrCreateKey(filepath.Join(data, "secret.key"))
	must(t, err)
	rec, err := st.SecretRecord(context.Background(), "KEYFILE")
	must(t, err)
	plain, err := secretbox.Open(key, "KEYFILE", rec.Sealed)
	must(t, err)
	if rec.Kind != "file" || !bytes.Equal(plain, bytesIn) {
		t.Fatalf("%+v opens to %q", rec.Kind, plain)
	}
	rec, err = st.SecretRecord(context.Background(), "LOGIN")
	must(t, err)
	plain, err = secretbox.Open(key, "LOGIN", rec.Sealed)
	must(t, err)
	if rec.Kind != "userpass" || rec.User != "deployer" || string(plain) != "a password long enough" {
		t.Fatalf("%+v opens to %q", rec, plain)
	}
	// Setting again changes the kind.
	ok(t, "a value long enough", "secret", "set", "KEYFILE", "--data", data)
	rec, err = st.SecretRecord(context.Background(), "KEYFILE")
	must(t, err)
	if rec.Kind != "text" {
		t.Fatalf("kind %q after setting a value over a file", rec.Kind)
	}
}

func TestASecretThatBreaksTheRuleOfItsKindIsRefusedWithoutTheValue(t *testing.T) {
	data := newData(t)
	dir := t.TempDir()
	empty, big := filepath.Join(dir, "empty"), filepath.Join(dir, "big")
	must(t, os.WriteFile(empty, nil, 0o600))
	must(t, os.WriteFile(big, bytes.Repeat([]byte("k"), config.MaxSecretFile+1), 0o600))
	const password = "a-password-of-two\nlines"
	for what, c := range map[string]struct {
		stdin string
		args  []string
		says  string
	}{
		"an empty file":               {"", []string{"secret", "set", "F", "--file", empty}, "not empty"},
		"a file over a megabyte":      {"", []string{"secret", "set", "F", "--file", big}, "at most 1048576 bytes"},
		"an empty file from stdin":    {"", []string{"secret", "set", "F", "--file", "-"}, "not empty"},
		"a file and a user":           {"x", []string{"secret", "set", "F", "--file", empty, "--user", "u"}, "give one"},
		"a password of two lines":     {password, []string{"secret", "set", "U", "--user", "deployer"}, "one line"},
		"a user with a colon":         {"a password long enough", []string{"secret", "set", "U", "--user", "a:b"}, "no colon"},
		"a text value with a newline": {"first line\nsecond line", []string{"secret", "set", "T"}, "one line"},
		"a file that is not there":    {"", []string{"secret", "set", "F", "--file", filepath.Join(dir, "none")}, "no such file"},
	} {
		code, out, errOut := kipp(c.stdin, append(c.args, "--data", data)...)
		if code == 0 {
			t.Errorf("%s: set", what)
		}
		contains(t, errOut, c.says)
		lacks(t, out+errOut, "a-password-of-two", "first line", "kkkkkkkk")
	}
	eq(t, "nothing refused was stored", ok(t, "", "secret", "list", "--data", data), "")
}

func TestASecretIsSetShownAndRescopedFromTheCommandLineAndEveryChangeIsAudited(t *testing.T) {
	data := newData(t)
	const value = "an obvious test value"
	ok(t, value, "secret", "set", "TOKEN", "--data", data)
	eq(t, "a new secret is global", ok(t, "", "secret", "list", "--data", data), "TOKEN  text  global\n")
	ok(t, "", "secret", "scope", "TOKEN", "--scope", "group:site", "--scope", "job:acme-*", "--data", data)
	eq(t, "rescoped", ok(t, "", "secret", "list", "--data", data), "TOKEN  text  group:site,job:acme-*\n")
	ok(t, "another test value", "secret", "set", "TOKEN", "--data", data)
	eq(t, "a value set again keeps the scopes", ok(t, "", "secret", "list", "--data", data), "TOKEN  text  group:site,job:acme-*\n")
	ok(t, value, "secret", "set", "OTHER", "--scope", "job:site-smoke", "--data", data)
	eq(t, "set with a scope", ok(t, "", "secret", "list", "--data", data), "OTHER  text  job:site-smoke\nTOKEN  text  group:site,job:acme-*\n")
	for _, args := range [][]string{
		{"secret", "scope", "TOKEN", "--data", data},
		{"secret", "scope", "TOKEN", "--scope", "job:[", "--data", data},
		{"secret", "scope", "NONE", "--scope", "global", "--data", data},
		{"secret", "set", "BAD", "--scope", "nobody", "--data", data},
	} {
		if code, _, _ := kipp(value, args...); code == 0 {
			t.Errorf("kipp %s was accepted", strings.Join(args, " "))
		}
	}
	eq(t, "nothing refused changed anything", ok(t, "", "secret", "list", "--data", data), "OTHER  text  job:site-smoke\nTOKEN  text  group:site,job:acme-*\n")
	got := audit(t, openDB(t, data))
	want := []string{
		"cli secret.set TOKEN: new text",
		"cli secret.scope TOKEN: group:site,job:acme-*",
		"cli secret.set TOKEN: replaced text",
		"cli secret.set OTHER: new text scopes job:site-smoke",
	}
	eq(t, "the audit", strings.Join(got, "\n"), strings.Join(want, "\n"))
	lacks(t, strings.Join(got, "\n"), value)
}

func TestAGrantOfBuildOrCancelWithoutViewIsRefusedWithTheReason(t *testing.T) {
	data := newData(t)
	ok(t, "", "user", "add", "runner", "--data", data)
	for _, perms := range []string{"build", "cancel", "build,cancel"} {
		code, _, errOut := kipp("", "user", "grant", "runner", "smoke", perms, "--data", data)
		eq(t, perms, code, 2)
		contains(t, errOut, "build and cancel need view too: smoke has no view")
	}
	eq(t, "nothing was stored", ok(t, "", "user", "list", "--data", data), "runner  user\n")
}
