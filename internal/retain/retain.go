// Package retain applies each job's keep rule to its builds and logs, and
// bounds the audit and the record of webhook deliveries.
package retain

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

const (
	// AuditRows is how many audit rows are kept: years of changes on a
	// server a few people run, and a bound on what a page of it costs.
	AuditRows = 10000
	// DeliveryDays is how long a webhook delivery's id is remembered, so
	// that the same delivery sent again in that time is refused.
	DeliveryDays = 30
	// OAuthClientDays is how long an app that registered itself and was
	// never used is kept.
	OAuthClientDays = 30
)

// Job removes the job's builds beyond its keep rule, with their log folders.
func Job(ctx context.Context, st *store.Store, job *config.Job, logsDir string, now time.Time) error {
	_, err := apply(ctx, st, job.Name, job.Keep, logsDir, now)
	return err
}

func apply(ctx context.Context, st *store.Store, name string, keep config.Keep, logsDir string, now time.Time) (int, error) {
	ids, err := st.OldBuilds(ctx, name, keep.Builds, keep.Days, now)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	if !fs.ValidPath(name) || name == "." || filepath.Base(name) != name {
		return 0, fmt.Errorf("retain: job name %q is not a single path element", name)
	}
	root, err := os.OpenRoot(logsDir)
	if errors.Is(err, fs.ErrNotExist) {
		root, err = nil, nil
	}
	if err != nil {
		return 0, err
	}
	if root != nil {
		defer func() { _ = root.Close() }()
	}
	n := 0
	var errs []error
	for _, id := range ids {
		gone, err := remove(ctx, st, root, ArtifactsDir(logsDir), name, id)
		if err != nil {
			errs = append(errs, fmt.Errorf("retain: %s, build %d: %w", name, id, err))
		}
		if gone {
			n++
		}
	}
	return n, errors.Join(errs...)
}

// remove deletes one listed build: its log folder, exactly <root>/<job>/<number>
// (root is nil when there is no logs directory), its artifacts under arts,
// then its row.
func remove(ctx context.Context, st *store.Store, root *os.Root, arts, name string, id int64) (bool, error) {
	b, err := st.Build(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if root != nil && b.Number > 0 {
		if err := root.RemoveAll(filepath.Join(name, strconv.Itoa(b.Number))); err != nil {
			return false, err
		}
	}
	if err := removeArtifacts(arts, b); err != nil {
		return false, err
	}
	err = st.DeleteBuild(ctx, id)
	return err == nil, err
}

// Sweep is the daily pass: every job in the snapshot by its rule, every
// job with no job file by the rule for removed jobs, then expired sessions,
// the audit beyond AuditRows and deliveries older than DeliveryDays.
// A job or a build that fails does not stop the rest; the error holds every
// failure, the count what was removed all the same.
func Sweep(ctx context.Context, st *store.Store, snap *config.Snapshot, logsDir string, now time.Time) (int, error) {
	rules := make(map[string]config.Keep, len(snap.Names))
	names := slices.Clone(snap.Names)
	for _, name := range snap.Names {
		rules[name] = snap.Jobs[name].Keep
	}
	removed, err := removedJobs(ctx, st, logsDir, rules)
	errs := []error{err}
	for _, name := range removed {
		// The default rule and a limit in days: with no build to come, the
		// newest ones would otherwise never be beyond the count.
		rules[name] = config.Keep{Builds: config.DefaultKeepBuilds, Days: config.RemovedKeepDays}
	}
	total := 0
	for _, name := range slices.Concat(names, removed) {
		n, err := apply(ctx, st, name, rules[name], logsDir, now)
		total += n
		errs = append(errs, err)
	}
	errs = append(errs, st.DeleteExpiredSessions(ctx, now), st.PruneAudit(ctx, AuditRows),
		st.PruneDeliveries(ctx, now.AddDate(0, 0, -DeliveryDays)),
		st.DeleteExpiredOAuth(ctx, now, now.AddDate(0, 0, -OAuthClientDays)))
	return total, errors.Join(errs...)
}

// removedJobs names, in order, what has a folder of logs or a build in the
// store and is not in known. Both are asked: a build that ended before it
// wrote a log has no folder, and a folder may outlive its builds' rows. An
// error comes with the names that could still be read.
func removedJobs(ctx context.Context, st *store.Store, logsDir string, known map[string]config.Keep) ([]string, error) {
	found := map[string]bool{}
	entries, dirErr := os.ReadDir(logsDir)
	if errors.Is(dirErr, fs.ErrNotExist) {
		dirErr = nil
	}
	for _, e := range entries {
		if e.IsDir() {
			found[e.Name()] = true
		}
	}
	last, err := st.LastBuilds(ctx)
	for name := range last {
		found[name] = true
	}
	for name := range known {
		delete(found, name)
	}
	return slices.Sorted(maps.Keys(found)), errors.Join(dirErr, err)
}

// ArtifactsDir is where the builds' artifacts are, beside their logs in
// the data directory.
func ArtifactsDir(logsDir string) string {
	return filepath.Join(filepath.Dir(logsDir), "artifacts")
}

// removeArtifacts removes the folder of b's artifacts, exactly
// <arts>/<job>/<number>, whatever links are in it.
func removeArtifacts(arts string, b *store.Build) error {
	if b.Number <= 0 {
		return nil
	}
	root, err := os.OpenRoot(arts)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return root.RemoveAll(filepath.Join(b.Job, strconv.Itoa(b.Number)))
}

// ArtifactsOver removes the artifacts of the oldest ended builds until
// all of them come to max bytes at most, each removal a row of the audit,
// and is how many builds lost theirs. A running build's are never
// removed, even when they alone are over.
func ArtifactsOver(ctx context.Context, st *store.Store, arts string, max int64, now time.Time) (int, error) {
	builds, total, err := st.ArtifactBuilds(ctx)
	if err != nil || total <= max {
		return 0, err
	}
	n := 0
	for _, b := range builds {
		if total <= max {
			break
		}
		if err := removeArtifacts(arts, &store.Build{Job: b.Job, Number: b.Number}); err != nil {
			return n, fmt.Errorf("retain: artifacts of %s #%d: %w", b.Job, b.Number, err)
		}
		if err := st.DeleteArtifacts(ctx, b.ID); err != nil {
			return n, err
		}
		total -= b.Size
		n++
		err := st.AppendAudit(ctx, store.Audit{At: now, Actor: "kipp", Action: "artifacts.remove",
			Target: fmt.Sprintf("%s #%d", b.Job, b.Number), Detail: fmt.Sprintf("%d bytes, the oldest build's while the artifacts were over --artifacts-max of %d", b.Size, max)})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
