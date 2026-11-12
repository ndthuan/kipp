package store

import (
	"errors"
	"testing"
	"time"
)

func TestJobChangesAreWrittenTogetherAndReadBack(t *testing.T) {
	s := open(t)
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	first := &JobVersion{Job: "site", Text: "name: site\n", Hash: "h1", Author: "admin", How: HowAPI, At: at}
	other := &JobVersion{Job: "docs", Text: "name: docs\n", Hash: "h2", Author: AuthorKipp, How: HowFiles, At: at}
	must(t, s.ApplyJobChanges(ctx, JobChanges{
		Versions: []*JobVersion{first, other},
		Edits:    map[string]JobEdit{"site": {Version: first, Base: "file"}, "docs": {Version: other}},
		Files:    map[string]string{"site": "file", "docs": "h2"},
	}))
	if first.ID == 0 || other.ID <= first.ID {
		t.Fatalf("ids: %d, %d", first.ID, other.ID)
	}
	edits, err := s.JobEdits(ctx)
	must(t, err)
	if len(edits) != 2 || edits["site"].Base != "file" || edits["docs"].Base != "" || edits["site"].Version.Text != "name: site\n" || !edits["site"].Version.At.Equal(at) {
		t.Fatalf("edits: %+v", edits)
	}

	gone := &JobVersion{Job: "site", Deleted: true, Author: "admin", How: HowAPI, At: at.Add(time.Minute)}
	must(t, s.ApplyJobChanges(ctx, JobChanges{Versions: []*JobVersion{gone}, Retire: []string{"docs"}, Edits: map[string]JobEdit{"site": {Version: gone}}, Files: map[string]string{"docs": ""}}))
	edits, err = s.JobEdits(ctx)
	must(t, err)
	if len(edits) != 1 || !edits["site"].Version.Deleted || edits["site"].Base != "" {
		t.Fatalf("edits after: %+v", edits)
	}
	files, err := s.JobFiles(ctx)
	must(t, err)
	if len(files) != 1 || files["site"] != "file" {
		t.Fatalf("files: %v", files)
	}
	last, err := s.LastJobVersions(ctx)
	must(t, err)
	if len(last) != 2 || last["site"].ID != gone.ID || last["docs"].ID != other.ID {
		t.Fatalf("last: %+v", last)
	}
}

func TestAJobsVersionsArePagedNewestFirst(t *testing.T) {
	s := open(t)
	var ids []int64
	for range 3 {
		v := &JobVersion{Job: "site", Text: "name: site\n", Hash: "h", Author: "admin", How: HowAPI, At: time.Now()}
		must(t, s.ApplyJobChanges(ctx, JobChanges{Versions: []*JobVersion{v}}))
		ids = append(ids, v.ID)
	}
	page, err := s.JobVersions(ctx, "site", 0, 2)
	must(t, err)
	if len(page) != 2 || page[0].ID != ids[2] || page[1].ID != ids[1] {
		t.Fatalf("first page: %+v", page)
	}
	page, err = s.JobVersions(ctx, "site", page[1].ID, 2)
	must(t, err)
	if len(page) != 1 || page[0].ID != ids[0] {
		t.Fatalf("second page: %+v", page)
	}
	v, err := s.JobVersion(ctx, "site", ids[1])
	must(t, err)
	if v.ID != ids[1] || v.Author != "admin" {
		t.Fatalf("version: %+v", v)
	}
	if _, err := s.JobVersion(ctx, "docs", ids[1]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another job's version: %v", err)
	}
}

func TestTheSeedIsDueOnlyOnADataDirectoryThatNeverHeldAJobOrABuild(t *testing.T) {
	due := func(s *Store, want bool) {
		t.Helper()
		got, err := s.SeedDue(ctx)
		must(t, err)
		if got != want {
			t.Fatalf("seed due: %v, want %v", got, want)
		}
	}
	s := open(t)
	due(s, true)
	// A job file a load found is not a job kept in the store.
	found := &JobVersion{Job: "docs", Text: "name: docs\n", Hash: "h", Author: AuthorKipp, How: HowFiles, At: time.Now()}
	must(t, s.ApplyJobChanges(ctx, JobChanges{Versions: []*JobVersion{found}}))
	due(s, true)
	must(t, s.ApplyJobChanges(ctx, JobChanges{Mark: MarkSeed}))
	due(s, false)
	// Marking twice is no error.
	must(t, s.ApplyJobChanges(ctx, JobChanges{Mark: MarkSeed}))

	kept := open(t)
	made := &JobVersion{Job: "site", Text: "name: site\n", Hash: "h", Author: "admin", How: HowAPI, At: time.Now()}
	must(t, kept.ApplyJobChanges(ctx, JobChanges{Versions: []*JobVersion{made}}))
	due(kept, false)

	built := open(t)
	must(t, built.CreateBuild(ctx, &Build{Job: "site", Status: Queued, Trigger: "manual", QueuedAt: time.Now()}))
	due(built, false)
}
