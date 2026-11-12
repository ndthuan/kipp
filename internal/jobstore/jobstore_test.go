package jobstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

var ctx = context.Background()

func jobText(name, run string) string {
	return "name: " + name + "\nstages:\n  s: {run: '" + run + "'}\n"
}

var byAPI = Who{Author: "admin", How: store.HowAPI}

type fixture struct {
	t    *testing.T
	dir  string
	st   *store.Store
	jobs *Jobs
	// published counts the sets handed to Publish.
	published int
}

// newFixture is a jobs directory with files and a store, and Jobs over
// them in mode, loaded once.
func newFixture(t *testing.T, mode Mode, files map[string]string) *fixture {
	t.Helper()
	f := &fixture{t: t, dir: t.TempDir()}
	for name, text := range files {
		f.write(name, text)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	f.st = st
	f.open(mode)
	return f
}

// open is a new Jobs on the same directory and store, as a server
// started again is, loaded once.
func (f *fixture) open(mode Mode) {
	f.t.Helper()
	f.jobs = New(Options{Mode: mode, Dir: f.dir, Store: f.st, Publish: func(context.Context, *config.Snapshot) { f.published++ }})
	f.load()
}

func (f *fixture) load() Report {
	f.t.Helper()
	r, err := f.jobs.Load(ctx)
	must(f.t, err)
	return r
}

func (f *fixture) write(name, text string) {
	f.t.Helper()
	must(f.t, os.WriteFile(filepath.Join(f.dir, name), []byte(text), 0o600))
}

func (f *fixture) remove(name string) {
	f.t.Helper()
	must(f.t, os.Remove(filepath.Join(f.dir, name)))
}

func (f *fixture) etag(name string) string {
	f.t.Helper()
	d, err := f.jobs.Definition(name)
	must(f.t, err)
	return `"` + d.Hash + `"`
}

// serves fails unless the job name serves with run in its text, from origin.
func (f *fixture) serves(name, run string, origin Origin) {
	f.t.Helper()
	d, err := f.jobs.Definition(name)
	if err != nil {
		f.t.Fatalf("%s does not serve: %v", name, err)
	}
	if !strings.Contains(string(d.Text), run) || d.Origin != origin {
		f.t.Fatalf("%s serves from %q:\n%s\nwant %q from %q", name, d.Origin, d.Text, run, origin)
	}
	if !strings.Contains(f.jobs.Snapshot().Jobs[name].Stages[0].Run, run) {
		f.t.Fatalf("the set's %s runs %q, want %q", name, f.jobs.Snapshot().Jobs[name].Stages[0].Run, run)
	}
}

func (f *fixture) absent(name string) {
	f.t.Helper()
	if _, err := f.jobs.Definition(name); !errors.Is(err, ErrNoJob) {
		f.t.Fatalf("%s serves: %v", name, err)
	}
}

// waits is the conflict of name, failing when there is none.
func (f *fixture) waits(name string) Conflict {
	f.t.Helper()
	for _, c := range f.jobs.Conflicts() {
		if c.Job == name {
			return c
		}
	}
	f.t.Fatalf("no conflict for %s: %+v", name, f.jobs.Conflicts())
	return Conflict{}
}

func (f *fixture) edits() map[string]store.JobEdit {
	f.t.Helper()
	e, err := f.st.JobEdits(ctx)
	must(f.t, err)
	return e
}

func (f *fixture) versions(name string) []*store.JobVersion {
	f.t.Helper()
	v, err := f.st.JobVersions(ctx, name, 0, 100)
	must(f.t, err)
	return v
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func isErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: got %v, want %v", what, err, want)
	}
}

func TestAJobCreatedInStoreModeIsKeptInTheDatabaseAndServedAfterARestart(t *testing.T) {
	f := newFixture(t, Store, nil)
	_, err := f.jobs.Create(ctx, "made", []byte(jobText("made", "echo made")), byAPI)
	must(t, err)
	f.serves("made", "echo made", FromStore)
	f.open(Store)
	f.serves("made", "echo made", FromStore)
}

