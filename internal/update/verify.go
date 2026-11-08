// Package update holds what kipp checks about a release before it runs
// one: that the list of checksums is signed by the release key, and that the
// archive is on that list.
package update

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// The three ways a download is refused; each is wrapped with detail.
var (
	// ErrSignature: SHA256SUMS is not signed by the release key.
	ErrSignature = errors.New("signature check failed")
	// ErrNotListed: SHA256SUMS has no line for the archive.
	ErrNotListed = errors.New("archive not listed")
	// ErrChecksum: the archive is not the one SHA256SUMS lists.
	ErrChecksum = errors.New("checksum check failed")
)

// Verify checks a downloaded archive. pub is a public key file, sig the
// signature file of sums, and name the archive's file name in sums. The
// signature is checked first, so a checksum is trusted only from a signed list.
func Verify(pub, archive, sums, sig []byte, name string) error {
	key, err := ParsePublicKey(pub)
	if err != nil {
		return fmt.Errorf("release key: %w", err)
	}
	if err := key.VerifySignature(sums, sig); err != nil {
		return fmt.Errorf("%w: SHA256SUMS is not signed by the release key: %w", ErrSignature, err)
	}
	listed := listedSums(sums, name)
	if len(listed) == 0 {
		return fmt.Errorf("%w: %s has no line in SHA256SUMS", ErrNotListed, name)
	}
	got := sha256.Sum256(archive)
	for _, want := range listed {
		if !bytes.Equal(want, got[:]) {
			return fmt.Errorf("%w: %s does not match its sha256 in SHA256SUMS", ErrChecksum, name)
		}
	}
	return nil
}

// listedSums returns the digests sums lists for name, in the format of
// sha256sum: a hex digest, a space, a space or *, the file name.
func listedSums(sums []byte, name string) [][]byte {
	var out [][]byte
	for line := range strings.SplitSeq(string(sums), "\n") {
		hexSum, file, ok := strings.Cut(strings.TrimSuffix(line, "\r"), " ")
		if !ok || len(file) < 2 || (file[0] != ' ' && file[0] != '*') || file[1:] != name {
			continue
		}
		if b, err := hex.DecodeString(hexSum); err == nil && len(b) == sha256.Size {
			out = append(out, b)
		}
	}
	return out
}
