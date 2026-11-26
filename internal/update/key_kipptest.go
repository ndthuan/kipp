//go:build kipptest

package update

import "os"

func releaseKey() []byte {
	if path := os.Getenv("KIPP_TEST_RELEASE_PUBKEY"); path != "" {
		if b, err := os.ReadFile(path); err == nil {
			return b
		}
	}
	return releasePub
}
