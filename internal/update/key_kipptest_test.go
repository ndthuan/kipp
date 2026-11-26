//go:build kipptest

package update

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestTheKipptestBuildTakesTheKeyFileTheEnvironmentNames(t *testing.T) {
	if !bytes.Equal(ReleaseKey(), releasePub) {
		t.Fatal("without the variable the key is the embedded one")
	}
	pub, _, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "k.pub")
	if err := os.WriteFile(path, pub.Marshal(), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIPP_TEST_RELEASE_PUBKEY", path)
	if !bytes.Equal(ReleaseKey(), pub.Marshal()) {
		t.Fatal("the named key was not used")
	}
}
