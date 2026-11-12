package jobstore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

// The choices of a conflict: the file serves and the edit is retired, or
// the edit is made over the file as it is now and serves.
const (
	KeepFile = "file"
	KeepEdit = "edit"
)

// Who made a change and through what: How is one of store.HowAPI,
// store.HowWeb and store.HowMCP.
type Who struct {
	Author, How string
}

// Create adds the job name with text. A name the set has is ErrExists; a
// child's is the caller's to refuse, as Jobs does not know the children.
func (j *Jobs) Create(ctx context.Context, name string, text []byte, who Who) (*store.JobVersion, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.editable(name); err != nil {
		return nil, err
	}
	if _, ok := j.snap.Texts[name]; ok {
		return nil, ErrExists
	}
	if err := j.diskUnchanged(name); err != nil {
		return nil, err
	}
	return j.commit(ctx, name, text, who, "created")
}

// Replace changes the job name to text, if ifMatch names what serves.
func (j *Jobs) Replace(ctx context.Context, name string, text []byte, ifMatch string, who Who) (*store.JobVersion, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.changeable(name, ifMatch); err != nil {
		return nil, err
	}
	return j.commit(ctx, name, text, who, "changed")
}

// Delete deletes the job name, if ifMatch names what serves.
func (j *Jobs) Delete(ctx context.Context, name, ifMatch string, who Who) (*store.JobVersion, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.changeable(name, ifMatch); err != nil {
		return nil, err
	}
	return j.commit(ctx, name, nil, who, "deleted")
}

// Restore makes the text of the job's version id its new version: a change
// of a job that serves, which needs ifMatch, or the job back once deleted.
func (j *Jobs) Restore(ctx context.Context, name string, id int64, ifMatch, author string) (*store.JobVersion, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.editable(name); err != nil {
		return nil, err
	}
	v, err := j.o.Store.JobVersion(ctx, name, id)
	if err != nil {
		return nil, err
	}
	if v.Deleted {
		return nil, ErrDeletion
	}
	if _, ok := j.snap.Texts[name]; ok {
		err = j.changeable(name, ifMatch)
	} else {
		err = j.diskUnchanged(name)
	}
	if err != nil {
		return nil, err
	}
	return j.commit(ctx, name, []byte(v.Text), Who{Author: author, How: store.HowRevert}, "restored")
}

// Resolve settles the edit of name that waits: KeepFile retires it,
// KeepEdit makes it over the file as it is now (in files mode, writes it).
func (j *Jobs) Resolve(ctx context.Context, name, keep, author string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.o.Mode == Off {
		return ErrEditsOff
	}
	c := j.conflictLocked(name)
	if c == nil {
		return ErrNoConflict
	}
	switch keep {
	case KeepEdit:
		if err := j.diskUnchanged(name); err != nil {
			return err
		}
		var text []byte
		if !c.Edit.Deleted {
			text = []byte(c.Edit.Text)
		}
		_, err := j.commit(ctx, name, text, Who{Author: author, How: store.HowResolve}, "resolved, the edit kept")
		return err
	case KeepFile:
		return j.retire(ctx, name, author)
	}
	return fmt.Errorf("keep is %s or %s, not %q", KeepFile, KeepEdit, keep)
}

// retire drops the edit of name, which waits while the file serves, and
// records what serves as a version when the last one says otherwise.
func (j *Jobs) retire(ctx context.Context, name, author string) error {
	c := store.JobChanges{Retire: []string{name}}
	last, err := j.o.Store.JobVersions(ctx, name, 0, 1)
	if err != nil {
		return err
	}
	var prev *store.JobVersion
	if len(last) > 0 {
		prev = last[0]
	}
	v := &store.JobVersion{Job: name, Author: author, How: store.HowResolve, At: j.o.Now()}
	if text, ok := j.snap.Texts[name]; ok {
		v.Text, v.Hash = string(text), j.snap.Hashes[name]
		if prev == nil || prev.Deleted || prev.Hash != v.Hash {
			c.Versions = append(c.Versions, v)
		}
	} else if prev != nil && !prev.Deleted {
		v.Deleted = true
		c.Versions = append(c.Versions, v)
	}
	if err := j.o.Store.ApplyJobChanges(ctx, c); err != nil {
		return err
	}
	j.conflicts = slices.DeleteFunc(slices.Clone(j.conflicts), func(c Conflict) bool { return c.Job == name })
	j.o.Log.Info("job edit retired; the file serves", "job", name, "how", store.HowResolve)
	return nil
}

// editable refuses every change in off mode, and a name no job may have.
func (j *Jobs) editable(name string) error {
	if j.o.Mode == Off {
		return ErrEditsOff
	}
	if !config.ValidName(name) {
		return &config.Errors{Items: []error{fmt.Errorf("name %q: lower-case letters, digits, dot, dash and underscore only", name)}}
	}
	return nil
}