func TestAnEditOverAFileThatIsAsItWasApplies(t *testing.T) {
	f := newFixture(t, Store, map[string]string{"a.yml": jobText("a", "echo file")})
	f.serves("a", "echo file", FromFile)
	_, err := f.jobs.Replace(ctx, "a", []byte(jobText("a", "echo edit")), f.etag("a"), byAPI)
	must(t, err)
	f.serves("a", "echo edit", StoreOverFile)
	// The file reformatted, with a comment, is the same job.
	f.write("a.yml", "# reformatted\nstages:\n  s:\n    run: \"echo file\"\nname: a\n")
	f.load()
	f.serves("a", "echo edit", StoreOverFile)
	if len(f.jobs.Conflicts()) != 0 {
		t.Fatalf("conflicts: %+v", f.jobs.Conflicts())
	}
}

func TestAnEditIsRetiredOnceTheFileSaysTheSameAndNoSecondVersionIsRecorded(t *testing.T) {
	f := newFixture(t, Store, map[string]string{"a.yml": jobText("a", "echo file")})
	_, err := f.jobs.Replace(ctx, "a", []byte(jobText("a", "echo edit")), f.etag("a"), byAPI)
	must(t, err)
	f.write("a.yml", "# caught up\n"+jobText("a", "echo edit"))
	f.load()
	f.serves("a", "echo edit", FromFile)
	if len(f.edits()) != 0 || len(f.jobs.Conflicts()) != 0 {
		t.Fatalf("edits %v, conflicts %+v", f.edits(), f.jobs.Conflicts())
	}
	if v := f.versions("a"); len(v) != 2 || v[0].How != store.HowAPI {
		t.Fatalf("versions: %+v", v)
	}
}

func TestADeletionIsRetiredOnceTheFileIsGone(t *testing.T) {
	f := newFixture(t, Store, map[string]string{"a.yml": jobText("a", "echo file")})
	_, err := f.jobs.Delete(ctx, "a", f.etag("a"), byAPI)
	must(t, err)
	f.absent("a")
	f.load()
	f.absent("a")
	f.remove("a.yml")
	f.load()
	f.absent("a")
	if len(f.edits()) != 0 || len(f.jobs.Conflicts()) != 0 {
		t.Fatalf("edits %v, conflicts %+v", f.edits(), f.jobs.Conflicts())
	}
}

func TestAnEditWaitsWhenItsFileChangedAndTheFileServes(t *testing.T) {
	f := newFixture(t, Store, map[string]string{"a.yml": jobText("a", "echo file")})
	_, err := f.jobs.Replace(ctx, "a", []byte(jobText("a", "echo edit")), f.etag("a"), byAPI)
	must(t, err)
	f.write("a.yml", jobText("a", "echo changed"))
	if r := f.load(); r.Conflicts != 1 {
		t.Fatalf("report: %+v", r)
	}
	f.serves("a", "echo changed", FromFile)
	c := f.waits("a")
	if c.Reason != reasonChanged || !strings.Contains(c.Edit.Text, "echo edit") || !strings.Contains(string(c.File), "echo changed") {
		t.Fatalf("conflict: %+v", c)
	}
	if _, ok := f.edits()["a"]; !ok {
		t.Fatal("the edit was not kept")
	}
	if d, _ := f.jobs.Definition("a"); !d.Conflict {
		t.Fatal("the job does not say an edit waits")
	}
}

func TestAnEditWaitsWhenItsFileWasRemovedAndTheJobIsAbsent(t *testing.T) {
	f := newFixture(t, Store, map[string]string{"a.yml": jobText("a", "echo file")})
	_, err := f.jobs.Replace(ctx, "a", []byte(jobText("a", "echo edit")), f.etag("a"), byAPI)
	must(t, err)
	f.remove("a.yml")
	f.load()
	f.absent("a")
	if c := f.waits("a"); c.Reason != reasonRemoved || c.File != nil {
		t.Fatalf("conflict: %+v", c)
	}
}

