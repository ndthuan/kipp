package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/store"
)

const siteJob = "name: site\nstages:\n  s: {run: 'echo site'}\n"

func TestJobPutCreatesThenReplacesAndGetPrintsTheYAML(t *testing.T) {
	s := start(t, map[string]string{"hello.yml": helloJob})
	code, out, errOut := kipp(siteJob, "job", "put", "site", "--url", s.url, "--token", s.token)
	eq(t, "create: "+errOut, code, 0)
	contains(t, out, "job site created (store)")

	changed := strings.Replace(siteJob, "echo site", "echo again", 1)
	file := filepath.Join(t.TempDir(), "site.yml")
	write(t, file, changed, 0o600)
	code, out, errOut = s.kipp("job", "put", "site", "-f", file)
	eq(t, "replace: "+errOut, code, 0)
	contains(t, out, "job site changed (store)")

	code, out, _ = s.kipp("job", "get", "site")
	eq(t, "get", code, 0)
	eq(t, "yaml", out, changed)

	code, _, errOut = s.kipp("job", "get", "nothing")
	eq(t, "get of no job", code, 1)
	contains(t, errOut, "kipp job get:")
}

func TestJobPutPrintsEveryErrorOfARefusal(t *testing.T) {
	s := start(t, map[string]string{"hello.yml": helloJob})
	bad := "name: site\nstages:\n  s: {run: 'true'}\nafter: [nowhere]\nconcurrency: sometimes\n"
	code, _, errOut := kipp(bad, "job", "put", "site", "--url", s.url, "--token", s.token)
	eq(t, "exit", code, 1)
	contains(t, errOut, "refused:\n  ")
	if strings.Count(errOut, "\n  ") < 2 {
		t.Fatalf("not every error is printed: %q", errOut)
	}
}

func TestJobPutWithAStaleIfMatchIsRefused(t *testing.T) {
	s := start(t, map[string]string{"hello.yml": helloJob})
	code, _, errOut := kipp(helloJob, "job", "put", "hello", "--if-match", `"stale"`, "--url", s.url, "--token", s.token)
	eq(t, "exit", code, 1)
	contains(t, errOut, "changed since")
}

// editor stands runEditor in for the test.
func editor(t *testing.T, edit func(path string)) {
	t.Helper()
	was := runEditor
	runEditor = func(_, path string, _ io.Reader, _, _ io.Writer) error {
		edit(path)
		return nil
	}
	t.Cleanup(func() { runEditor = was })
}

func TestJobEditSendsWhatTheEditorLeftAndAnUnchangedBufferSendsNothing(t *testing.T) {
	s := start(t, map[string]string{"hello.yml": helloJob})
	script := filepath.Join(t.TempDir(), "fake-editor")
	write(t, script, "#!/bin/sh\nsed 's/echo hello/echo edited/' \"$1\" > \"$1.new\" && mv \"$1.new\" \"$1\"\n", 0o700)
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", script)
	code, out, errOut := s.kipp("job", "edit", "hello")
	eq(t, "edit: "+errOut, code, 0)
	contains(t, out, "job hello changed (store over file)")
	_, got, _ := s.kipp("job", "get", "hello")
	contains(t, got, "echo edited")

	t.Setenv("EDITOR", "true")
	code, out, _ = s.kipp("job", "edit", "hello")
	eq(t, "unchanged", code, 0)
	contains(t, out, "no change, nothing sent")
	_, hist, _ := s.kipp("job", "history", "hello")
	if n := strings.Count(hist, "\n"); n != 2 {
		t.Fatalf("an unchanged buffer made a version:\n%s", hist)
	}
}

func TestJobEditKeepsTheBufferWhenTheJobChangedMeanwhile(t *testing.T) {
	s := start(t, map[string]string{"hello.yml": helloJob})
	editor(t, func(path string) {
		root, err := os.OpenRoot(filepath.Dir(path))
		must(t, err)
		must(t, root.WriteFile(filepath.Base(path), []byte(strings.Replace(helloJob, "echo hello", "echo mine", 1)), 0o600))
		must(t, root.Close())
		// Somebody else changes the job while the editor is open.
		code, _, errOut := kipp(strings.Replace(helloJob, "echo hello", "echo theirs", 1), "job", "put", "hello", "--url", s.url, "--token", s.token)
		eq(t, "the other change: "+errOut, code, 0)
	})
	code, _, errOut := s.kipp("job", "edit", "hello")
	eq(t, "exit", code, 1)
	contains(t, errOut, "changed since", "kipp job edit hello again", "your edit is kept in ")
	path := strings.TrimSpace(errOut[strings.LastIndex(errOut, "kept in ")+len("kept in "):])
	root, err := os.OpenRoot(filepath.Dir(path))
	must(t, err)
	kept, err := root.ReadFile(filepath.Base(path))
	must(t, err)
	must(t, root.Close())
	contains(t, string(kept), "echo mine")
	must(t, os.RemoveAll(filepath.Dir(path)))
	_, got, _ := s.kipp("job", "get", "hello")
	contains(t, got, "echo theirs")
}

