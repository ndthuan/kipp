package cachesrv

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/apispec/apispectest"
	"github.com/ndthuan/kipp/internal/cache"
	"github.com/ndthuan/kipp/internal/problem"
	"github.com/ndthuan/kipp/internal/store"
)

var ctx = context.Background()

type rig struct {
	t   *testing.T
	st  *store.Store
	svc *Service
	srv *httptest.Server
	now time.Time
}

func newRig(t *testing.T, mod ...func(*Options)) *rig {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	r := &rig{t: t, st: st, now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	o := Options{Dir: filepath.Join(t.TempDir(), "cache"), Store: st, Now: func() time.Time { return r.now }}
	for _, m := range mod {
		m(&o)
	}
	r.svc = New(o)
	r.srv = httptest.NewServer(apispectest.Wrap(t, r.svc.Handler()))
	t.Cleanup(r.srv.Close)
	return r
}

// stage is a stage's view of the cache: the token of a build and a directory
// and home of its own.
type stage struct {
	r         *rig
	token     string
	scope     string
	dir, home string
}

func (r *rig) stage(build int64, ns cache.Namespace, scopes ...string) *stage {
	return &stage{r: r, token: r.svc.Mint(Grant{Build: build, Scopes: scopes, Namespace: ns}), scope: scopes[0],
		dir: r.t.TempDir(), home: r.t.TempDir()}
}

func (s *stage) run(args ...string) (code int, stderr string) {
	var out, errb bytes.Buffer
	env := map[string]string{cache.URLEnv: s.r.srv.URL, cache.TokenEnv: s.token, cache.ScopeEnv: s.scope}
	code = cache.Command(args, cache.Env{
		Getenv: func(k string) string { return env[k] }, Dir: s.dir, Home: s.home, Stdout: &out, Stderr: &errb,
	})
	return code, errb.String()
}

func (s *stage) write(name, content string) {
	s.r.t.Helper()
	p := filepath.Join(s.dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		s.r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		s.r.t.Fatal(err)
	}
}

// payload is the file the tests keep in the cached directory.
func (s *stage) payload() string {
	b, _ := os.ReadFile(filepath.Join(s.dir, "mods/x"))
	return string(b)
}

func (s *stage) save(key string) int {
	s.r.t.Helper()
	code, msg := s.run("save", "--key", key, "--path", "mods")
	if code != 0 {
		s.r.t.Fatalf("save %s: exit %d: %s", key, code, msg)
	}
	return code
}

func TestAnExactKeyIsRestoredAndExitsZero(t *testing.T) {
	r := newRig(t)
	a := r.stage(1, cache.Default, "acme")
	a.write("mods/x", "payload")
	a.save("mods-v1")
	b := r.stage(2, cache.Default, "acme")
	code, msg := b.run("restore", "--key", "mods-v1", "--path", "mods")
	if code != 0 || !strings.Contains(msg, "mods-v1") {
		t.Fatalf("exit %d: %s", code, msg)
	}
	if b.payload() != "payload" {
		t.Fatalf("restored %q", b.payload())
	}
}

func TestAPrefixHitRestoresTheNewestAndExitsOne(t *testing.T) {
	r := newRig(t)
	a := r.stage(1, cache.Default, "acme")
	a.write("mods/x", "old")
	a.save("mods-1")
	r.now = r.now.Add(time.Hour)
	a.write("mods/x", "new")
	a.save("mods-2")
	b := r.stage(2, cache.Default, "acme")
	code, msg := b.run("restore", "--key", "mods-9", "--restore-key", "nope-", "--restore-key", "mods-", "--path", "mods")
	if code != 1 || !strings.Contains(msg, "mods-2") {
		t.Fatalf("exit %d: %s", code, msg)
	}
	if b.payload() != "new" {
		t.Fatalf("restored %q, want the newest", b.payload())
	}
}

func TestNoHitExitsOneAndRestoresNothing(t *testing.T) {
	r := newRig(t)
	b := r.stage(2, cache.Default, "acme")
	code, msg := b.run("restore", "--key", "absent", "--restore-key", "ab", "--path", "mods")
	if code != 1 || !strings.Contains(msg, "absent") {
		t.Fatalf("exit %d: %s", code, msg)
	}
	if _, err := os.Stat(filepath.Join(b.dir, "mods")); err == nil {
		t.Fatal("a miss restored something")
	}
}

func TestASavedKeyIsNeverOverwritten(t *testing.T) {
	r := newRig(t)
	a := r.stage(1, cache.Default, "acme")
	a.write("mods/x", "first")
	a.save("k")
	a.write("mods/x", "second")
	code, msg := a.run("save", "--key", "k", "--path", "mods")
	if code != 0 || !strings.Contains(msg, "already exists") {
		t.Fatalf("exit %d: %s", code, msg)
	}
	b := r.stage(2, cache.Default, "acme")
	b.run("restore", "--key", "k", "--path", "mods")
	if b.payload() != "first" {
		t.Fatalf("restored %q, the first save must stay", b.payload())
	}
}

func TestTwoSavesOfOneKeyAtOnceLeaveOneWholeEntry(t *testing.T) {
	r := newRig(t)
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Go(func() {
			s := r.stage(int64(i+1), cache.Default, "acme")
			s.write("mods/x", strings.Repeat("v", 100000+i))
			if code, msg := s.run("save", "--key", "race", "--path", "mods"); code != 0 {
				t.Errorf("save %d: exit %d: %s", i, code, msg)
			}
		})
	}
	wg.Wait()
	all, err := r.st.CacheEntries(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("entries %v %v, want one", all, err)
	}
	files, _ := filepath.Glob(filepath.Join(r.svc.o.Dir, "acme", "default", "*"))
	if len(files) != 1 || !strings.HasSuffix(files[0], "race.tar.zst") {
		t.Fatalf("files %v, want only race.tar.zst (no temp file left)", files)
	}
	b := r.stage(99, cache.Default, "acme")
	if code, msg := b.run("restore", "--key", "race", "--path", "mods"); code != 0 {
		t.Fatalf("the entry does not restore whole: exit %d: %s", code, msg)
	}
	if n := len(b.payload()); n < 100000 || n > 100005 {
		t.Fatalf("restored %d bytes, not one save's whole file", n)
	}
}

