package artifacts

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/config"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A glob that climbs out with .. is refused by kipp check; a link that
// leads out of the stage's directory, however its glob found it, is
// refused when the stage ends, and nothing of it is read.
func TestAMatchOutsideTheStageDirectoryIsRefused(t *testing.T) {
	for _, glob := range []string{"../x", "a/../../x", "/etc/passwd"} {
		_, err := config.ParseJob("a.yml", []byte("name: a\nstages:\n  s: {artifacts: {paths: [\""+glob+"\"]}, run: 'true'}\n"))
		if err == nil || !strings.Contains(err.Error(), "leaves the stage's directory") {
			t.Fatalf("%s: %v", glob, err)
		}
	}
	outside := t.TempDir()
	writeFiles(t, outside, map[string]string{"secret.txt": "not the stage's"})
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"out/ok.txt": "ok"})
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "out", "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "up")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../"+filepath.Base(outside), filepath.Join(dir, "rel")); err != nil {
		t.Fatal(err)
	}
	for _, glob := range []string{"out/*", "out", "out/link.txt", "up/secret.txt", "rel"} {
		files, err := Collect(dir, []string{glob}, 1<<20)
		if _, outsideErr := errors.AsType[*OutsideError](err); !outsideErr {
			t.Fatalf("%s: %v %+v", glob, err, files)
		}
	}
	// A link inside the directory is its target's contents.
	if err := os.Symlink("ok.txt", filepath.Join(dir, "out", "same.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "out", "link.txt")); err != nil {
		t.Fatal(err)
	}
	files, err := Collect(dir, []string{"out/*"}, 1<<20)
	if err != nil || len(files) != 2 || files[1].Path != "out/same.txt" || files[1].Size != 2 {
		t.Fatalf("%v %+v", err, files)
	}
}

func TestCollectCountsTheMatchesOnceAndRefusesWhatIsOverMax(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"a.txt": "12345", "d/b.txt": "123", "d/e/c.txt": "1"})
	files, err := Collect(dir, []string{"*.txt", "a.txt", "d"}, 9)
	if err != nil || len(files) != 3 || files[0].Path != "a.txt" || files[2].Path != "d/e/c.txt" {
		t.Fatalf("%v %+v", err, files)
	}
	if files[0].SHA256 != "5994471abb01112afcc18159f6cc74b4f511b99806da59b3caf5a9c173cacfc5" {
		t.Fatalf("%s", files[0].SHA256)
	}
	_, err = Collect(dir, []string{"*.txt", "d"}, 8)
	if big, ok := errors.AsType[*TooLargeError](err); !ok || big.Error() != "artifacts: 9 bytes over the stage's max_size of 8" {
		t.Fatalf("%v", err)
	}
	if files, err := Collect(filepath.Join(dir, "none"), []string{"*"}, 1); err != nil || files != nil {
		t.Fatalf("a directory that is not there: %v %v", err, files)
	}
}

func TestTheZipHoldsTheStagesFilesUnderTheirPaths(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"r/index.html": "<p>hi</p>", "log.txt": "line"})
	files, err := Collect(dir, []string{"r", "log.txt"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Zip(&buf, dir, files); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(rc)
		_ = rc.Close()
		got[f.Name] = string(data)
	}
	if len(got) != 2 || got["r/index.html"] != "<p>hi</p>" || got["log.txt"] != "line" {
		t.Fatalf("%v", got)
	}
}

func TestATarOfADirectoryUnpacksAsItWas(t *testing.T) {
	src := t.TempDir()
	writeFiles(t, src, map[string]string{"a.txt": "a", "d/b.txt": "bb"})
	var buf bytes.Buffer
	if err := Tar(&buf, src); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "in")
	files, err := Untar(&buf, dst, 3, -1)
	if err != nil || len(files) != 2 || files[1].Path != "d/b.txt" {
		t.Fatalf("%v %+v", err, files)
	}
	if data, _ := os.ReadFile(filepath.Join(dst, "d", "b.txt")); string(data) != "bb" {
		t.Fatalf("%q", data)
	}
	var empty bytes.Buffer
	if err := Tar(&empty, filepath.Join(src, "none")); err != nil {
		t.Fatal(err)
	}
	if files, err := Untar(&empty, filepath.Join(t.TempDir(), "e"), 1, -1); err != nil || len(files) != 0 {
		t.Fatalf("%v %v", err, files)
	}
}
