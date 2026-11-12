package jobstore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

// seed is Seed on the fixture, failing on an error.
func (f *fixture) seed() bool {
	f.t.Helper()
	seeded, err := f.jobs.Seed(ctx)
	must(f.t, err)
	return seeded
}

func TestANewStoreServerCreatesTheTwoExampleJobsOnceAndADeletedOneStaysGone(t *testing.T) {
	f := newFixture(t, Store, nil)
	if !f.seed() {
		t.Fatal("a new server in store mode created no example job")
	}
	f.serves("hello", `echo "hello, $WHO"`, FromStore)
	f.serves("hello-schedule", "the time here is", FromStore)
	for _, name := range []string{"hello", "hello-schedule"} {
		v := f.versions(name)
		if len(v) != 1 || v[0].How != store.HowSeed || v[0].Author != store.AuthorKipp {
			t.Fatalf("%s versions: %+v", name, v)
		}
	}

	f.open(Store)
	if f.seed() {
		t.Fatal("started twice, the example jobs were created twice")
	}
	if n := len(f.jobs.Snapshot().Names); n != 2 {
		t.Fatalf("started twice, %d jobs", n)
	}

	_, err := f.jobs.Delete(ctx, "hello", f.etag("hello"), byAPI)
	must(t, err)
	f.open(Store)
	f.seed()
	f.absent("hello")
	f.serves("hello-schedule", "the time here is", FromStore)
}

func TestNoExampleJobIsCreatedInOffOrFilesMode(t *testing.T) {
	for _, mode := range []Mode{Off, Files} {
		f := newFixture(t, mode, nil)
		if f.seed() || len(f.jobs.Snapshot().Names) != 0 {
			t.Fatalf("%s mode created the example jobs", mode)
		}
		due, err := f.st.SeedDue(ctx)
		must(t, err)
		if !due {
			t.Fatalf("%s mode decided the seed for a later start in store mode", mode)
		}
	}
}

func TestAJobFilePresentAtTheFirstStartMeansNoExampleJobsEver(t *testing.T) {
	f := newFixture(t, Store, map[string]string{"site.yml": jobText("site", "true")})
	if f.seed() {
		t.Fatal("the example jobs were created beside a job file")
	}
	f.remove("site.yml")
	f.load()
	if f.seed() {
		t.Fatal("the example jobs were created once the job file was gone")
	}
	f.absent("hello")
}

func TestAServerThatHasBuiltNeverGrowsTheExampleJobs(t *testing.T) {
	f := newFixture(t, Store, nil)
	must(t, f.st.CreateBuild(ctx, &store.Build{Job: "gone", Status: store.Success, Trigger: "cli"}))
	if f.seed() {
		t.Fatal("an upgraded server whose jobs directory is empty grew the example jobs")
	}
}

func TestTheSeedIsPublishedAndPassesTheCheckWithAVarsFile(t *testing.T) {
	f := newFixture(t, Store, map[string]string{config.VarsFile: "global: {GREETING: hi}\n"})
	before := f.published
	if !f.seed() || f.published != before+1 {
		t.Fatalf("seeded with a vars file: published %d times", f.published-before)
	}
	var got *config.Snapshot
	f.jobs.o.Publish = func(_ context.Context, s *config.Snapshot) { got = s }
	f.load()
	if got == nil || got.Jobs["hello"] == nil {
		t.Fatal("a load after the seed lost the example jobs")
	}
}

func TestTheExampleJobsAreTheExamplesWordForWord(t *testing.T) {
	seed := SeedJobs()
	if len(seed) != 2 {
		t.Fatalf("%d example jobs", len(seed))
	}
	examples, err := os.OpenRoot(filepath.Join("..", "..", "examples", "jobs"))
	must(t, err)
	defer func() { _ = examples.Close() }()
	for name, text := range seed {
		want, err := examples.ReadFile(name)
		must(t, err)
		if !bytes.Equal(text, want) {
			t.Errorf("%s differs from examples/jobs/%s", name, name)
		}
	}
	if _, err := config.Check(seed); err != nil {
		t.Fatalf("the example jobs fail the check: %v", err)
	}
}