// The way a server moves its files into the store: import them while the
// files still load, then start without the files. An imported job equal
// to its file must not be retired, or the move would lose every job.
func TestAnImportedJobEqualToAFileWaitsInTheStoreAndServesOnceTheFileIsGone(t *testing.T) {
	text := jobText("b", "echo same")
	f := newFixture(t, Store, map[string]string{"b.yml": text})
	j, err := config.ParseJob("b.yml", []byte(text))
	must(t, err)
	v := &store.JobVersion{Job: "b", Text: text, Hash: config.DecodedHash(j), Author: "cli", How: store.HowImport, At: time.Now()}
	must(t, f.st.ApplyJobChanges(ctx, store.JobChanges{Versions: []*store.JobVersion{v}, Edits: map[string]store.JobEdit{"b": {Version: v}}}))
	f.load()
	f.serves("b", "echo same", FromFile)
	if c := f.waits("b"); c.Reason != reasonSameName {
		t.Fatalf("conflict: %+v", c)
	}
	f.load()
	if _, ok := f.edits()["b"]; !ok {
		t.Fatal("the imported job was retired")
	}
	f.remove("b.yml")
	f.load()
	f.serves("b", "echo same", FromStore)
	if len(f.jobs.Conflicts()) != 0 {
		t.Fatalf("conflicts: %+v", f.jobs.Conflicts())
	}
}

func TestAFileThatAppearsBesideAJobOfTheStoreServesAndTheStoredJobWaits(t *testing.T) {
	f := newFixture(t, Store, nil)
	_, err := f.jobs.Create(ctx, "c", []byte(jobText("c", "echo stored")), byAPI)
	must(t, err)
	f.write("c.yml", jobText("c", "echo file"))
	f.load()
	f.serves("c", "echo file", FromFile)
	if c := f.waits("c"); c.Reason != reasonSameName {
		t.Fatalf("conflict: %+v", c)
	}
}

func TestAnEditTheSetRefusesOnceTheFilesChangedIsSetAsideAndTheLoadIsTaken(t *testing.T) {
	f := newFixture(t, Store, map[string]string{"up.yml": jobText("up", "echo up"), "other.yml": jobText("other", "echo other")})
	_, err := f.jobs.Create(ctx, "down", []byte("name: down\ntriggers: {after: [up]}\nstages:\n  s: {run: echo down}\n"), byAPI)
	must(t, err)
	f.remove("up.yml")
	r := f.load()
	f.absent("down")
	f.absent("up")
	f.serves("other", "echo other", FromFile)
	c := f.waits("down")
	if !strings.Contains(c.Reason, "triggers.after") || r.Conflicts != 1 {
		t.Fatalf("conflict: %+v, report %+v", c, r)
	}
	// Once the file is back the edit applies again.
	f.write("up.yml", jobText("up", "echo up"))
	f.load()
	f.serves("down", "echo down", FromStore)
}

func TestABadFileStillRefusesTheWholeLoadAndChangesNothing(t *testing.T) {
	f := newFixture(t, Store, map[string]string{"a.yml": jobText("a", "echo a")})
	f.write("bad.yml", "name: other\nstages:\n  s: {run: 'true'}\n")
	before := f.published
	_, err := f.jobs.Load(ctx)
	if _, ok := errors.AsType[*config.Errors](err); !ok || !strings.Contains(err.Error(), "bad.yml") {
		t.Fatalf("load: %v", err)
	}
	f.serves("a", "echo a", FromFile)
	if f.published != before {
		t.Fatal("a refused load published a set")
	}
}

