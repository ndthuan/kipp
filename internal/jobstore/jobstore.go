// Package jobstore is where the server's jobs come from: the job files of
// a directory, and the jobs created, changed and deleted through kipp,
// kept in the database with every version. It decides at each load which
// of the two a job is served from, writes a change through kipp to the
// store or to the file, and hands the set it serves to the rest of kipp.
package jobstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

// Mode is what a change through kipp does: nothing, a job of the store, or
// a write of the job file.
type Mode string

// The three modes of serve --job-edits.
const (
	Off   Mode = "off"
	Store Mode = "store"
	Files Mode = "files"
)

// ParseMode reads a --job-edits value.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(s); m {
	case Off, Store, Files:
		return m, nil
	}
	return "", fmt.Errorf("--job-edits is off, store or files, not %q", s)
}

// Origin is where a job served comes from.
type Origin string

// The three origins of a job of the server's own.
const (
	FromFile      Origin = "file"
	FromStore     Origin = "store"
	StoreOverFile Origin = "store over file"
)

// Errors a change through kipp is refused with; a refused set is a
// *config.Errors, and a file that could not be written a *WriteError.
var (
	ErrEditsOff             = errors.New("job edits are off on this server (serve --job-edits off)")
	ErrNoJob                = errors.New("no such job")
	ErrExists               = errors.New("a job of that name exists")
	ErrPreconditionRequired = errors.New("a change of a job needs If-Match with the job's ETag")
	ErrStale                = errors.New("the job changed since that ETag was read")
	ErrFilesChanged         = errors.New("the job file changed on disk since kipp read it; reload first")
	ErrNoConflict           = errors.New("no edit of that job waits in conflict")
	ErrDeletion             = errors.New("that version is a deletion; restore the version before it")
)

// WriteError is a job file that could not be written or removed; nothing
// changed in memory.
type WriteError struct {
	Path string
	Err  error
}

func (e *WriteError) Error() string { return fmt.Sprintf("writing %s: %v", e.Path, e.Err) }
func (e *WriteError) Unwrap() error { return e.Err }

// Conflict is an edit of the store that waits while the file serves.
type Conflict struct {
	Job    string
	Reason string
	// File is the text of the job file, nil when there is none; FileHash
	// its decoded hash.
	File     []byte
	FileHash string
	// Edit is the version of the edit that waits (a deletion has no
	// text), and Base the decoded hash of the file it was made over,
	// empty for none.
	Edit *store.JobVersion
	Base string
}

// Options are what Jobs is given.
type Options struct {
	Mode Mode
	// Dir is the jobs directory; "" is none, in store mode alone.
	Dir   string
	Store *store.Store
	Log   *slog.Logger
	// Publish is handed every set Jobs serves, under its lock, so that two
	// changes reach the rest of kipp in the order they were made.
	Publish func(ctx context.Context, snap *config.Snapshot)
	Now     func() time.Time
}

// Jobs is the server's job set and the changes made to it. Its methods are
// safe to call at once.
type Jobs struct {
	o Options

	mu sync.Mutex
	// disk is the job files as the last load read them, by job name, and
	// vars the VarsFile's text, nil for none.
	disk map[string]diskJob
	vars []byte
	// served are the texts the set was checked from, snap the set, and
	// origins where each job of it comes from.
	served    config.Files
	snap      *config.Snapshot
	origins   map[string]Origin
	conflicts []Conflict
}

type diskJob struct {
	text []byte
	hash string
}

// New is Jobs serving nothing until its first Load.
func New(o Options) *Jobs {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Publish == nil {
		o.Publish = func(context.Context, *config.Snapshot) {}
	}
	return &Jobs{o: o, disk: map[string]diskJob{}, served: config.Files{}, origins: map[string]Origin{},
		snap: &config.Snapshot{Jobs: map[string]*config.Job{}, Texts: map[string][]byte{}, Hashes: map[string]string{}}}
}

// Mode is the server's --job-edits.
func (j *Jobs) Mode() Mode { return j.o.Mode }

// Report is what a load did.
type Report struct {
	Jobs      int
	Conflicts int
}

