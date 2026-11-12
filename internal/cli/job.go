package cli

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/config"
)

const jobUsage = `job get <name>
       kipp job put <name> [-f <file>|-] [--if-match <etag>]
       kipp job edit <name>
       kipp job rm <name> [--if-match <etag>]
       kipp job history <name>
       kipp job restore <name> <version> [--if-match <etag>]
       kipp job resolve <name> file|edit
       kipp job export <dir> [--force]
       kipp job import <dir> --data <dir>`

// jobArgs is how many arguments each subcommand that talks to a server
// takes after its name.
var jobArgs = map[string]int{"get": 1, "put": 1, "edit": 1, "rm": 1, "history": 1, "restore": 2, "resolve": 2, "export": 1}

// runEditor opens path in editor, a command line as $VISUAL holds it, on
// the terminal; a test stands in for it.
var runEditor = func(editor, path string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.Command("/bin/sh", "-c", editor+` "$1"`, "sh", path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return cmd.Run()
}

type jobCmd struct {
	c              *client
	stdin          io.Reader
	stdout, stderr io.Writer
	file, ifMatch  string
	force          bool
}

func cmdJob(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlags(jobUsage, stderr)
	connect := clientFlags(fs)
	j := &jobCmd{stdin: stdin, stdout: stdout, stderr: stderr}
	fs.StringVar(&j.file, "f", "-", "put: the job `file` to send; - is stdin")
	fs.StringVar(&j.ifMatch, "if-match", "", "put, rm, restore: the `etag` the change is made over, instead of a fresh read's")
	fs.BoolVar(&j.force, "force", false, "export: write into a directory that is not empty, replacing its job files")
	data := fs.String("data", "", "import: the server's data `directory`")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return exitFor(err)
	}
	if len(positional) == 2 && positional[0] == "import" {
		return importJobs(positional[1], *data, fs.Usage, stdout, stderr)
	}
	if n, known := jobArgs[firstOf(positional)]; !known || n != len(positional)-1 {
		fs.Usage()
		return 2
	}
	sub, rest := positional[0], positional[1:]
	if j.c, err = connect(); err == nil {
		err = j.run(sub, rest)
	}
	if err != nil {
		return fail(stderr, "job "+sub, err)
	}
	return 0
}

func (j *jobCmd) run(sub string, args []string) error {
	switch sub {
	case "get":
		return j.get(args[0])
	case "put":
		return j.put(args[0])
	case "edit":
		return j.edit(args[0])
	case "rm":
		return j.rm(args[0])
	case "history":
		return j.history(args[0])
	case "restore":
		return j.restore(args[0], args[1])
	case "resolve":
		return j.resolve(args[0], args[1])
	}
	return j.export(args[0])
}

// definition is a job as the server serves it.
type definition struct {
	Name   string `json:"name"`
	YAML   string `json:"yaml"`
	ETag   string `json:"etag"`
	Source string `json:"source"`
}

