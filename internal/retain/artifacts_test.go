package retain

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

// keepFile gives build b of job a kept file of size bytes, on disk and in
// the store.
func keepFile(t *testing.T, st *store.Store, arts string, b *store.Build, size int) {
	t.Helper()
	dir := filepath.Join(arts, b.Job, strconv.Itoa(b.Number), "s")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "out.bin"), make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.PutArtifacts(context.Background(), b.ID, "s", []store.Artifact{{Stage: "s", Path: "out.bin", Size: int64(size), SHA256: strings.Repeat("0", 64)}}); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactsGoWithTheirBuild(t *testing.T) {
	st, logs := openStore(t)
	ctx := context.Background()
	now := time.Now()
	seed(t, st, logs, "a", 3, now)
	arts := ArtifactsDir(logs)
	builds, _ := st.Builds(ctx, "a", 10)
	for _, b := range builds {
		keepFile(t, st, arts, b, 4)
	}
	job := parse(t, "name: a\nkeep: {builds: 1}\nstages:\n  s: {run: true}\n")
	if err := Job(ctx, st, job, logs, now); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(arts, "a", "1")) || exists(filepath.Join(arts, "a", "2")) {
		t.Fatal("the removed builds' artifacts are still there")
	}
	if !exists(filepath.Join(arts, "a", "3", "s", "out.bin")) {
		t.Fatal("the kept build's artifacts are gone")
	}
	if rows, _ := st.Artifacts(ctx, builds[0].ID); len(rows) != 1 {
		t.Fatalf("%+v", rows)
	}
}

func TestArtifactsMaxRemovesTheOldestBuildsFirstAndAudits(t *testing.T) {
	st, logs := openStore(t)
	ctx := context.Background()
	now := time.Now()
	arts := ArtifactsDir(logs)
	seed(t, st, logs, "site-smoke", 3, now)
	seed(t, st, logs, "site-deploy", 1, now)
	running := &store.Build{Job: "site-smoke", Status: store.Running, Trigger: "cli", QueuedAt: now.AddDate(0, 0, -9)}
	if err := st.CreateBuild(ctx, running); err != nil {
		t.Fatal(err)
	}
	smoke, _ := st.Builds(ctx, "site-smoke", 10) // newest first: the running one, 3, 2, 1
	deploy, _ := st.Builds(ctx, "site-deploy", 10)
	keepFile(t, st, arts, smoke[3], 10) // site-smoke #1, the oldest
	keepFile(t, st, arts, smoke[2], 10)
	keepFile(t, st, arts, deploy[0], 10)
	keepFile(t, st, arts, smoke[1], 10)
	keepFile(t, st, arts, running, 100)
	// 140 in all: the running build's alone are over 25, and stay.
	n, err := ArtifactsOver(ctx, st, arts, 125, now)
	if err != nil || n != 2 {
		t.Fatalf("%d %v", n, err)
	}
	keeps(t, st, arts, map[*store.Build]bool{smoke[3]: false, smoke[2]: false, deploy[0]: true, smoke[1]: true, running: true})
	audit, err := st.AuditBefore(ctx, 0, 10)
	if err != nil || len(audit) != 2 {
		t.Fatalf("%v %+v", err, audit)
	}
	if a := audit[1]; a.Actor != "kipp" || a.Action != "artifacts.remove" || a.Target != "site-smoke #1" || !strings.Contains(a.Detail, "10 bytes") {
		t.Fatalf("%+v", a)
	}
	if n, err := ArtifactsOver(ctx, st, arts, 1, now); err != nil || n != 2 {
		t.Fatalf("down to the running build: %d %v", n, err)
	}
	if rows, _ := st.Artifacts(ctx, running.ID); len(rows) != 1 {
		t.Fatal("a running build's artifacts were removed")
	}
}

// keeps fails unless each build keeps its artifacts, or has none left on
// disk or in the store, as want says.
func keeps(t *testing.T, st *store.Store, arts string, want map[*store.Build]bool) {
	t.Helper()
	for b, kept := range want {
		rows, err := st.Artifacts(context.Background(), b.ID)
		there := exists(filepath.Join(arts, b.Job, strconv.Itoa(b.Number)))
		if err != nil || (len(rows) == 1) != kept || there != kept {
			t.Fatalf("%s #%d: %d rows, on disk %v, want kept %v (%v)", b.Job, b.Number, len(rows), there, kept, err)
		}
	}
}