// Load reads the job files and the store again, decides what serves, and
// publishes it. A bad file refuses the whole load (a *config.Errors) and
// changes nothing.
func (j *Jobs) Load(ctx context.Context) (Report, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	disk, vars, err := readDisk(j.o.Dir)
	if err != nil {
		return Report{}, err
	}
	edits, err := j.o.Store.JobEdits(ctx)
	if err != nil {
		return Report{}, err
	}
	p, err := j.plan(disk, vars, edits)
	if err != nil {
		return Report{}, err
	}
	if j.o.Mode == Files {
		if disk, err = j.writeApplied(&p, disk); err != nil {
			return Report{}, err
		}
	}
	if err := j.record(ctx, p, disk); err != nil {
		return Report{}, err
	}
	for _, name := range p.converged {
		j.o.Log.Info("job edit retired; the file now says the same", "job", name)
	}
	for _, c := range p.conflicts {
		j.o.Log.Warn("job edit waits in conflict; the file serves", "job", c.Job, "reason", c.Reason)
	}
	j.disk, j.vars, j.served, j.snap, j.origins, j.conflicts = disk, vars, p.files, p.snap, p.origins, p.conflicts
	j.o.Publish(ctx, p.snap)
	return Report{Jobs: len(p.snap.Names), Conflicts: len(p.conflicts)}, nil
}

// Precheck reads and parses the job files of dir, as a load does, so that a
// server refuses a bad file before it makes anything: a bad file is a
// *config.Errors.
func Precheck(dir string) error {
	_, _, err := readDisk(dir)
	return err
}

// readDisk reads and parses the job files of dir, none for "": a bad file
// is a *config.Errors. The set is not checked as a whole, as a job of the
// store may be part of it.
func readDisk(dir string) (map[string]diskJob, []byte, error) {
	disk := map[string]diskJob{}
	if dir == "" {
		return disk, nil, nil
	}
	files, err := config.ReadDir(dir)
	errs, refused := errors.AsType[*config.Errors](err)
	if err != nil && !refused {
		return nil, nil, err
	}
	if errs == nil {
		errs = &config.Errors{}
	}
	var vars []byte
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if name == config.VarsFile {
			vars = files[name]
			if _, err := config.ParseVars(vars); err != nil {
				errs.Items = append(errs.Items, fmt.Errorf("%s: %w", name, err))
			}
			continue
		}
		job, err := config.ParseJob(name, files[name])
		if err != nil {
			errs.Items = append(errs.Items, fmt.Errorf("%s: %w", name, err))
			continue
		}
		disk[job.Name] = diskJob{text: files[name], hash: config.DecodedHash(job)}
	}
	if len(errs.Items) > 0 {
		return nil, nil, errs
	}
	return disk, vars, nil
}

// Snapshot is the set served now.
func (j *Jobs) Snapshot() *config.Snapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.snap
}

// Definition is a job of the server's own as it serves: its text, its
// decoded hash (the ETag of a change), where it comes from, and whether
// an edit of it waits in conflict.
type Definition struct {
	Job      string
	Text     []byte
	Hash     string
	Origin   Origin
	Conflict bool
}

// Definition is the job name as it serves, or ErrNoJob.
func (j *Jobs) Definition(name string) (Definition, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	text, ok := j.snap.Texts[name]
	if !ok {
		return Definition{}, ErrNoJob
	}
	return Definition{Job: name, Text: text, Hash: j.snap.Hashes[name], Origin: j.origins[name], Conflict: j.conflictLocked(name) != nil}, nil
}

// Where is where the job name comes from, "" for none of the server's own
// (a child, or a job that is gone), and whether an edit of it waits.
func (j *Jobs) Where(name string) (Origin, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.origins[name], j.conflictLocked(name) != nil
}

// Conflicts are the edits that wait, by job name.
func (j *Jobs) Conflicts() []Conflict {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.conflicts)
}

func (j *Jobs) conflictLocked(name string) *Conflict {
	i := slices.IndexFunc(j.conflicts, func(c Conflict) bool { return c.Job == name })
	if i < 0 {
		return nil
	}
	return &j.conflicts[i]
}

// Versions are up to limit of the job's versions older than beforeID,
// newest first.
func (j *Jobs) Versions(ctx context.Context, name string, beforeID int64, limit int) ([]*store.JobVersion, error) {
	return j.o.Store.JobVersions(ctx, name, beforeID, limit)
}

// Version is one version of the job, or store.ErrNotFound.
func (j *Jobs) Version(ctx context.Context, name string, id int64) (*store.JobVersion, error) {
	return j.o.Store.JobVersion(ctx, name, id)
}

// Matches reports whether an If-Match header names hash: "*", or a list of
// entity tags, weak or strong, one of which is it.
func Matches(ifMatch, hash string) bool {
	for tag := range strings.SplitSeq(ifMatch, ",") {
		tag = strings.TrimPrefix(strings.TrimSpace(tag), "W/")
		if tag == "*" || strings.Trim(tag, `"`) == hash {
			return true
		}
	}
	return false
}