func TestAChangeIsCheckedOnTheWholeSetAndRefusedWithEveryError(t *testing.T) {
	f := newFixture(t, Store, map[string]string{"a.yml": jobText("a", "echo a")})
	_, err := f.jobs.Create(ctx, "d", []byte("name: d\ntriggers: {after: [nope, nada]}\nstages:\n  s: {run: echo d}\n"), byAPI)
	errs, ok := errors.AsType[*config.Errors](err)
	if !ok || len(errs.Items) != 2 {
		t.Fatalf("create: %v", err)
	}
	// The name in the text must be the job's.
	_, err = f.jobs.Replace(ctx, "a", []byte(jobText("b", "echo b")), f.etag("a"), byAPI)
	if _, ok := errors.AsType[*config.Errors](err); !ok {
		t.Fatalf("a text naming another job: %v", err)
	}
	// Deleting a job another names in after is refused too.
	_, err = f.jobs.Create(ctx, "e", []byte("name: e\ntriggers: {after: [a]}\nstages:\n  s: {run: echo e}\n"), byAPI)
	must(t, err)
	_, err = f.jobs.Delete(ctx, "a", f.etag("a"), byAPI)
	if _, ok := errors.AsType[*config.Errors](err); !ok {
		t.Fatalf("deleting a job named in after: %v", err)
	}
	f.serves("a", "echo a", FromFile)
	if v := f.versions("a"); len(v) != 1 {
		t.Fatalf("a refused change was recorded: %+v", v)
	}
}

func TestAReplaceOrADeleteNeedsTheCurrentETag(t *testing.T) {
	f := newFixture(t, Store, map[string]string{"a.yml": jobText("a", "echo a")})
	text := []byte(jobText("a", "echo b"))
	_, err := f.jobs.Replace(ctx, "a", text, "", byAPI)
	isErr(t, "no If-Match", err, ErrPreconditionRequired)
	_, err = f.jobs.Delete(ctx, "a", "", byAPI)
	isErr(t, "no If-Match on a delete", err, ErrPreconditionRequired)
	_, err = f.jobs.Replace(ctx, "a", text, `"0000"`, byAPI)
	isErr(t, "a stale one", err, ErrStale)
	_, err = f.jobs.Replace(ctx, "missing", text, `"0000"`, byAPI)
	isErr(t, "no such job", err, ErrNoJob)
	old := f.etag("a")
	_, err = f.jobs.Replace(ctx, "a", text, "W/"+old, byAPI)
	must(t, err)
	_, err = f.jobs.Delete(ctx, "a", old, byAPI)
	isErr(t, "the ETag before the change", err, ErrStale)
}

func TestCreateRefusesANameTheSetHas(t *testing.T) {
	f := newFixture(t, Store, map[string]string{"a.yml": jobText("a", "echo a")})
	_, err := f.jobs.Create(ctx, "a", []byte(jobText("a", "echo b")), byAPI)
	isErr(t, "create", err, ErrExists)
}

func TestOffModeRefusesEveryChangeAndStillReads(t *testing.T) {
	f := newFixture(t, Off, map[string]string{"a.yml": jobText("a", "echo a")})
	_, err := f.jobs.Create(ctx, "n", []byte(jobText("n", "echo n")), byAPI)
	isErr(t, "create", err, ErrEditsOff)
	_, err = f.jobs.Replace(ctx, "a", []byte(jobText("a", "echo b")), f.etag("a"), byAPI)
	isErr(t, "replace", err, ErrEditsOff)
	_, err = f.jobs.Delete(ctx, "a", f.etag("a"), byAPI)
	isErr(t, "delete", err, ErrEditsOff)
	_, err = f.jobs.Restore(ctx, "a", 1, f.etag("a"), "admin")
	isErr(t, "restore", err, ErrEditsOff)
	isErr(t, "resolve", f.jobs.Resolve(ctx, "a", KeepFile, "admin"), ErrEditsOff)
	f.serves("a", "echo a", FromFile)
	if v := f.versions("a"); len(v) != 1 || v[0].How != store.HowFiles {
		t.Fatalf("versions: %+v", v)
	}
}

func TestOffModeServesTheFilesAndListsAStoredEditAsWaiting(t *testing.T) {
	f := newFixture(t, Store, map[string]string{"a.yml": jobText("a", "echo a")})
	_, err := f.jobs.Replace(ctx, "a", []byte(jobText("a", "echo edit")), f.etag("a"), byAPI)
	must(t, err)
	f.open(Off)
	f.serves("a", "echo a", FromFile)
	if c := f.waits("a"); c.Reason != reasonOff {
		t.Fatalf("conflict: %+v", c)
	}
}

