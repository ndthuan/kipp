package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
)

// Hash is the SHA-256 of the files' contents, in the order given, in hex:
// what GitHub's hashFiles gives a cache key. A file that cannot be read is
// an error, and so is no file at all.
func Hash(files []string) (string, error) {
	if len(files) == 0 {
		return "", errors.New("no file to hash")
	}
	h := sha256.New()
	for _, name := range files {
		f, err := os.Open(name)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(h, f)
		_ = f.Close()
		if err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