func TestABranchReadsItsOwnThenTheDefaultBranchesNeverAnothers(t *testing.T) {
	r := newRig(t)
	def := r.stage(1, cache.Default, "acme")
	def.write("mods/x", "default")
	def.save("mods-d")
	other := r.stage(2, cache.NamespaceOfBranch("other"), "acme")
	other.write("mods/x", "other")
	r.now = r.now.Add(time.Hour)
	other.save("mods-o") // newer than the default's
	mine := r.stage(3, cache.NamespaceOfBranch("mine"), "acme")
	code, msg := mine.run("restore", "--key", "mods-zz", "--restore-key", "mods-", "--path", "mods")
	if code != 1 || !strings.Contains(msg, "mods-d") || mine.payload() != "default" {
		t.Fatalf("exit %d: %s; restored %q, want the default branch's", code, msg, mine.payload())
	}
	mine.write("mods/x", "mine")
	mine.save("mods-m")
	again := r.stage(4, cache.NamespaceOfBranch("mine"), "acme")
	if code, _ := again.run("restore", "--key", "mods-m", "--path", "mods"); code != 0 || again.payload() != "mine" {
		t.Fatalf("a branch does not read its own: exit %d, %q", code, again.payload())
	}
	if code, _ := other.run("restore", "--key", "mods-m", "--path", "mods2"); code == 0 {
		t.Fatal("a branch read another branch's entry")
	}
}

func TestAJobNamespaceIsReadByThatJobOnly(t *testing.T) {
	r := newRig(t)
	a := r.stage(1, cache.NamespaceOf(false, "", "", "nightly", false), "acme")
	a.write("mods/x", "n")
	a.save("k")
	same := r.stage(2, "j-nightly", "acme")
	if code, _ := same.run("restore", "--key", "k", "--path", "mods"); code != 0 {
		t.Fatal("a job does not read its own namespace")
	}
	other := r.stage(3, "j-weekly", "acme")
	if code, _ := other.run("restore", "--key", "k", "--path", "mods"); code != 1 {
		t.Fatal("another job read the namespace")
	}
}