func firstOf(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func jobPath(name string) string { return "/jobs/" + url.PathEscape(name) }

// definition reads the job name; a job the server does not have is an
// *answered with 404.
func (j *jobCmd) definition(name string) (*definition, error) {
	var d definition
	if err := j.c.call(http.MethodGet, jobPath(name)+"/definition", nil, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// etag is --if-match, else the ETag of a fresh read; "" for a job the
// server does not have, when absentOK.
func (j *jobCmd) etag(name string, absentOK bool) (string, error) {
	if j.ifMatch != "" {
		return j.ifMatch, nil
	}
	d, err := j.definition(name)
	if absentOK && answeredWith(err, http.StatusNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return d.ETag, nil
}

// send makes a change and reads the job as it then serves; with an etag,
// as If-Match.
func (j *jobCmd) send(method, path string, body any, etag string) (*definition, error) {
	var sent []byte
	if body != nil {
		var err error
		if sent, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	hdr := http.Header{}
	if etag != "" {
		hdr.Set("If-Match", etag)
	}
	_, data, err := j.c.doWith(method, path, sent, hdr)
	if err != nil {
		return nil, err
	}
	var d definition
	if len(data) > 0 {
		if err := json.Unmarshal(data, &d); err != nil {
			return nil, err
		}
	}
	return &d, nil
}

func (j *jobCmd) get(name string) error {
	d, err := j.definition(name)
	if err != nil {
		return err
	}
	_, err = io.WriteString(j.stdout, d.YAML)
	return err
}

// put creates the job, or replaces it over the ETag it reads first.
func (j *jobCmd) put(name string) error {
	text, err := j.readJob()
	if err != nil {
		return err
	}
	etag, err := j.etag(name, true)
	if err != nil {
		return err
	}
	if etag == "" {
		d, err := j.send(http.MethodPost, "/jobs", map[string]string{"name": name, "yaml": string(text)}, "")
		if err == nil {
			say(j.stdout, "job %s created (%s)", name, d.Source)
		}
		return err
	}
	return j.replace(name, text, etag)
}

func (j *jobCmd) replace(name string, text []byte, etag string) error {
	d, err := j.send(http.MethodPut, jobPath(name)+"/definition", map[string]string{"yaml": string(text)}, etag)
	if err == nil {
		say(j.stdout, "job %s changed (%s)", name, d.Source)
	}
	return err
}

// readJob is the text -f names, stdin for -, no longer than a request may
// carry and one byte, which the server then refuses for its size.
func (j *jobCmd) readJob() ([]byte, error) {
	r := j.stdin
	if j.file != "-" {
		f, err := os.Open(j.file)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	return io.ReadAll(io.LimitReader(r, api.MaxBody+1))
}

// edit opens the job in the editor and sends what comes back over the
// ETag it read: a change made meanwhile is refused, and the edit is then
// kept in its file, whose path is said.
func (j *jobCmd) edit(name string) error {
	d, err := j.definition(name)
	if err != nil {
		return err
	}
	// The buffer is <name>.yml, so that an editor knows it for YAML, in a
	// directory of its own for its owner alone.
	dir, err := os.MkdirTemp("", "kipp-edit-")
	if err != nil {
		return err
	}
	file := config.FileName(name)
	path := filepath.Join(dir, file)
	text, err := j.editIn(dir, file, d.YAML)
	if err != nil {
		return errors.Join(err, os.RemoveAll(dir))
	}
	if string(text) == d.YAML {
		say(j.stdout, "job %s: no change, nothing sent", name)
		return os.RemoveAll(dir)
	}
	if err := j.replace(name, text, d.ETag); err != nil {
		if answeredWith(err, http.StatusPreconditionFailed) {
			err = fmt.Errorf("%w; run kipp job edit %s again to start from what serves now", err, name)
		}
		return fmt.Errorf("%w\nyour edit is kept in %s", err, path)
	}
	return os.RemoveAll(dir)
}

// editIn writes text to file in dir, opens it in the editor and is what
// the editor left there.
func (j *jobCmd) editIn(dir, file, text string) ([]byte, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	if err := root.WriteFile(file, []byte(text), 0o600); err != nil {
		return nil, err
	}
	editor := cmp.Or(os.Getenv("VISUAL"), os.Getenv("EDITOR"), "vi")
	if err := runEditor(editor, filepath.Join(dir, file), j.stdin, j.stdout, j.stderr); err != nil {
		return nil, fmt.Errorf("the editor %q failed: %w", editor, err)
	}
	return root.ReadFile(file)
}

func (j *jobCmd) rm(name string) error {
	etag, err := j.etag(name, false)
	if err == nil {
		_, err = j.send(http.MethodDelete, jobPath(name), nil, etag)
	}
	if err == nil {
		say(j.stdout, "job %s deleted", name)
	}
	return err
}

// jobVersion is one line of a job's history.
type jobVersion struct {
	ID      int64  `json:"id"`
	Deleted bool   `json:"deleted"`
	Author  string `json:"author"`
	How     string `json:"how"`
	At      string `json:"at"`
}

func (j *jobCmd) history(name string) error {
	var all []jobVersion
	for cursor := ""; ; {
		var page struct {
			Items []jobVersion `json:"items"`
			Next  *string      `json:"next_cursor"`
		}
		path := jobPath(name) + "/versions"
		if cursor != "" {
			path += "?cursor=" + url.QueryEscape(cursor)
		}
		if err := j.c.call(http.MethodGet, path, nil, &page); err != nil {
			return err
		}
		all = append(all, page.Items...)
		if page.Next == nil {
			break
		}
		cursor = *page.Next
	}
	if len(all) == 0 {
		return fmt.Errorf("no version of a job called %s is recorded", name)
	}
	for _, v := range all {
		line := fmt.Sprintf("%d  %s  %s  %s", v.ID, v.At, v.Author, v.How)
		if v.Deleted {
			line += "  deleted"
		}
		say(j.stdout, "%s", line)
	}
	return nil
}

func (j *jobCmd) restore(name, ver string) error {
	id, err := strconv.ParseInt(ver, 10, 64)
	if err != nil || id < 1 {
		return refused(fmt.Sprintf("a version is a number, 1 or more, not %q", ver))
	}
	etag, err := j.etag(name, true)
	if err != nil {
		return err
	}
	d, err := j.send(http.MethodPost, fmt.Sprintf("%s/versions/%d/restore", jobPath(name), id), nil, etag)
	if err == nil {
		say(j.stdout, "job %s restored to version %d (%s)", name, id, d.Source)
	}
	return err
}

func (j *jobCmd) resolve(name, keep string) error {
	if keep != "file" && keep != "edit" {
		return refused(fmt.Sprintf("resolve keeps the file or the edit, not %q", keep))
	}
	_, err := j.send(http.MethodPost, "/admin/job-conflicts/"+url.PathEscape(name)+"/resolve", map[string]string{"keep": keep}, "")
	if err == nil {
		say(j.stdout, "job %s: kept the %s", name, keep)
	}
	return err
}

// export writes every job of the server into dir as <name>.yml. It reads
// them all before it writes one, so that a refusal leaves dir as it was.
func (j *jobCmd) export(dir string) error {
	var list struct {
		Items []struct {
			Name    string `json:"name"`
			Removed bool   `json:"removed"`
		} `json:"items"`
	}
	if err := j.c.call(http.MethodGet, "/jobs", nil, &list); err != nil {
		return err
	}
	texts := map[string]string{}
	for _, item := range list.Items {
		if item.Removed {
			continue
		}
		d, err := j.definition(item.Name)
		if answeredWith(err, http.StatusNotFound) {
			continue // deleted since the list was read
		}
		if err != nil {
			return err
		}
		texts[item.Name] = d.YAML
	}
	root, err := exportDir(dir, j.force)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	for _, name := range slices.Sorted(maps.Keys(texts)) {
		// A job's env may hold what only an admin may read.
		if err := root.WriteFile(config.FileName(name), []byte(texts[name]), 0o600); err != nil {
			return err
		}
	}
	say(j.stdout, "exported %s to %s", jobCount(len(texts)), dir)
	return nil
}

// exportDir makes dir ready for an export and opens it: made, in a
// directory that exists, when absent; refused when it holds anything,
// unless --force, which removes its job files and leaves the rest
// (_vars.yml among it).
func exportDir(dir string, force bool) (*os.Root, error) {
	parent, err := os.OpenRoot(filepath.Dir(dir))
	if err != nil {
		return nil, err
	}
	defer func() { _ = parent.Close() }()
	base := filepath.Base(dir)
	if err := parent.Mkdir(base, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	root, err := parent.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	err = clearIn(root, dir, force)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	return root, nil
}

func clearIn(root *os.Root, dir string, force bool) error {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return err
	}
	if len(entries) > 0 && !force {
		return refused(dir + " is not empty; --force replaces the job files in it")
	}
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".yml") && e.Name() != config.VarsFile {
			if err := root.Remove(e.Name()); err != nil {
				return err
			}
		}
	}
	return nil
}
