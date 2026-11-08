//go:build kipptest

package update

import "os"

// The end-to-end smoke signs a release with a key of its own and names the
// public key file here; a build without the kipptest tag has no such door.
func releaseKey() []byte {
	if path := os.Getenv("KIPP_TEST_RELEASE_PUBKEY"); path != "" {
		if b, err := os.ReadFile(path); err == nil {
			return b
		}
	}
	return releasePub
}