func TestJobRmHistoryRestoreAndResolve(t *testing.T) {
	s := start(t, map[string]string{"hello.yml": helloJob})
	ok(t, siteJob, "job", "put", "site", "--url", s.url, "--token", s.token)
	code, out, errOut := s.kipp("job", "rm", "site")
	eq(t, "rm: "+errOut, code, 0)
	contains(t, out, "job site deleted")

	_, hist, _ := s.kipp("job", "history", "site")
	lines := strings.Split(strings.TrimSpace(hist), "\n")
	eq(t, "history lines", len(lines), 2)
	contains(t, lines[0], "admin  api  deleted")
	created := strings.Fields(lines[1])[0]

	code, out, errOut = s.kipp("job", "restore", "site", created)
	eq(t, "restore: "+errOut, code, 0)
	contains(t, out, "job site restored to version "+created+" (store)")

	code, _, errOut = s.kipp("job", "restore", "site", "first")
	eq(t, "a version that is no number", code, 2)
	contains(t, errOut, "a version is a number")
	code, _, _ = s.kipp("job", "history", "nothing")
	eq(t, "history of no job", code, 1)

	// An edit over a file that then changes waits; resolve keeps the file.
	ok(t, strings.Replace(helloJob, "echo hello", "echo stored", 1), "job", "put", "hello", "--url", s.url, "--token", s.token)
	write(t, filepath.Join(s.jobs, "hello.yml"), strings.Replace(helloJob, "echo hello", "echo by hand", 1), 0o600)
	code, _, errOut = s.kipp("reload")
	eq(t, "reload: "+errOut, code, 0)
	code, _, errOut = s.kipp("job", "resolve", "hello", "neither")
	eq(t, "keep neither", code, 2)
	contains(t, errOut, "the file or the edit")
	code, out, errOut = s.kipp("job", "resolve", "hello", "file")
	eq(t, "resolve: "+errOut, code, 0)
	contains(t, out, "job hello: kept the file")
	_, got, _ := s.kipp("job", "get", "hello")
	contains(t, got, "echo by hand")
}

func TestJobExportWritesEveryJobAndRefusesADirectoryInUse(t *testing.T) {
	s := start(t, map[string]string{"hello.yml": helloJob})
	ok(t, siteJob, "job", "put", "site", "--url", s.url, "--token", s.token)
	dir := filepath.Join(t.TempDir(), "out")
	code, out, errOut := s.kipp("job", "export", dir)
	eq(t, "export: "+errOut, code, 0)
	contains(t, out, "exported 2 jobs to "+dir)
	out2, err := os.OpenRoot(dir)
	must(t, err)
	got, err := out2.ReadFile("site.yml")
	must(t, err)
	must(t, out2.Close())
	eq(t, "site.yml", string(got), siteJob)

	code, _, errOut = s.kipp("job", "export", dir)
	eq(t, "into a directory in use", code, 2)
	contains(t, errOut, "--force")

	write(t, filepath.Join(dir, "old.yml"), "name: old\n", 0o600)
	write(t, filepath.Join(dir, "_vars.yml"), "global: {A: b}\n", 0o600)
	write(t, filepath.Join(dir, "notes.txt"), "mine\n", 0o600)
	code, _, errOut = s.kipp("job", "export", dir, "--force")
	eq(t, "--force: "+errOut, code, 0)
	for _, name := range []string{"_vars.yml", "notes.txt", "hello.yml", "site.yml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "old.yml")); !os.IsNotExist(err) {
		t.Fatalf("--force left a job file the server does not have: %v", err)
	}
}

