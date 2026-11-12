package jobstore

import (
	"context"
	"embed"
	"maps"
	"slices"
	"strings"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

// The example jobs are examples/jobs/hello.yml and hello-schedule.yml,
// word for word: a test holds them equal.
//
//go:embed seed/*.yml
var seedFS embed.FS

// SeedJobs are the example jobs a new server in store mode starts with, by
// file name.
func SeedJobs() config.Files {
	entries, err := seedFS.ReadDir("seed")
	if err != nil {
		panic(err) // embedded at build time
	}
	out := config.Files{}
	for _, e := range entries {
		data, err := seedFS.ReadFile("seed/" + e.Name())
		if err != nil {
			panic(err)
		}
		out[e.Name()] = data
	}
	return out
}

// Seed creates the example jobs once, in store mode, on a data directory
// that never held a job of the store or a build, when the set serves no
// job: a new server has something to run. A server that serves a job when
// the seed is first due decides for good not to, so that a jobs directory
// emptied later does not grow them. It reports whether it created them.
func (j *Jobs) Seed(ctx context.Context) (bool, error) {
	if j.o.Mode != Store {
		return false, nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	due, err := j.o.Store.SeedDue(ctx)
	if err != nil || !due {
		return false, err
	}
	decided := store.JobChanges{Mark: store.MarkSeed}
	if len(j.snap.Names) > 0 {
		return false, j.o.Store.ApplyJobChanges(ctx, decided)
	}
	seed := SeedJobs()
	try := maps.Clone(j.served)
	maps.Copy(try, seed)
	snap, err := config.Check(try)
	if err != nil {
		j.o.Log.Warn("the example jobs do not fit the jobs directory's _vars.yml; not creating them", "err", err)
		return false, j.o.Store.ApplyJobChanges(ctx, decided)
	}
	origins := maps.Clone(j.origins)
	c := store.JobChanges{Edits: map[string]store.JobEdit{}, Mark: store.MarkSeed}
	var names []string
	for file := range seed {
		names = append(names, strings.TrimSuffix(file, ".yml"))
	}
	slices.Sort(names)
	for _, name := range names {
		v := &store.JobVersion{Job: name, Text: string(seed[config.FileName(name)]), Hash: snap.Hashes[name], Author: store.AuthorKipp, How: store.HowSeed, At: j.o.Now()}
		c.Versions = append(c.Versions, v)
		c.Edits[name] = store.JobEdit{Version: v}
		origins[name] = FromStore
	}
	if err := j.o.Store.ApplyJobChanges(ctx, c); err != nil {
		return false, err
	}
	j.served, j.snap, j.origins = try, snap, origins
	j.o.Log.Info("example jobs created; change or delete them as you like", "jobs", strings.Join(names, ","))
	j.o.Publish(ctx, snap)
	return true, nil
}