func TestVersionsAreRecordedForChangesThroughKippAndForFilesALoadFindsChanged(t *testing.T) {
	f := newFixture(t, Store, map[string]string{"a.yml": jobText("a", "echo one")})
	f.load() // nothing changed: no second version
	f.write("a.yml", "# only a comment more\n"+jobText("a", "echo one"))
	f.load()
	f.write("a.yml", jobText("a", "echo two"))
	f.load()
	_, err := f.jobs.Replace(ctx, "a", []byte("# kept as written\n"+jobText("a", "echo three")), f.etag("a"), byAPI)
	must(t, err)
	v := f.versions("a")
	if len(v) != 3 {
		t.Fatalf("versions: %+v", v)
	}
	want := []struct{ how, author, run string }{{store.HowAPI, "admin", "echo three"}, {store.HowFiles, store.AuthorKipp, "echo two"}, {store.HowFiles, store.AuthorKipp, "echo one"}}
	for i, w := range want {
		if v[i].How != w.how || v[i].Author != w.author || !strings.Contains(v[i].Text, w.run) || v[i].Hash == "" {
			t.Errorf("version %d: %+v, want %+v", i, v[i], w)
		}
	}
	if !strings.HasPrefix(v[0].Text, "# kept as written\n") {
		t.Errorf("the text was not kept as written: %q", v[0].Text)
	}
	// A file removed with nothing left serving is a deletion.
	f.write("gone.yml", jobText("gone", "echo gone"))
	f.load()
	f.remove("gone.yml")
	f.load()
	if g := f.versions("gone"); len(g) != 2 || !g[0].Deleted || g[0].How != store.HowFiles {
		t.Fatalf("versions of the removed file: %+v", g)
	}
}

func TestRestoreOfADeletedJobBringsItBackAsANewVersion(t *testing.T) {
	f := newFixture(t, Store, nil)
	first, err := f.jobs.Create(ctx, "c", []byte(jobText("c", "echo first")), byAPI)
	must(t, err)
	_, err = f.jobs.Replace(ctx, "c", []byte(jobText("c", "echo second")), f.etag("c"), byAPI)
	must(t, err)
	gone, err := f.jobs.Delete(ctx, "c", f.etag("c"), byAPI)
	must(t, err)
	f.absent("c")
	_, err = f.jobs.Restore(ctx, "c", gone.ID, "", "admin")
	isErr(t, "a deletion", err, ErrDeletion)
	v, err := f.jobs.Restore(ctx, "c", first.ID, "", "admin")
	must(t, err)
	f.serves("c", "echo first", FromStore)
	if v.How != store.HowRevert || len(f.versions("c")) != 4 {
		t.Fatalf("restored %+v, versions %+v", v, f.versions("c"))
	}
	// Restoring over a job that serves is a change: it needs the ETag.
	_, err = f.jobs.Restore(ctx, "c", first.ID, "", "admin")
	isErr(t, "restore over a job", err, ErrPreconditionRequired)
	_, err = f.jobs.Restore(ctx, "c", 999, "", "admin")
	isErr(t, "no such version", err, store.ErrNotFound)
}

func TestTheSetsHashFollowsTheTextsItServes(t *testing.T) {
	f := newFixture(t, Store, map[string]string{"a.yml": jobText("a", "echo a")})
	before := f.jobs.Snapshot().Hash
	dirHash := func() string {
		snap, err := config.Load(f.dir)
		must(t, err)
		return snap.Hash
	}
	if before != dirHash() {
		t.Fatal("the set of the files alone hashes otherwise than config.Load")
	}
	_, err := f.jobs.Replace(ctx, "a", []byte(jobText("a", "echo b")), f.etag("a"), byAPI)
	must(t, err)
	if f.jobs.Snapshot().Hash == before {
		t.Fatal("the hash did not change with the job")
	}
}