func TestJobExportIsAnAdminsAlone(t *testing.T) {
	s := start(t, map[string]string{"hello.yml": helloJob})
	ok(t, "a long password nobody guesses\n", "user", "add", "viewer", "--password-stdin", "--data", s.data)
	ok(t, "", "user", "grant", "viewer", "hello", "view", "--data", s.data)
	out := ok(t, "", "token", "create", "viewer", "--label", "test", "--data", s.data)
	token := strings.TrimSpace(out[strings.LastIndex(out, "kipp_"):])
	code, _, _ := kipp("", "job", "export", t.TempDir(), "--url", s.url, "--token", token)
	eq(t, "a viewer's export", code, 3)
}

func TestJobRefusesAnUnknownSubcommandOrTheWrongArguments(t *testing.T) {
	for _, args := range [][]string{{"job"}, {"job", "fly", "hello"}, {"job", "get"}, {"job", "restore", "hello"}, {"job", "import"}} {
		code, _, errOut := kipp("", args...)
		eq(t, strings.Join(args, " "), code, 2)
		contains(t, errOut, "usage: kipp job get <name>")
	}
}

func TestJobImportMakesEveryJobFileAJobOfTheStoreAllOrNothing(t *testing.T) {
	data := newData(t)
	jobs := t.TempDir()
	write(t, filepath.Join(jobs, "site.yml"), siteJob, 0o600)
	write(t, filepath.Join(jobs, "docs.yml"), "name: docs\nstages:\n  s: {run: 'echo docs'}\n", 0o600)
	write(t, filepath.Join(jobs, "_vars.yml"), "global: {A: b}\n", 0o600)
	out := ok(t, "", "job", "import", jobs, "--data", data)
	contains(t, out, "imported 2 jobs into the store: docs, site", "next reload or restart", "_vars.yml is not a job")

	st, err := openStore(data, false)
	must(t, err)
	edits, err := st.JobEdits(context.Background())
	must(t, err)
	v := edits["site"].Version
	if len(edits) != 2 || edits["site"].Base != "" || v.How != store.HowImport || v.Author != "cli" || v.Text != siteJob || v.Hash == "" {
		t.Fatalf("edits: %+v, site: %+v", edits, v)
	}
	must(t, st.Close())

	// A name the store holds refuses the whole import.
	other := t.TempDir()
	write(t, filepath.Join(other, "site.yml"), siteJob, 0o600)
	write(t, filepath.Join(other, "new.yml"), "name: new\nstages:\n  s: {run: 'true'}\n", 0o600)
	code, _, errOut := kipp("", "job", "import", other, "--data", data)
	eq(t, "a held name", code, 1)
	contains(t, errOut, "the store already holds site; nothing imported")

	// So does a directory with an error in it.
	write(t, filepath.Join(other, "site.yml"), "name: site\nstages: {}\n", 0o600)
	code, _, errOut = kipp("", "job", "import", other, "--data", data)
	eq(t, "a bad directory", code, 1)
	contains(t, errOut, "has errors; nothing imported")

	st, err = openStore(data, false)
	must(t, err)
	defer func() { _ = st.Close() }()
	edits, err = st.JobEdits(context.Background())
	must(t, err)
	if _, ok := edits["new"]; ok || len(edits) != 2 {
		t.Fatalf("a refused import wrote: %v", edits)
	}

	code, _, errOut = kipp("", "job", "import", t.TempDir(), "--data", data)
	eq(t, "an empty directory", code, 1)
	contains(t, errOut, "holds no job file")
	code, _, errOut = kipp("", "job", "import", jobs)
	eq(t, "no --data", code, 2)
	contains(t, errOut, "--data is required")
}

func TestImportedJobsServeAfterARestartWithoutAJobsDirectory(t *testing.T) {
	s := start(t, map[string]string{"hello.yml": helloJob})
	eq(t, "exit", s.stop(), 0)
	ok(t, "", "job", "import", s.jobs, "--data", s.data)
	again := &served{t: t, stderr: &syncBuffer{}, data: s.data, token: s.token}
	again.serve()
	code, body, _ := again.call("GET", "/api/v1/jobs/hello/definition", "")
	eq(t, "definition: "+body, code, 200)
	contains(t, body, `"source":"store"`)
}

func TestANewServerWithNoJobsDirectoryStartsWithTheTwoExampleJobs(t *testing.T) {
	data, _, token := dataAndJobs(t, nil)
	s := &served{t: t, stderr: &syncBuffer{}, data: data, token: token}
	s.serve()
	contains(t, s.stderr.String(), `msg="example jobs created; change or delete them as you like" jobs=hello,hello-schedule`)
	code, out, errOut := s.kipp("job", "get", "hello-schedule")
	eq(t, "get: "+errOut, code, 0)
	contains(t, out, "cron:")
}