func TestATokenOutlivesNoBuild(t *testing.T) {
	r := newRig(t)
	a := r.stage(1, cache.Default, "acme")
	a.write("mods/x", "x")
	a.save("k")
	r.svc.Revoke(1)
	if code, msg := a.run("save", "--key", "k2", "--path", "mods"); code != 2 || !strings.Contains(msg, "401") {
		t.Fatalf("save after the build ended: exit %d: %s", code, msg)
	} else if !strings.Contains(msg, "its build has ended") || strings.Contains(msg, `"detail"`) {
		t.Fatalf("the client shows the problem's detail, not its JSON: %s", msg) // the person reads the reason
	}
	if code, msg := a.run("restore", "--key", "k", "--path", "mods2"); code != 1 || !strings.Contains(msg, "401") {
		t.Fatalf("restore after the build ended: exit %d: %s", code, msg)
	}
}

func TestATokenReachesOnlyItsStagesScopes(t *testing.T) {
	r := newRig(t)
	a := r.stage(1, cache.Default, "acme")
	a.write("mods/x", "x")
	a.save("k")
	if code, msg := a.run("save", "--scope", "other", "--key", "k", "--path", "mods"); code != 2 || !strings.Contains(msg, "403") {
		t.Fatalf("save to a scope the stage did not name: exit %d: %s", code, msg)
	}
	if code, msg := a.run("restore", "--scope", "other", "--key", "k", "--path", "mods2"); code != 1 || !strings.Contains(msg, "403") {
		t.Fatalf("restore from a scope the stage did not name: exit %d: %s", code, msg)
	}
	two := r.stage(2, cache.Default, "acme", "extra")
	if code, msg := two.run("save", "--scope", "extra", "--key", "k", "--path", "mods"); code != 2 {
		// the stage's directory is empty: a path that is not there is a failed save, not a 403
		if strings.Contains(msg, "403") {
			t.Fatalf("a scope the stage named was refused: %s", msg)
		}
	}
}

func TestAnEntryOver10GiBIsRefusedWithExitTwo(t *testing.T) {
	r := newRig(t, func(o *Options) { o.EntryMax = 2000 })
	a := r.stage(1, cache.Default, "acme")
	// Random bytes do not compress, so the entry is over the limit set.
	rnd := make([]byte, 50000)
	if _, err := rand.Read(rnd); err != nil {
		t.Fatal(err)
	}
	a.write("mods/big", string(rnd))
	code, msg := a.run("save", "--key", "big", "--path", "mods")
	if code != 2 || !strings.Contains(msg, "413") {
		t.Fatalf("exit %d: %s", code, msg)
	}
	if all, _ := r.st.CacheEntries(ctx); len(all) != 0 {
		t.Fatalf("a refused entry was kept: %v", all)
	}
	files, _ := filepath.Glob(filepath.Join(r.svc.o.Dir, "acme", "default", "*"))
	if len(files) != 0 {
		t.Fatalf("a refused entry left files: %v", files)
	}
}

func TestACacheOutageIsAMissForARestoreAndAFailureForASave(t *testing.T) {
	r := newRig(t)
	a := r.stage(1, cache.Default, "acme")
	a.write("mods/x", "x")
	r.srv.Close()
	if code, msg := a.run("restore", "--key", "k", "--path", "mods"); code != 1 || !strings.Contains(msg, "miss") {
		t.Fatalf("restore: exit %d: %s", code, msg)
	}
	if code, msg := a.run("save", "--key", "k", "--path", "mods"); code != 2 || !strings.Contains(msg, "not saved") {
		t.Fatalf("save: exit %d: %s", code, msg)
	}
}

func TestASaveOfAPathThatIsNotThereFailsWithExitTwoAndKeepsNothing(t *testing.T) {
	r := newRig(t)
	a := r.stage(1, cache.Default, "acme")
	code, msg := a.run("save", "--key", "k", "--path", "nope")
	if code != 2 || !strings.Contains(msg, "nope") {
		t.Fatalf("exit %d: %s", code, msg)
	}
	if all, _ := r.st.CacheEntries(ctx); len(all) != 0 {
		t.Fatalf("kept %v", all)
	}
}

