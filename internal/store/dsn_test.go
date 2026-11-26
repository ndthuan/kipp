package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestADataDirectoryNamedWithAQuestionMarkAHashOrAPercentHoldsItsOwnDatabase(t *testing.T) {
	for _, name := range []string{"a?b", "a?mode=memory", "a#b", "a%41b", "100%", "a b", "all?of#them%2F"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), name)
			must(t, os.Mkdir(dir, 0o750))
			path := filepath.Join(dir, "db.sqlite")
			s, err := Open(path)
			must(t, err)
			must(t, s.CreateBuild(ctx, &Build{Job: "a", Status: Success, Trigger: "cli", QueuedAt: cacheT0}))
			objs, err := s.MeasureObjects(ctx)
			must(t, err)
			if len(objs.Objects) == 0 {
				t.Fatal("the read-only connection measured nothing")
			}
			must(t, s.Close())
			if info, err := os.Stat(path); err != nil || info.Size() == 0 {
				t.Fatalf("the database is not the file asked for: %v", err)
			}
			entries, err := os.ReadDir(filepath.Dir(dir))
			must(t, err)
			if len(entries) != 1 {
				t.Fatalf("files beside the data directory: %v", entries)
			}
			again, err := OpenCurrent(path)
			must(t, err)
			builds, err := again.Builds(ctx, "a", 5)
			must(t, err)
			must(t, again.Close())
			if len(builds) != 1 {
				t.Fatalf("%d builds read back, want 1", len(builds))
			}
		})
	}
}
