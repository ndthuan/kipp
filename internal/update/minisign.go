package update

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/blake2b"
)

// The files are in the format of the minisign tool (https://jedisct1.github.io/minisign/),
// so a user can check a release by hand with it. The secret key is the
// unencrypted form of its key file: the signing key lives in a CI secret,
// where a password would sit beside it.

const (
	algPure    = "Ed"
	algHashed  = "ED"
	idLen      = 8
	sigLen     = ed25519.SignatureSize
	secretLen  = 2 + 2 + 2 + 32 + 8 + 8 + idLen + ed25519.PrivateKeySize + 32
	trustedTag = "trusted comment: "
)

// PublicKey is a minisign public key.
type PublicKey struct {
	ID  [idLen]byte
	Key ed25519.PublicKey
}

// SecretKey is an unencrypted minisign secret key.
type SecretKey struct {
	ID  [idLen]byte
	Key ed25519.PrivateKey
}

// GenerateKey makes a new key pair.
func GenerateKey() (PublicKey, SecretKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return PublicKey{}, SecretKey{}, err
	}
	var id [idLen]byte
	if _, err := rand.Read(id[:]); err != nil {
		return PublicKey{}, SecretKey{}, err
	}
	return PublicKey{ID: id, Key: pub}, SecretKey{ID: id, Key: priv}, nil
}

// payloads returns the text lines of a key or signature file that are data:
// minisign's "untrusted comment:" line and our "#" comment lines are not.
func payloads(text []byte) []string {
	var out []string
	for line := range strings.SplitSeq(string(text), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "untrusted comment:") {
			continue
		}
		out = append(out, line)
	}
	return out
}

func decode(line string, n int, what string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(line)
	if err != nil {
		return nil, fmt.Errorf("%s is not base64: %w", what, err)
	}
	if len(b) != n {
		return nil, fmt.Errorf("%s is %d bytes, want %d", what, len(b), n)
	}
	return b, nil
}

// ParsePublicKey reads a minisign public key file; lines that start with #
// are comments.
func ParsePublicKey(text []byte) (PublicKey, error) {
	lines := payloads(text)
	if len(lines) != 1 {
		return PublicKey{}, errors.New("public key: want one key line")
	}
	b, err := decode(lines[0], 2+idLen+ed25519.PublicKeySize, "public key")
	if err != nil {
		return PublicKey{}, err
	}
	if string(b[:2]) != algPure {
		return PublicKey{}, errors.New("public key: not an ed25519 key")
	}
	var k PublicKey
	copy(k.ID[:], b[2:2+idLen])
	k.Key = ed25519.PublicKey(b[2+idLen:])
	return k, nil
}

// Marshal is the public key file.
func (k PublicKey) Marshal() []byte {
	raw := append(append([]byte(algPure), k.ID[:]...), k.Key...)
	return []byte(fmt.Sprintf("untrusted comment: minisign public key %X\n%s\n", reverse(k.ID), base64.StdEncoding.EncodeToString(raw)))
}

// reverse is the key id as minisign prints it.
func reverse(id [idLen]byte) []byte {
	out := make([]byte, idLen)
	for i, b := range id {
		out[idLen-1-i] = b
	}
	return out
}

func secretChecksum(id [idLen]byte, key ed25519.PrivateKey) [32]byte {
	return blake2b.Sum256(append(append([]byte(algPure), id[:]...), key...))
}

// Marshal is the secret key file.
func (k SecretKey) Marshal() []byte {
	raw := make([]byte, 0, secretLen)
	raw = append(raw, algPure...)
	raw = append(raw, 0, 0) // no key derivation: the key is not encrypted
	raw = append(raw, "B2"...)
	raw = append(raw, make([]byte, 32+8+8)...)
	raw = append(raw, k.ID[:]...)
	raw = append(raw, k.Key...)
	sum := secretChecksum(k.ID, k.Key)
	raw = append(raw, sum[:]...)
	return []byte("untrusted comment: minisign secret key\n" + base64.StdEncoding.EncodeToString(raw) + "\n")
}