func TestADamagedEntryRestoresAsAMiss(t *testing.T) {
	r := newRig(t)
	a := r.stage(1, cache.Default, "acme")
	a.write("mods/x", strings.Repeat("payload", 1000))
	a.save("k")
	file := filepath.Join(r.svc.o.Dir, "acme", "default", "k.tar.zst")
	b, _ := os.ReadFile(file)
	if err := os.WriteFile(file, b[:len(b)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	c := r.stage(2, cache.Default, "acme")
	if code, msg := c.run("restore", "--key", "k", "--path", "mods"); code != 1 || !strings.Contains(msg, "k") {
		t.Fatalf("exit %d: %s", code, msg)
	}
}

func TestAnEntryWhoseFileIsGoneIsAMissAndItsRowGoes(t *testing.T) {
	r := newRig(t)
	a := r.stage(1, cache.Default, "acme")
	a.write("mods/x", "x")
	a.save("k")
	if err := os.Remove(filepath.Join(r.svc.o.Dir, "acme", "default", "k.tar.zst")); err != nil {
		t.Fatal(err)
	}
	if code, _ := r.stage(2, cache.Default, "acme").run("restore", "--key", "k", "--path", "mods"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if all, _ := r.st.CacheEntries(ctx); len(all) != 0 {
		t.Fatalf("the row of a missing file stays: %v", all)
	}
	// ...so the key can be saved again.
	a.save("k")
}

func TestHashOfAMissingFileExitsTwo(t *testing.T) {
	r := newRig(t)
	a := r.stage(1, cache.Default, "acme")
	if code, _ := a.run("hash", "nope"); code != 2 {
		t.Fatalf("exit %d", code)
	}
	a.write("f", "x")
	if code, _ := a.run("hash", filepath.Join(a.dir, "f")); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

func TestCacheMaxRemovesTheLeastRecentlyRestoredFirst(t *testing.T) {
	r := newRig(t)
	a := r.stage(1, cache.Default, "acme")
	a.write("mods/x", strings.Repeat("a", 5000))
	for _, k := range []string{"k1", "k2", "k3"} {
		r.now = r.now.Add(time.Hour)
		a.save(k)
	}
	// k1 is restored last of the three, so k2 is the least recently used.
	r.now = r.now.Add(time.Hour)
	if code, _ := r.stage(2, cache.Default, "acme").run("restore", "--key", "k1", "--path", "mods"); code != 0 {
		t.Fatal("restore k1")
	}
	all, _ := r.st.CacheEntries(ctx)
	var total int64
	for _, e := range all {
		total += e.Size
	}
	keep := total - all[0].Size // room for two of the three
	n, err := r.svc.Trim(ctx, keep, 0)
	if err != nil || n != 1 {
		t.Fatalf("removed %d, %v", n, err)
	}
	left, _ := r.st.CacheEntries(ctx)
	keys := []string{left[0].Key, left[1].Key}
	if len(left) != 2 || strings.Contains(strings.Join(keys, " "), "k2") {
		t.Fatalf("left %v, want k2 gone", keys)
	}
	if _, err := os.Stat(filepath.Join(r.svc.o.Dir, "acme", "default", "k2.tar.zst")); err == nil {
		t.Fatal("the removed entry's file stays")
	}
	if rows, _ := r.st.AuditBefore(ctx, 0, 10); len(rows) == 0 {
		t.Fatal("a removal left no row in the audit")
	}
}

func TestAnEntryIdleSevenDaysIsRemoved(t *testing.T) {
	r := newRig(t)
	a := r.stage(1, cache.Default, "acme")
	a.write("mods/x", "x")
	a.save("old")
	r.now = r.now.Add(6 * 24 * time.Hour)
	a.save("young")
	r.now = r.now.Add(2 * 24 * time.Hour) // old: 8 days saved, never restored
	n, err := r.svc.Sweep(ctx)
	if err != nil || n != 1 {
		t.Fatalf("removed %d, %v", n, err)
	}
	left, _ := r.st.CacheEntries(ctx)
	if len(left) != 1 || left[0].Key != "young" {
		t.Fatalf("left %v", left)
	}
}

func TestAnEntryAndAScopeAreRemovedByHandAsTheActor(t *testing.T) {
	r := newRig(t)
	save := func(scope, key string) {
		s := r.stage(1, cache.Default, scope)
		s.write("f", key)
		if code, say := s.run("save", "--key", key, "--path", "f"); code != 0 {
			t.Fatalf("%d %s", code, say)
		}
	}
	save("site", "a")
	save("site", "b")
	save("api", "c")
	es, err := r.st.CacheEntries(ctx)
	if err != nil || len(es) != 3 {
		t.Fatalf("%v %+v", err, es)
	}
	one := es[0] // the first saved, in scope site
	file := r.svc.file(one.Scope, cache.Namespace(one.Namespace), one.Key)
	if err := r.svc.Remove(ctx, one.ID, "boss"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("the file is still there: %v", err)
	}
	if err := r.svc.Remove(ctx, one.ID, "boss"); err == nil {
		t.Fatal("an entry that is gone was removed again")
	}
	n, err := r.svc.RemoveScope(ctx, "site", "boss")
	left, _ := r.st.CacheEntries(ctx)
	if err != nil || n != 1 || len(left) != 1 || left[0].Scope != "api" {
		t.Fatalf("%v removed %d, left %+v", err, n, left)
	}
	audit, err := r.st.AuditBefore(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	by := 0
	for _, a := range audit {
		if a.Action == "cache.remove" && a.Actor == "boss" {
			by++
		}
	}
	if by != 2 {
		t.Fatalf("%d audit rows by boss for %d removals", by, 2)
	}
}

func TestTheCacheRoutesAreUnderAPIV1(t *testing.T) {
	r := newRig(t)
	for _, row := range r.svc.Routes() {
		if !strings.HasPrefix(row.Path, "/api/v1/cache/{scope}/") {
			t.Errorf("route %s", row.Pattern())
		}
	}
}

// problemOf asks the cache directly and decodes the problem it answers.
func (r *rig) problemOf(method, path, token string, body string) (int, problem.Doc, http.Header) {
	r.t.Helper()
	req, err := http.NewRequest(method, r.srv.URL+path, strings.NewReader(body))
	if err != nil {
		r.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if got := res.Header.Get("Content-Type"); got != problem.MediaType {
		r.t.Errorf("%s %s answered %q", method, path, got)
	}
	var d problem.Doc
	if err := json.NewDecoder(res.Body).Decode(&d); err != nil {
		r.t.Fatalf("%s %s: not a problem: %v", method, path, err)
	}
	return res.StatusCode, d, res.Header
}

func TestEveryFailureOfTheCacheIsAProblem(t *testing.T) {
	r := newRig(t, func(o *Options) { o.EntryMax = 10 })
	s := r.stage(1, cache.NamespaceOfBranch("main"), "mods")
	for _, c := range []struct {
		name, method, path, token, body, code string
		status                                int
	}{
		{"no token", "GET", "/api/v1/cache/mods/lookup?key=k", "", "", "unauthorized", 401},
		{"a token nobody holds", "GET", "/api/v1/cache/mods/lookup?key=k", "kippc_nope", "", "unauthorized", 401},
		{"another scope", "GET", "/api/v1/cache/other/lookup?key=k", s.token, "", "forbidden", 403},
		{"a key that is not valid", "GET", "/api/v1/cache/mods/lookup?key=a%20b", s.token, "", "bad_input", 400},
		{"a put key that is not valid", "PUT", "/api/v1/cache/mods/a%20b", s.token, "x", "bad_input", 400},
		{"nothing found", "GET", "/api/v1/cache/mods/lookup?key=nothing", s.token, "", "not_found", 404},
		{"an entry that is not a number", "GET", "/api/v1/cache/mods/entry/x", s.token, "", "not_found", 404},
		{"an entry over the most", "PUT", "/api/v1/cache/mods/big", s.token, strings.Repeat("x", 100), "too_large", 413},
		{"a path nobody serves", "GET", "/api/v1/cache/mods/nothing/here/at/all", s.token, "", "not_found", 404},
		{"a wrong method", "POST", "/api/v1/cache/mods/lookup", s.token, "", "method_not_allowed", 405},
	} {
		status, d, _ := r.problemOf(c.method, c.path, c.token, c.body)
		if status != c.status || d.Code != c.code || d.Status != c.status || d.Detail == "" {
			t.Errorf("%s: %d %+v, want %d %s", c.name, status, d, c.status, c.code)
		}
	}
	_, _, h := r.problemOf("GET", "/api/v1/cache/mods/lookup?key=k", "", "")
	if h.Get("WWW-Authenticate") != "Bearer" {
		t.Errorf("a 401 names no scheme: %v", h)
	}
	_, _, h = r.problemOf("POST", "/api/v1/cache/mods/lookup", s.token, "")
	if h.Get("Allow") != "GET, HEAD, PUT" { // lookup is also a key
		t.Errorf("Allow = %q", h.Get("Allow"))
	}
}