// changeable is nil when the job name serves, ifMatch names it, and in
// files mode its file is as kipp read it.
func (j *Jobs) changeable(name, ifMatch string) error {
	if err := j.editable(name); err != nil {
		return err
	}
	hash, ok := j.snap.Hashes[name]
	switch {
	case !ok:
		return ErrNoJob
	case ifMatch == "":
		return ErrPreconditionRequired
	case !Matches(ifMatch, hash):
		return ErrStale
	}
	return j.diskUnchanged(name)
}

// diskUnchanged is files mode's check before a write: the job's file on
// disk still has the decoded hash kipp read, or is still absent.
func (j *Jobs) diskUnchanged(name string) error {
	if j.o.Mode != Files {
		return nil
	}
	want := lookup(j.disk, name)
	data, err := os.ReadFile(filepath.Join(j.o.Dir, config.FileName(name)))
	switch {
	case errors.Is(err, fs.ErrNotExist) && want == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist), err == nil && want == nil:
		return ErrFilesChanged
	case err != nil:
		return err
	}
	job, err := config.ParseJob(config.FileName(name), data)
	if err != nil || config.DecodedHash(job) != want.hash {
		return ErrFilesChanged
	}
	return nil
}

// commit checks the set with text as the job name (nil deletes it), writes
// the change to the store or to the file, serves it and publishes it.
func (j *Jobs) commit(ctx context.Context, name string, text []byte, who Who, what string) (*store.JobVersion, error) {
	fname := config.FileName(name)
	try := maps.Clone(j.served)
	if text == nil {
		delete(try, fname)
	} else {
		try[fname] = text
	}
	snap, err := config.Check(try)
	if err != nil {
		return nil, err
	}
	v := &store.JobVersion{Job: name, Text: string(text), Deleted: text == nil, Hash: snap.Hashes[name], Author: who.Author, How: who.How, At: j.o.Now()}
	c := store.JobChanges{Versions: []*store.JobVersion{v}}
	disk, origins := j.disk, maps.Clone(j.origins)
	file := lookup(j.disk, name)
	switch {
	case j.o.Mode == Files:
		if disk, err = writeDisk(j.o.Dir, j.disk, name, text, v.Hash); err != nil {
			return nil, err
		}
		c.Retire, c.Files = []string{name}, map[string]string{name: v.Hash}
		origins[name] = FromFile
	case text == nil && file == nil:
		c.Retire = []string{name}
	case file == nil:
		c.Edits = map[string]store.JobEdit{name: {Version: v}}
		origins[name] = FromStore
	default:
		c.Edits = map[string]store.JobEdit{name: {Version: v, Base: file.hash}}
		origins[name] = StoreOverFile
	}
	if text == nil {
		delete(origins, name)
	}
	err = j.o.Store.ApplyJobChanges(ctx, c)
	if err != nil && j.o.Mode != Files {
		return nil, err
	}
	// In files mode the file is written and is the job, recorded or not.
	j.disk, j.served, j.snap, j.origins = disk, try, snap, origins
	j.conflicts = slices.DeleteFunc(slices.Clone(j.conflicts), func(c Conflict) bool { return c.Job == name })
	j.o.Log.Info("job "+what+" through kipp", "job", name, "version", v.ID, "how", who.How, "by", who.Author)
	j.o.Publish(ctx, snap)
	if err != nil {
		return nil, fmt.Errorf("the job file was written and its version was not recorded: %w", err)
	}
	return v, nil
}

// writeDisk writes or removes the job file of name in dir, and is disk as
// it then is.
func writeDisk(dir string, disk map[string]diskJob, name string, text []byte, hash string) (map[string]diskJob, error) {
	fname := config.FileName(name)
	disk = maps.Clone(disk)
	if text == nil {
		if _, ok := disk[name]; ok {
			if err := removeFile(dir, fname); err != nil {
				return nil, err
			}
		}
		delete(disk, name)
		return disk, nil
	}
	if err := writeFile(dir, fname, text); err != nil {
		return nil, err
	}
	disk[name] = diskJob{text: text, hash: hash}
	return disk, nil
}

// writeApplied is files mode at a load: the edits a store server left that
// apply are written to their files and retired. It is disk as it then is.
func (j *Jobs) writeApplied(p *plan, disk map[string]diskJob) (map[string]diskJob, error) {
	for _, name := range slices.Sorted(maps.Keys(p.applied)) {
		v := p.applied[name].Version
		var text []byte
		if !v.Deleted {
			text = []byte(v.Text)
			p.origins[name] = FromFile
		}
		var err error
		if disk, err = writeDisk(j.o.Dir, disk, name, text, v.Hash); err != nil {
			return nil, err
		}
		p.written = append(p.written, name)
		j.o.Log.Info("job edit of the store written to its file", "job", name, "version", v.ID)
	}
	return disk, nil
}
