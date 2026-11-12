package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

// importJobs makes every job of dir a job of the store of the data
// directory, with no file under it: all of them or, when the directory has
// an error or the store holds one of the names already, none.
func importJobs(dir, data string, usage func(), stdout, stderr io.Writer) int {
	return onStore("job import", data, usage, stderr, func(ctx context.Context, st *store.Store) error {
		files, snap, err := readImport(dir)
		if err != nil {
			return err
		}
		edits, err := st.JobEdits(ctx)
		if err != nil {
			return err
		}
		held := slices.DeleteFunc(slices.Clone(snap.Names), func(n string) bool { _, ok := edits[n]; return !ok })
		if len(held) > 0 {
			return fmt.Errorf("the store already holds %s; nothing imported", strings.Join(held, ", "))
		}
		c := store.JobChanges{Edits: map[string]store.JobEdit{}}
		now := time.Now()
		for _, name := range snap.Names {
			v := &store.JobVersion{Job: name, Text: string(snap.Texts[name]), Hash: config.DecodedHash(snap.Jobs[name]), Author: "cli", How: store.HowImport, At: now}
			c.Versions = append(c.Versions, v)
			c.Edits[name] = store.JobEdit{Version: v}
		}
		if err := st.ApplyJobChanges(ctx, c); err != nil {
			return err
		}
		say(stdout, "imported %s into the store: %s", jobCount(len(snap.Names)), strings.Join(snap.Names, ", "))
		say(stdout, "a running server takes them at its next reload or restart; start it without --jobs %s, or a job file of the same name serves and the stored job waits", dir)
		if _, ok := files[config.VarsFile]; ok {
			say(stdout, "%s is not a job and was not imported: its variables reach the jobs only while --jobs names a directory that holds it", config.VarsFile)
		}
		return record(ctx, st, "job.import", dir, jobCount(len(snap.Names)))
	})
}

// readImport reads and checks the job files of dir, as kipp check does.
func readImport(dir string) (config.Files, *config.Snapshot, error) {
	files, err := config.ReadDir(dir)
	var snap *config.Snapshot
	if err == nil {
		snap, err = config.Check(files)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%s has errors; nothing imported:\n%w", dir, err)
	}
	if len(snap.Names) == 0 {
		return nil, nil, fmt.Errorf("%s holds no job file; nothing imported", dir)
	}
	return files, snap, nil
}