func TestResolveKeepsTheFileOrMakesTheEditOverItAsItIsNow(t *testing.T) {
	f := newFixture(t, Store, map[string]string{"a.yml": jobText("a", "echo file"), "b.yml": jobText("b", "echo file")})
	for _, n := range []string{"a", "b"} {
		_, err := f.jobs.Replace(ctx, n, []byte(jobText(n, "echo edit")), f.etag(n), byAPI)
		must(t, err)
		f.write(n+".yml", jobText(n, "echo changed"))
	}
	f.load()
	isErr(t, "no conflict", f.jobs.Resolve(ctx, "zzz", KeepFile, "admin"), ErrNoConflict)
	if err := f.jobs.Resolve(ctx, "a", "both", "admin"); err == nil {
		t.Fatal("a choice that is none was taken")
	}
	must(t, f.jobs.Resolve(ctx, "a", KeepFile, "admin"))
	f.serves("a", "echo changed", FromFile)
	must(t, f.jobs.Resolve(ctx, "b", KeepEdit, "admin"))
	f.serves("b", "echo edit", StoreOverFile)
	f.load()
	f.serves("a", "echo changed", FromFile)
	f.serves("b", "echo edit", StoreOverFile)
	if len(f.jobs.Conflicts()) != 0 {
		t.Fatalf("conflicts: %+v", f.jobs.Conflicts())
	}
	// The changed file was recorded by the load that found it; keeping the
	// edit records it again, as it serves once more.
	if v := f.versions("a"); v[0].How != store.HowFiles || !strings.Contains(v[0].Text, "echo changed") {
		t.Fatalf("a's newest version: %+v", v[0])
	}
	if v := f.versions("b"); v[0].How != store.HowResolve || !strings.Contains(v[0].Text, "echo edit") {
		t.Fatalf("b's newest version: %+v", v[0])
	}
}

func TestFilesModeWritesTheFileAtomicallyThenServesIt(t *testing.T) {
	f := newFixture(t, Files, map[string]string{"a.yml": jobText("a", "echo a")})
	text := "# as written\n" + jobText("a", "echo b")
	_, err := f.jobs.Replace(ctx, "a", []byte(text), f.etag("a"), byAPI)
	must(t, err)
	f.serves("a", "echo b", FromFile)
	got, err := os.ReadFile(filepath.Join(f.dir, "a.yml"))
	must(t, err)
	if string(got) != text {
		t.Fatalf("the file holds %q", got)
	}
	_, err = f.jobs.Create(ctx, "n", []byte(jobText("n", "echo n")), byAPI)
	must(t, err)
	entries, err := os.ReadDir(f.dir)
	must(t, err)
	if len(entries) != 2 {
		t.Fatalf("the directory holds %v", entries)
	}
	if len(f.edits()) != 0 {
		t.Fatalf("files mode kept edits: %v", f.edits())
	}
	_, err = f.jobs.Delete(ctx, "n", f.etag("n"), byAPI)
	must(t, err)
	if _, err := os.Stat(filepath.Join(f.dir, "n.yml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the deleted job's file: %v", err)
	}
	// A load finds the files as kipp wrote them: no version of its own.
	f.load()
	if v := f.versions("a"); len(v) != 2 || v[0].How != store.HowAPI {
		t.Fatalf("versions: %+v", v)
	}
}

func TestFilesModeRefusesAChangeOnceTheFileChangedOnDisk(t *testing.T) {
	f := newFixture(t, Files, map[string]string{"a.yml": jobText("a", "echo a")})
	etag := f.etag("a")
	f.write("a.yml", jobText("a", "echo behind kipp's back"))
	_, err := f.jobs.Replace(ctx, "a", []byte(jobText("a", "echo b")), etag, byAPI)
	isErr(t, "replace", err, ErrFilesChanged)
	f.write("x.yml", jobText("x", "echo x"))
	_, err = f.jobs.Create(ctx, "x", []byte(jobText("x", "echo y")), byAPI)
	isErr(t, "create over a file not read", err, ErrFilesChanged)
}

