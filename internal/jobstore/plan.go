package jobstore

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

type plan struct {
	files     config.Files
	snap      *config.Snapshot
	origins   map[string]Origin
	conflicts []Conflict
	converged []string
	written   []string
	applied   map[string]store.JobEdit
}

const (
	reasonChanged  = "the job file changed since the edit was made over it"
	reasonRemoved  = "the job file the edit was made over was removed"
	reasonSameName = "the job is kept in the store and a job file of the same name exists"
	reasonOff      = "job edits are off on this server; the job files serve"
)

type outcome int

const (
	applies outcome = iota
	converges
	waits
)

func classify(file *diskJob, e store.JobEdit) (outcome, string) {
	v := e.Version
	switch {
	case e.Base == "" && file == nil:
		if v.Deleted {
			return converges, ""
		}
		return applies, ""
	case e.Base == "":
		return waits, reasonSameName
	case file != nil && file.hash == e.Base:
		return applies, ""
	case v.Deleted && file == nil, !v.Deleted && file != nil && file.hash == v.Hash:
		return converges, ""
	case file == nil:
		return waits, reasonRemoved
	}
	return waits, reasonChanged
}

func (j *Jobs) plan(disk map[string]diskJob, vars []byte, edits map[string]store.JobEdit) (plan, error) {
	p := plan{origins: map[string]Origin{}, applied: map[string]store.JobEdit{}}
	files := config.Files{}
	if vars != nil {
		files[config.VarsFile] = vars
	}
	for name, d := range disk {
		files[config.FileName(name)] = d.text
		p.origins[name] = FromFile
	}
	for _, name := range slices.Sorted(maps.Keys(edits)) {
		e := edits[name]
		file := lookup(disk, name)
		if j.o.Mode == Off {
			p.conflicts = append(p.conflicts, conflictOf(name, reasonOff, file, e))
			continue
		}
		switch out, why := classify(file, e); out {
		case applies:
			p.applied[name] = e
			apply(files, p.origins, name, e, file != nil)
		case converges:
			p.converged = append(p.converged, name)
		case waits:
			p.conflicts = append(p.conflicts, conflictOf(name, why, file, e))
		}
	}
	snap, err := config.Check(files)
	if err != nil {
		if files, snap, err = p.setAside(files, disk); err != nil {
			return plan{}, err
		}
	}
	p.files, p.snap = files, snap
	slices.SortFunc(p.conflicts, func(a, b Conflict) int { return strings.Compare(a.Job, b.Job) })
	return p, nil
}

func lookup(disk map[string]diskJob, name string) *diskJob {
	if d, ok := disk[name]; ok {
		return &d
	}
	return nil
}

func conflictOf(name, why string, file *diskJob, e store.JobEdit) Conflict {
	c := Conflict{Job: name, Reason: why, Edit: e.Version, Base: e.Base}
	if file != nil {
		c.File, c.FileHash = file.text, file.hash
	}
	return c
}

func apply(files config.Files, origins map[string]Origin, name string, e store.JobEdit, overFile bool) {
	if e.Version.Deleted {
		delete(files, config.FileName(name))
		delete(origins, name)
		return
	}
	files[config.FileName(name)] = []byte(e.Version.Text)
	origins[name] = FromStore
	if overFile {
		origins[name] = StoreOverFile
	}
}

func revert(files config.Files, origins map[string]Origin, disk map[string]diskJob, name string) {
	if d, ok := disk[name]; ok {
		files[config.FileName(name)] = d.text
		origins[name] = FromFile
		return
	}
	delete(files, config.FileName(name))
	delete(origins, name)
}

func (p *plan) setAside(files config.Files, disk map[string]diskJob) (config.Files, *config.Snapshot, error) {
	_, err := config.Check(files)
	for progress := true; progress && err != nil; {
		progress = false
		for _, name := range slices.Sorted(maps.Keys(p.applied)) {
			try, tryOrigins := maps.Clone(files), maps.Clone(p.origins)
			revert(try, tryOrigins, disk, name)
			snap, tryErr := config.Check(try)
			if tryErr != nil && errorCount(tryErr) >= errorCount(err) {
				continue
			}
			p.wait(name, disk, gone(err, tryErr))
			files, p.origins = try, tryOrigins
			if tryErr == nil {
				return files, snap, nil
			}
			err, progress = tryErr, true
		}
	}
	if !errors.As(err, new(*config.Errors)) {
		return nil, nil, err
	}
	for _, name := range slices.Sorted(maps.Keys(p.applied)) {
		revert(files, p.origins, disk, name)
		p.wait(name, disk, err.Error())
	}
	snap, err := config.Check(files)
	return files, snap, err
}

func (p *plan) wait(name string, disk map[string]diskJob, why string) {
	p.conflicts = append(p.conflicts, conflictOf(name, "the job set refuses the edit now: "+why, lookup(disk, name), p.applied[name]))
	delete(p.applied, name)
}

func errorCount(err error) int {
	if errs, ok := errors.AsType[*config.Errors](err); ok {
		return len(errs.Items)
	}
	return 1
}

func gone(before, after error) string {
	var still []string
	if after != nil {
		still = strings.Split(after.Error(), "\n")
	}
	var out []string
	for line := range strings.SplitSeq(before.Error(), "\n") {
		if !slices.Contains(still, line) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

func (j *Jobs) record(ctx context.Context, p plan, disk map[string]diskJob) error {
	seen, err := j.o.Store.JobFiles(ctx)
	if err != nil {
		return err
	}
	last, err := j.o.Store.LastJobVersions(ctx)
	if err != nil {
		return err
	}
	c := store.JobChanges{Retire: append(slices.Clone(p.converged), p.written...), Files: map[string]string{}}
	now := j.o.Now()
	names := slices.Collect(maps.Keys(disk))
	for name := range seen {
		if _, ok := disk[name]; !ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		d, onDisk := disk[name]
		if d.hash == seen[name] {
			continue
		}
		c.Files[name] = d.hash
		prev := last[name]
		switch _, serves := p.snap.Texts[name]; {
		case onDisk && p.origins[name] == FromFile && (prev == nil || prev.Deleted || prev.Hash != d.hash):
			c.Versions = append(c.Versions, &store.JobVersion{Job: name, Text: string(d.text), Hash: d.hash, Author: store.AuthorKipp, How: store.HowFiles, At: now})
		case !onDisk && !serves && prev != nil && !prev.Deleted:
			c.Versions = append(c.Versions, &store.JobVersion{Job: name, Deleted: true, Author: store.AuthorKipp, How: store.HowFiles, At: now})
		}
	}
	if len(c.Versions) == 0 && len(c.Retire) == 0 && len(c.Files) == 0 {
		return nil
	}
	return j.o.Store.ApplyJobChanges(ctx, c)
}
