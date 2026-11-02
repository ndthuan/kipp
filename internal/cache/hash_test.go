package cache

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHashIsTheSameForTheSameFilesInTheSameOrder(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	writeFile(t, a, "alpha")
	writeFile(t, b, "beta")
	h1, err := Hash([]string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := Hash([]string{a, b})
	rev, _ := Hash([]string{b, a})
	if h1 != h2 || h1 == rev || len(h1) != 64 {
		t.Fatalf("hash %q again %q reversed %q", h1, h2, rev)
	}
	// SHA-256 of "alphabeta", the contents in the order given.
	if h1 != "a4c4aeb92c20500f364b12b3771ef3a11193e2cf04d0f28956a829749993b39f" {
		t.Fatalf("hash %s", h1)
	}
	writeFile(t, b, "gamma")
	if h3, _ := Hash([]string{a, b}); h3 == h1 {
		t.Fatal("changed contents, same hash")
	}
}

func TestHashOfAMissingFileIsAnError(t *testing.T) {
	if _, err := Hash([]string{filepath.Join(t.TempDir(), "nope")}); err == nil {
		t.Fatal("a missing file was hashed")
	}
	if _, err := Hash(nil); err == nil {
		t.Fatal("no files were hashed")
	}
}