func TestAWriteThatFailsInFilesModeLeavesMemoryUnchanged(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes into a directory without write permission")
	}
	f := newFixture(t, Files, map[string]string{"a.yml": jobText("a", "echo a")})
	must(t, os.Chmod(f.dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(f.dir, 0o700) })
	before := f.published
	_, err := f.jobs.Replace(ctx, "a", []byte(jobText("a", "echo b")), f.etag("a"), byAPI)
	if we, ok := errors.AsType[*WriteError](err); !ok || !strings.Contains(we.Error(), "a.yml") || we.Unwrap() == nil {
		t.Fatalf("replace: %v", err)
	}
	f.serves("a", "echo a", FromFile)
	if f.published != before || len(f.versions("a")) != 1 {
		t.Fatalf("published %d sets, versions %+v", f.published-before, f.versions("a"))
	}
}

func TestFilesModeRefusesToStartOnADirectoryItCannotWriteAndSaysTheWaysOut(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes into a directory without write permission")
	}
	dir := t.TempDir()
	must(t, CheckWritable(dir))
	must(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	err := CheckWritable(dir)
	if err == nil {
		t.Fatal("a read-only directory passed")
	}
	for _, want := range []string{dir, "uid", ":ro", "--job-edits store", "writable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q is not in %q", want, err)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("the check left %v", entries)
	}
}

