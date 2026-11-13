package jobstore

import (
	"os"
	"path/filepath"
	"testing"
)

// A job file name that is not a plain name in the directory is refused at
// the file system itself, whatever the callers checked before.
func TestJobFileRefusesAPathOutOfTheDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "jobs")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../escape.yml", "sub/x.yml", "/abs.yml", "..", ".", ""} {
		if err := writeFile(dir, name, []byte("x")); err == nil {
			t.Errorf("writeFile(%q) wrote outside a plain name", name)
		}
		if err := removeFile(dir, name); err == nil {
			t.Errorf("removeFile(%q) removed outside a plain name", name)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "escape.yml")); err == nil {
		t.Fatal("a file was written outside the jobs directory")
	}
	if err := writeFile(dir, "site-smoke.yml", []byte("x")); err != nil {
		t.Fatalf("a plain name is refused: %v", err)
	}
	if err := removeFile(dir, "site-smoke.yml"); err != nil {
		t.Fatalf("a plain name is not removed: %v", err)
	}
}
