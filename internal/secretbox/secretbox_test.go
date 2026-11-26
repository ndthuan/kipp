package secretbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newKey(t *testing.T, name string) []byte {
	t.Helper()
	key, err := LoadOrCreateKey(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestKeyIsCreatedOnceWithMode600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")
	k1, err := LoadOrCreateKey(path)
	if err != nil || len(k1) != 32 {
		t.Fatal(err, len(k1))
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	k2, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(k1) != string(k2) {
		t.Fatal("key changed on second load")
	}
}

func TestAKeyOfTheWrongSizeIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(path); err == nil {
		t.Fatal("a 5-byte key was accepted")
	}
}

func TestSealOpenRoundTripAndTamper(t *testing.T) {
	key := newKey(t, "k")
	sealed, err := Seal(key, "GREETING", []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open(key, "GREETING", sealed)
	if err != nil || string(got) != "hello" {
		t.Fatal(err, string(got))
	}
	if _, err := Open(newKey(t, "k2"), "GREETING", sealed); err == nil {
		t.Fatal("wrong key opened")
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := Open(key, "GREETING", sealed); err == nil {
		t.Fatal("tampered box opened")
	}
	if _, err := Open(key, "GREETING", []byte("tiny")); err == nil {
		t.Fatal("a truncated box opened")
	}
}

// Whoever can write the database and not read the key could otherwise put
// one secret's sealed value in another's row, and have a job that may read
// the second print the first.
func TestAValueSealedUnderOneNameDoesNotOpenUnderAnother(t *testing.T) {
	key := newKey(t, "k")
	sealed, err := Seal(key, "DEPLOY_KEY", []byte("the deploy key"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"CHAT_TOKEN", "", "DEPLOY_KEY2", "deploy_key"} {
		if got, err := Open(key, name, sealed); err == nil {
			t.Fatalf("opened as %q: %q", name, got)
		}
	}
	if got, err := Open(key, "DEPLOY_KEY", sealed); err != nil || string(got) != "the deploy key" {
		t.Fatal(err, string(got))
	}
}

func TestAKeyFileOpenToOthersIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(path, make([]byte, 32), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadOrCreateKey(path)
	if err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("got %v", err)
	}
}