func TestFilesModeWritesTheEditsAStoreServerLeftAndKeepsTheOnesInConflict(t *testing.T) {
	f := newFixture(t, Store, map[string]string{"a.yml": jobText("a", "echo a"), "b.yml": jobText("b", "echo b"), "gone.yml": jobText("gone", "echo gone")})
	_, err := f.jobs.Replace(ctx, "a", []byte(jobText("a", "echo edited")), f.etag("a"), byAPI)
	must(t, err)
	_, err = f.jobs.Create(ctx, "new", []byte(jobText("new", "echo new")), byAPI)
	must(t, err)
	_, err = f.jobs.Delete(ctx, "gone", f.etag("gone"), byAPI)
	must(t, err)
	_, err = f.jobs.Replace(ctx, "b", []byte(jobText("b", "echo edited")), f.etag("b"), byAPI)
	must(t, err)
	f.write("b.yml", jobText("b", "echo changed"))

	f.open(Files)
	f.serves("a", "echo edited", FromFile)
	f.serves("new", "echo new", FromFile)
	f.absent("gone")
	f.serves("b", "echo changed", FromFile)
	for name, want := range map[string]string{"a.yml": "echo edited", "new.yml": "echo new"} {
		got, err := os.ReadFile(filepath.Join(f.dir, name))
		must(t, err)
		if !strings.Contains(string(got), want) {
			t.Errorf("%s holds %q", name, got)
		}
	}
	if _, err := os.Stat(filepath.Join(f.dir, "gone.yml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("gone.yml: %v", err)
	}
	if e := f.edits(); len(e) != 1 || e["b"].Version == nil {
		t.Fatalf("edits left: %v", e)
	}
	f.waits("b")
}

func TestStoreModeNeedsNoJobsDirectory(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	j := New(Options{Mode: Store, Store: st})
	_, err = j.Load(ctx)
	must(t, err)
	_, err = j.Create(ctx, "solo", []byte(jobText("solo", "echo solo")), byAPI)
	must(t, err)
	j = New(Options{Mode: Store, Store: st})
	r, err := j.Load(ctx)
	must(t, err)
	if r.Jobs != 1 {
		t.Fatalf("report: %+v", r)
	}
}

func TestMatchesReadsAnIfMatchHeader(t *testing.T) {
	for header, want := range map[string]bool{`"abc"`: true, `W/"abc"`: true, `"x", "abc"`: true, `*`: true, `"abd"`: false, ``: false} {
		if got := Matches(header, "abc"); got != want {
			t.Errorf("%q: got %v", header, got)
		}
	}
}

func TestParseModeTakesTheThreeModesAlone(t *testing.T) {
	for _, s := range []string{"off", "store", "files"} {
		if m, err := ParseMode(s); err != nil || string(m) != s {
			t.Errorf("%s: %v, %v", s, m, err)
		}
	}
	if _, err := ParseMode("git"); err == nil {
		t.Fatal("git was taken")
	}
}

func TestPrecheckRefusesABadFileBeforeAnythingIsOpened(t *testing.T) {
	dir := t.TempDir()
	must(t, Precheck(""))
	must(t, os.WriteFile(filepath.Join(dir, "a.yml"), []byte(jobText("a", "echo a")), 0o600))
	must(t, Precheck(dir))
	must(t, os.WriteFile(filepath.Join(dir, "b.yml"), []byte("name: c\nstages:\n  s: {run: 'true'}\n"), 0o600))
	if _, ok := errors.AsType[*config.Errors](Precheck(dir)); !ok {
		t.Fatal("a file holding another job's name passed")
	}
}

func TestWhereAndTheVersionsReadWhatServes(t *testing.T) {
	f := newFixture(t, Store, map[string]string{"a.yml": jobText("a", "echo a")})
	if f.jobs.Mode() != Store {
		t.Fatalf("mode %s", f.jobs.Mode())
	}
	v, err := f.jobs.Create(ctx, "b", []byte(jobText("b", "echo b")), byAPI)
	must(t, err)
	for name, want := range map[string]Origin{"a": FromFile, "b": FromStore, "c": ""} {
		if got, waits := f.jobs.Where(name); got != want || waits {
			t.Errorf("%s: %q, %v", name, got, waits)
		}
	}
	vs, err := f.jobs.Versions(ctx, "b", 0, 10)
	must(t, err)
	got, err := f.jobs.Version(ctx, "b", v.ID)
	must(t, err)
	if len(vs) != 1 || got.Text != jobText("b", "echo b") {
		t.Fatalf("versions %+v, version %+v", vs, got)
	}
}

func TestEditsTheSetRefusesAreSetAsideOneByOne(t *testing.T) {
	f := newFixture(t, Store, map[string]string{"up.yml": jobText("up", "echo up")})
	for _, name := range []string{"down1", "down2"} {
		_, err := f.jobs.Create(ctx, name, []byte("name: "+name+"\ntriggers: {after: [up]}\nstages:\n  s: {run: echo}\n"), byAPI)
		must(t, err)
	}
	f.remove("up.yml")
	if r := f.load(); r.Conflicts != 2 || r.Jobs != 0 {
		t.Fatalf("report %+v", r)
	}
	f.waits("down1")
	f.waits("down2")
}

func TestDeletedListsTheJobsWhoseNewestVersionIsADeletionUntilOneIsMadeAgain(t *testing.T) {
	f := newFixture(t, Store, map[string]string{"kept.yml": jobText("kept", "true")})
	first, err := f.jobs.Create(ctx, "b", []byte(jobText("b", "echo b")), byAPI)
	must(t, err)
	_, err = f.jobs.Create(ctx, "a", []byte(jobText("a", "echo a")), byAPI)
	must(t, err)
	_, err = f.jobs.Delete(ctx, "b", f.etag("b"), byAPI)
	must(t, err)
	_, err = f.jobs.Delete(ctx, "a", f.etag("a"), byAPI)
	must(t, err)
	gone, err := f.jobs.Deleted(ctx)
	must(t, err)
	if len(gone) != 2 || gone[0].Job != "a" || gone[1].Job != "b" || !gone[0].Deleted {
		t.Fatalf("deleted %+v", gone)
	}
	_, err = f.jobs.Restore(ctx, "b", first.ID, "", "admin")
	must(t, err)
	gone, err = f.jobs.Deleted(ctx)
	must(t, err)
	if len(gone) != 1 || gone[0].Job != "a" {
		t.Fatalf("after a restore %+v", gone)
	}
}
