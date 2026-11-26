package storage

import (
	"cmp"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

// Report is what the data directory holds and what to change.
type Report struct {
	At              time.Time
	Disk            Disk
	Parts           []Part
	Database        Database
	Jobs            []Job
	Recommendations []Recommendation
	Check           *Check
	Heal            *Heal
	Compact         CompactState
}

// Disk is the disk of the data directory.
type Disk struct {
	Known              bool
	Free, Total, Floor int64
}

// The kinds of a Part, in the order the report lists them.
const (
	PartDatabase        = "database"
	PartLogs            = "logs"
	PartArtifacts       = "artifacts"
	PartCache           = "cache"
	PartWorkspaces      = "workspaces"
	PartStageTemp       = "stage_temp"
	PartUpgradeBackups  = "upgrade_backups"
	PartSchemaCopies    = "schema_copies"
	PartUpgradeBinaries = "upgrade_binaries"
	PartSmallFiles      = "small_files"
	PartOther           = "other"
)

// Part is one part of the data directory: a kind kipp keeps, or one entry
// of the top level that is none of them.
type Part struct {
	Kind  string
	Name  string
	Bytes int64
	Made  bool
}

// Database is the database file and what is in it.
type Database struct {
	File, WAL            int64
	PageSize, Pages      int64
	FreeBytes, UsedBytes int64
	AutoVacuum           string
	Objects              []store.DBObject
	Exact                bool
	MeasuredAt           time.Time
	Measuring            bool
	MeasureError         string
}

// Why a job's file is gone.
const (
	GoneRemoved = "removed"
	GoneClosed  = "closed"
)

// Job is what one job holds.
type Job struct {
	Name            string
	Builds, Kept    int
	Keep            config.Keep
	Tests, Metrics  int64
	DBBytes         int64
	Logs, Artifacts int64
	Gone, Parent    string
	Newest, Until   time.Time
}

// Bytes is all the job takes: its rows, logs and artifacts.
func (j Job) Bytes() int64 { return j.DBBytes + j.Logs + j.Artifacts }

// Report reads the data directory and the database now. The measure of
// the tables is the last one taken; a stale one is taken again apart. It is
// Overview with the data directory's parts and the jobs and what to change.
func (s *Service) Report(ctx context.Context) (*Report, error) {
	r, err := s.Overview(ctx)
	if err != nil {
		return nil, err
	}
	if r.Parts, err = s.Parts(); err != nil {
		return nil, err
	}
	if r.Jobs, err = s.Jobs(ctx); err != nil {
		return nil, err
	}
	r.Recommendations = Recommend(r)
	return r, nil
}

// Overview is the report without what takes a walk of the data directory or
// a read of every job: the disk, the database's facts and measure, the
// compact, the self-check and the heal.
func (s *Service) Overview(ctx context.Context) (*Report, error) {
	r := &Report{At: s.o.Now()}
	free, total, ok := s.space(s.o.Data)
	r.Disk = Disk{Known: ok, Free: free, Total: total, Floor: floor(total)}
	f, err := s.o.Store.DBFile(ctx)
	if err != nil {
		return nil, err
	}
	dbPath := s.o.Store.Path()
	r.Database = Database{File: fileSize(dbPath), WAL: fileSize(dbPath + "-wal"), PageSize: f.PageSize, Pages: f.Pages,
		FreeBytes: f.FreeBytes(), UsedBytes: f.UsedBytes(), AutoVacuum: f.AutoVacuum.String()}
	s.MeasureSoon()
	s.mu.Lock()
	s.measuredInto(&r.Database)
	r.Compact = s.compact
	r.Check = s.check
	r.Heal = s.heal
	s.mu.Unlock()
	return r, nil
}

func (s *Service) measuredInto(db *Database) {
	db.Objects = s.objects.Objects
	db.Exact = s.objects.Exact
	db.MeasuredAt = s.measuredAt
	db.Measuring = s.measuring
	db.MeasureError = s.measureErr
}

// Parts walks the data directory.
func (s *Service) Parts() ([]Part, error) { return parts(s.o.Data) }

// Jobs reads what each job holds: the data directory's folders of logs and
// artifacts, and the rows by the last measure of the tables.
func (s *Service) Jobs(ctx context.Context) ([]Job, error) {
	var db Database
	s.mu.Lock()
	s.measuredInto(&db)
	s.mu.Unlock()
	return s.jobs(ctx, &db)
}

func fileSize(path string) int64 {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func treeSize(path string) int64 {
	var n int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

func partOf(name string) string {
	switch {
	case name == "db.sqlite" || name == "db.sqlite-wal" || name == "db.sqlite-shm":
		return PartDatabase
	case strings.HasPrefix(name, "db.schema-") && strings.HasSuffix(name, ".sqlite"):
		return PartSchemaCopies
	case strings.HasPrefix(name, ".upgrade-dry-run-") || name == "bin":
		return PartUpgradeBinaries
	case strings.HasPrefix(name, "."+CheckFile+"-"):
		return PartSmallFiles
	}
	switch name {
	case "logs":
		return PartLogs
	case "artifacts":
		return PartArtifacts
	case "cache":
		return PartCache
	case "workspace":
		return PartWorkspaces
	case "stage-tmp":
		return PartStageTemp
	case "upgrade":
		return PartUpgradeBackups
	case "secret.key", "device.key", "lock", "update-check.json", CheckFile, "apps", "secret-files":
		return PartSmallFiles
	}
	return PartOther
}

var partNames = map[string]string{
	PartDatabase: "db.sqlite, -wal, -shm", PartLogs: "logs/", PartArtifacts: "artifacts/", PartCache: "cache/",
	PartWorkspaces: "workspace/", PartStageTemp: "stage-tmp/", PartUpgradeBackups: "upgrade/",
	PartSchemaCopies: "db.schema-N.sqlite", PartUpgradeBinaries: "bin/, .upgrade-dry-run-*",
	PartSmallFiles: "keys, lock, apps/, secret-files/, small files",
}

var partOrder = []string{PartDatabase, PartLogs, PartArtifacts, PartCache, PartWorkspaces, PartStageTemp,
	PartUpgradeBackups, PartSchemaCopies, PartUpgradeBinaries, PartSmallFiles}

func parts(data string) ([]Part, error) {
	entries, err := os.ReadDir(data)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	sum := map[string]int64{}
	var others []Part
	for _, e := range entries {
		n := treeSize(filepath.Join(data, e.Name()))
		kind := partOf(e.Name())
		if kind == PartOther {
			others = append(others, Part{Kind: PartOther, Name: e.Name(), Bytes: n})
			continue
		}
		sum[kind] += n
	}
	out := make([]Part, 0, len(partOrder)+len(others))
	for _, kind := range partOrder {
		out = append(out, Part{Kind: kind, Name: partNames[kind], Bytes: sum[kind], Made: true})
	}
	slices.SortFunc(others, func(a, b Part) int { return cmp.Or(cmp.Compare(b.Bytes, a.Bytes), cmp.Compare(a.Name, b.Name)) })
	return append(out, others...), nil
}

func childSizes(dir string) map[string]int64 {
	out := map[string]int64{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() {
			out[e.Name()] = treeSize(filepath.Join(dir, e.Name()))
		}
	}
	return out
}

func (s *Service) jobs(ctx context.Context, db *Database) ([]Job, error) {
	rows, err := s.o.Store.JobRows(ctx)
	if err != nil {
		return nil, err
	}
	parents, err := s.o.Store.Parents(ctx)
	if err != nil {
		return nil, err
	}
	snap := s.snapshot()
	logs := childSizes(filepath.Join(s.o.Data, "logs"))
	arts := childSizes(filepath.Join(s.o.Data, "artifacts"))
	byName := map[string]*Job{}
	get := func(name string) *Job {
		if j, ok := byName[name]; ok {
			return j
		}
		j := &Job{Name: name}
		byName[name] = j
		return j
	}
	var totalTests, totalMetrics int64
	totalBuilds := 0
	for _, r := range rows {
		j := get(r.Job)
		j.Builds, j.Kept, j.Tests, j.Metrics, j.Newest = r.Builds, r.Kept, r.Tests, r.Metrics, r.Newest
		totalTests += r.Tests
		totalMetrics += r.Metrics
		totalBuilds += r.Builds
	}
	for name, n := range logs {
		get(name).Logs = n
	}
	for name, n := range arts {
		get(name).Artifacts = n
	}
	perTest, perMetric, perBuild := perRow(db.Objects, "tests", totalTests), perRow(db.Objects, "metric_history", totalMetrics),
		perRow(db.Objects, "builds", int64(totalBuilds))+perRow(db.Objects, "stages", int64(totalBuilds))
	out := make([]Job, 0, len(byName))
	for _, j := range byName {
		j.DBBytes = int64(perTest*float64(j.Tests) + perMetric*float64(j.Metrics) + perBuild*float64(j.Builds))
		j.Parent = parents[j.Name]
		if job, ok := snap.Jobs[j.Name]; ok {
			j.Keep = job.Keep
		} else {
			j.Gone = GoneRemoved
			if j.Parent != "" {
				j.Gone = GoneClosed
			}
			j.Keep = config.Keep{Builds: config.DefaultKeepBuilds, Days: config.RemovedKeepDays}
			if !j.Newest.IsZero() {
				j.Until = j.Newest.AddDate(0, 0, config.RemovedKeepDays)
			}
		}
		out = append(out, *j)
	}
	slices.SortFunc(out, func(a, b Job) int { return cmp.Or(cmp.Compare(b.Bytes(), a.Bytes()), cmp.Compare(a.Name, b.Name)) })
	return out, nil
}

func perRow(objs []store.DBObject, table string, rows int64) float64 {
	if rows == 0 {
		return 0
	}
	var b int64
	for _, o := range objs {
		if o.Table == table {
			b += o.Bytes
		}
	}
	return float64(b) / float64(rows)
}
