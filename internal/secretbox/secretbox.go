// Package secretbox seals secret values with the data directory's key
// (XChaCha20-Poly1305), so the database alone does not give them up.
package secretbox

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/crypto/chacha20poly1305"
)

// LoadOrCreateKey reads the 32-byte key at path, creating it (mode 0600) on
// first start. Creation is exclusive, so two first starts agree on one key.
// The key is opened through its directory as a root: a link in there
// cannot lead kipp to take another file for its key, or to write one
// elsewhere.
func LoadOrCreateKey(path string) ([]byte, error) {
	key := make([]byte, chacha20poly1305.KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	f, err := root.OpenFile(filepath.Base(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return readKey(root, path)
	}
	if err != nil {
		return nil, err
	}
	_, werr := f.Write(key)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return nil, werr
	}
	return key, nil
}

// readKey checks the mode of the file it reads, not of whatever the path
// named a moment before.
func readKey(root *os.Root, path string) ([]byte, error) {
	f, err := root.Open(filepath.Base(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s: mode %v lets group or others read the key; chmod 600 it", path, st.Mode().Perm())
	}
	if st.Size() != chacha20poly1305.KeySize {
		return nil, fmt.Errorf("%s: want %d bytes, got %d", path, chacha20poly1305.KeySize, st.Size())
	}
	key := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(f, key); err != nil {
		return nil, err
	}
	return key, nil
}

// Seal returns nonce || ciphertext. The value is sealed as the secret
// called name and opens under no other: the name is authenticated with it,
// so a sealed value moved to another secret's row is refused.
func Seal(key []byte, name string, plaintext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plaintext, []byte(name)), nil
}

// Open reverses Seal; a wrong key, another name or a changed byte is an error.
func Open(key []byte, name string, sealed []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < aead.NonceSize() {
		return nil, errors.New("sealed value too short")
	}
	nonce, box := sealed[:aead.NonceSize()], sealed[aead.NonceSize():]
	return aead.Open(nil, nonce, box, []byte(name))
}