// ParseSecretKey reads a secret key file made by SecretKey.Marshal.
func ParseSecretKey(text []byte) (SecretKey, error) {
	lines := payloads(text)
	if len(lines) != 1 {
		return SecretKey{}, errors.New("secret key: want one key line")
	}
	b, err := decode(lines[0], secretLen, "secret key")
	if err != nil {
		return SecretKey{}, err
	}
	if string(b[:2]) != algPure || string(b[4:6]) != "B2" {
		return SecretKey{}, errors.New("secret key: not an ed25519 key")
	}
	if b[2] != 0 || b[3] != 0 {
		return SecretKey{}, errors.New("secret key: password-protected keys are not supported")
	}
	var k SecretKey
	rest := b[2+2+2+32+8+8:]
	copy(k.ID[:], rest[:idLen])
	k.Key = ed25519.PrivateKey(bytes.Clone(rest[idLen : idLen+ed25519.PrivateKeySize]))
	want := secretChecksum(k.ID, k.Key)
	if !bytes.Equal(want[:], rest[idLen+ed25519.PrivateKeySize:]) {
		return SecretKey{}, errors.New("secret key: checksum does not match, the file is damaged")
	}
	return k, nil
}

// Sign signs msg and returns the signature file. The trusted comment is
// signed with the message, so it cannot be changed after.
func (k SecretKey) Sign(msg []byte, trusted string) []byte {
	return k.sign(algPure, msg, trusted)
}

// signHashed makes the prehashed form that newer minisign makes by default.
func (k SecretKey) signHashed(msg []byte, trusted string) []byte {
	sum := blake2b.Sum512(msg)
	return k.sign(algHashed, sum[:], trusted)
}

func (k SecretKey) sign(alg string, signed []byte, trusted string) []byte {
	sig := ed25519.Sign(k.Key, signed)
	global := ed25519.Sign(k.Key, append(bytes.Clone(sig), trusted...))
	raw := append(append([]byte(alg), k.ID[:]...), sig...)
	return []byte("untrusted comment: signature from kipp-release\n" +
		base64.StdEncoding.EncodeToString(raw) + "\n" +
		trustedTag + trusted + "\n" +
		base64.StdEncoding.EncodeToString(global) + "\n")
}

// VerifySignature checks that sigFile is a signature of msg by k.
func (k PublicKey) VerifySignature(msg, sigFile []byte) error {
	lines := strings.Split(strings.ReplaceAll(string(sigFile), "\r\n", "\n"), "\n")
	if len(lines) < 4 || !strings.HasPrefix(lines[0], "untrusted comment:") || !strings.HasPrefix(lines[2], trustedTag) {
		return errors.New("signature file is empty or cut short")
	}
	raw, err := decode(strings.TrimSpace(lines[1]), 2+idLen+sigLen, "signature")
	if err != nil {
		return err
	}
	global, err := decode(strings.TrimSpace(lines[3]), sigLen, "trusted comment signature")
	if err != nil {
		return err
	}
	if !bytes.Equal(raw[2:2+idLen], k.ID[:]) {
		return errors.New("signature is by another key")
	}
	sig := raw[2+idLen:]
	signed := msg
	switch string(raw[:2]) {
	case algPure:
	case algHashed:
		sum := blake2b.Sum512(msg)
		signed = sum[:]
	default:
		return errors.New("signature algorithm is not ed25519")
	}
	if !ed25519.Verify(k.Key, signed, sig) {
		return errors.New("signature does not match the data")
	}
	trusted := strings.TrimPrefix(lines[2], trustedTag)
	if !ed25519.Verify(k.Key, append(bytes.Clone(sig), trusted...), global) {
		return errors.New("trusted comment does not match its signature")
	}
	return nil
}
