package state

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestCurrentAndPreviousRoundTripAndAnAbsentFileIsNone(t *testing.T) {
	d := t.TempDir()
	if v, err := ReadCurrent(d); v != "" || err != nil {
		t.Fatal(v, err)
	}
	if err := WriteCurrent(d, "v1.2.3"); err != nil {
		t.Fatal(err)
	}
	if v, _ := ReadCurrent(d); v != "v1.2.3" {
		t.Fatal(v)
	}
	if err := WritePrevious(d, Bundled); err != nil {
		t.Fatal(err)
	}
	if v, _ := ReadPrevious(d); v != Bundled {
		t.Fatal(v)
	}
}

func TestACurrentThatIsNotAReleaseVersionIsRefusedAndReadAsNone(t *testing.T) {
	d := t.TempDir()
	for _, bad := range []string{"bundled", "../x", "v1.2", "v1.2.3-rc1", ""} {
		if err := WriteCurrent(d, bad); err == nil {
			t.Fatalf("%q written to current", bad)
		}
	}
	if err := os.MkdirAll(BinDir(d), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(BinDir(d), "current"), []byte("../../etc/passwd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, err := ReadCurrent(d); v != "" || err != nil {
		t.Fatalf("read %q, %v", v, err)
	}
}

func TestPendingRoundTripsAndOnlyARollbackMayNameBundled(t *testing.T) {
	d := t.TempDir()
	if p, err := ReadPending(d); p != nil || err != nil {
		t.Fatal(p, err)
	}
	want := Pending{Kind: KindUpgrade, Version: "v1.0.1", Backup: "backup-v1.0.0-20261103T101500Z", From: "v1.0.0"}
	if err := WritePending(d, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPending(d)
	if err != nil || !reflect.DeepEqual(*got, want) {
		t.Fatal(got, err)
	}
	if err := WritePending(d, Pending{Kind: KindUpgrade, Version: Bundled}); err == nil {
		t.Fatal("an upgrade to bundled was written")
	}
	if err := WritePending(d, Pending{Kind: KindRollback, Version: Bundled, From: "v1.0.1"}); err != nil {
		t.Fatal(err)
	}
	if err := ClearPending(d); err != nil {
		t.Fatal(err)
	}
	if err := ClearPending(d); err != nil {
		t.Fatal("clearing nothing is not an error:", err)
	}
	if p, _ := ReadPending(d); p != nil {
		t.Fatal(p)
	}
}

func TestRejectedKeepsOneVersionPerLineOnceAndSkipsJunk(t *testing.T) {
	d := t.TempDir()
	if err := Reject(d, "v1.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := Reject(d, "v1.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := Reject(d, "v1.0.2"); err != nil {
		t.Fatal(err)
	}
	if err := Reject(d, "dev"); err == nil {
		t.Fatal("dev was rejected")
	}
	b, _ := os.ReadFile(filepath.Join(BinDir(d), "rejected"))
	if string(b) != "v1.0.1\nv1.0.2\n" {
		t.Fatalf("%q", b)
	}
	if err := os.WriteFile(filepath.Join(BinDir(d), "rejected"), []byte("v1.0.1\n\nx\n../y\nv1.0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _ := ReadRejected(d)
	if !reflect.DeepEqual(got, []string{"v1.0.1"}) {
		t.Fatal(got)
	}
}

func TestStatusAndFailureRoundTripAndLeaveNoTemporaryFile(t *testing.T) {
	d := t.TempDir()
	if s, err := ReadStatus(d); s != nil || err != nil {
		t.Fatal(s, err)
	}
	at := time.Date(2026, 11, 3, 10, 15, 0, 0, time.UTC)
	want := Status{State: StateSwitching, Version: "v1.0.1", From: "v1.0.0", Running: 0, Queued: 2, UpdatedAt: at}
	if err := WriteStatus(d, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadStatus(d)
	if err != nil || !got.UpdatedAt.Equal(at) || got.State != StateSwitching || got.Queued != 2 {
		t.Fatal(got, err)
	}
	f := Failure{Version: "v1.0.1", From: "v1.0.0", Reason: "did not answer", At: at}
	if err := WriteFailure(d, f); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadFailure(d); got == nil || got.Reason != "did not answer" {
		t.Fatal(got)
	}
	entries, _ := os.ReadDir(UpgradeDir(d))
	if len(entries) != 2 {
		t.Fatalf("%d files left in upgrade/", len(entries))
	}
}

func TestAWriteReplacesTheFileWholeOrNotAtAll(t *testing.T) {
	d := t.TempDir()
	if err := WriteCurrent(d, "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	// A directory where the file goes makes the rename fail; the old
	// state and the directory stay as they were.
	dir := filepath.Join(BinDir(d), "previous")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WritePrevious(d, "v0.9.0"); err == nil {
		t.Fatal("renamed over a directory")
	}
	entries, _ := os.ReadDir(BinDir(d))
	for _, e := range entries {
		if e.Name() != "current" && e.Name() != "previous" {
			t.Fatalf("left %s behind", e.Name())
		}
	}
}

func TestBinaryVersionNamesOnlyReleaseBinaries(t *testing.T) {
	for name, want := range map[string]string{"kipp-v1.0.1": "v1.0.1", "kipp-v1.0.1.tmp": "", "kipp-dev": "", "current": "", "kipp-v1.0": ""} {
		if got, ok := BinaryVersion(name); got != want && ok {
			t.Fatalf("%s: %s", name, got)
		} else if !ok && want != "" {
			t.Fatalf("%s not recognised", name)
		}
	}
	if BinaryPath("/d", "v1.0.1") != "/d/bin/kipp-v1.0.1" {
		t.Fatal(BinaryPath("/d", "v1.0.1"))
	}
}
