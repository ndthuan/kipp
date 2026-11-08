package update

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type release struct {
	pub     []byte
	archive []byte
	sums    []byte
	sig     []byte
	secret  SecretKey
}

const archiveName = "kipp_1.2.3_linux_amd64.tar.gz"

func newRelease(t *testing.T) release {
	t.Helper()
	pub, sec, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	archive := []byte("an archive of kipp")
	sum := sha256.Sum256(archive)
	other := sha256.Sum256([]byte("another"))
	sums := []byte(hex.EncodeToString(other[:]) + "  kipp_1.2.3_darwin_arm64.tar.gz\n" + hex.EncodeToString(sum[:]) + "  " + archiveName + "\n")
	return release{pub.Marshal(), archive, sums, sec.Sign(sums, "file:SHA256SUMS"), sec}
}

func (r release) verify() error { return Verify(r.pub, r.archive, r.sums, r.sig, archiveName) }

func TestAGoodArchiveVerifies(t *testing.T) {
	if err := newRelease(t).verify(); err != nil {
		t.Fatal(err)
	}
}

func TestAFlippedByteInTheArchiveFailsTheChecksum(t *testing.T) {
	r := newRelease(t)
	r.archive[3] ^= 1
	err := r.verify()
	if !errors.Is(err, ErrChecksum) || errors.Is(err, ErrSignature) || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("got %v", err)
	}
}

func TestAnArchiveNotInTheListFails(t *testing.T) {
	r := newRelease(t)
	err := Verify(r.pub, r.archive, r.sums, r.sig, "kipp_9.9.9_linux_amd64.tar.gz")
	if !errors.Is(err, ErrNotListed) {
		t.Fatalf("got %v", err)
	}
	// A name that only ends like a listed one is not listed.
	if err := Verify(r.pub, r.archive, r.sums, r.sig, "linux_amd64.tar.gz"); !errors.Is(err, ErrNotListed) {
		t.Fatalf("suffix taken: %v", err)
	}
}

func TestAModifiedListFailsTheSignature(t *testing.T) {
	r := newRelease(t)
	r.sums = append(r.sums, "00  extra\n"...)
	err := r.verify()
	if !errors.Is(err, ErrSignature) || errors.Is(err, ErrChecksum) || !strings.Contains(err.Error(), "signed") {
		t.Fatalf("got %v", err)
	}
}

func TestASignatureByAnotherKeyFails(t *testing.T) {
	r := newRelease(t)
	_, other, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	r.sig = other.Sign(r.sums, "x")
	if err := r.verify(); !errors.Is(err, ErrSignature) {
		t.Fatalf("got %v", err)
	}
	// The same key id with another key does not pass either.
	forged := SecretKey{ID: r.secret.ID, Key: other.Key}
	r.sig = forged.Sign(r.sums, "x")
	if err := r.verify(); !errors.Is(err, ErrSignature) {
		t.Fatalf("got %v", err)
	}
}

func TestAnEmptyOrCutSignatureFails(t *testing.T) {
	r := newRelease(t)
	lines := strings.SplitAfter(string(r.sig), "\n")
	cuts := [][]byte{nil, []byte("\n"), []byte(lines[0]), []byte(lines[0] + lines[1]), []byte(strings.Join(lines[:3], "")), r.sig[:len(r.sig)/2], []byte(lines[0] + "AAAA\n" + lines[2] + lines[3])}
	for i, cut := range cuts {
		r.sig = cut
		if err := r.verify(); !errors.Is(err, ErrSignature) {
			t.Errorf("cut %d: got %v", i, err)
		}
	}
}

func TestATamperedTrustedCommentFails(t *testing.T) {
	r := newRelease(t)
	r.sig = bytes.Replace(r.sig, []byte("file:SHA256SUMS"), []byte("file:other"), 1)
	if err := r.verify(); !errors.Is(err, ErrSignature) {
		t.Fatalf("got %v", err)
	}
}

func TestABadPublicKeyIsNotASignatureFailure(t *testing.T) {
	r := newRelease(t)
	for _, pub := range []string{"", "not base64!", "RWQ=\n", "# only a comment\n"} {
		err := Verify([]byte(pub), r.archive, r.sums, r.sig, archiveName)
		if err == nil || errors.Is(err, ErrSignature) {
			t.Errorf("%q: got %v", pub, err)
		}
	}
}

func TestEveryLineForTheArchiveMustMatch(t *testing.T) {
	r := newRelease(t)
	bad := sha256.Sum256([]byte("x"))
	r.sums = append(r.sums, (hex.EncodeToString(bad[:]) + " *" + archiveName + "\r\n")...)
	r.sig = r.secret.Sign(r.sums, "x")
	if err := r.verify(); !errors.Is(err, ErrChecksum) {
		t.Fatalf("got %v", err)
	}
}

func TestAPrehashedSignatureOfTheMinisignToolVerifies(t *testing.T) {
	// The form newer minisign makes by default: ed25519 over the BLAKE2b-512
	// of the message.
	pub, sec, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("hello\n")
	sig := sec.signHashed(msg, "t")
	if err := pub.VerifySignature(msg, sig); err != nil {
		t.Fatal(err)
	}
	if err := pub.VerifySignature([]byte("hello"), sig); err == nil {
		t.Fatal("another message verified")
	}
}

func TestKeyFilesRoundTripAndAreMinisignShaped(t *testing.T) {
	pub, sec, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParsePublicKey(append([]byte("# a comment\n"), pub.Marshal()...))
	if err != nil || p.ID != pub.ID || !bytes.Equal(p.Key, pub.Key) {
		t.Fatalf("public key: %v", err)
	}
	s, err := ParseSecretKey(sec.Marshal())
	if err != nil || s.ID != sec.ID || !bytes.Equal(s.Key, sec.Key) {
		t.Fatalf("secret key: %v", err)
	}
	damaged := bytes.Replace(sec.Marshal(), []byte("untrusted comment: minisign secret key\n"), []byte("untrusted comment: x\nAAAA\n"), 1)
	if _, err := ParseSecretKey(damaged); err == nil {
		t.Fatal("two key lines taken")
	}
	text := sec.Marshal()
	text[len(text)-5] ^= 1 // inside the checksum
	if _, err := ParseSecretKey(text); err == nil {
		t.Fatal("a damaged secret key taken")
	}
}

func TestTheKeyInTheRepositoryParses(t *testing.T) {
	if _, err := ParsePublicKey(ReleaseKey()); err != nil {
		t.Fatal(err)
	}
}

// The test hooks exist only under the kipptest tag: neither binary a user
// gets may hold the name of one.
func TestNoTestHookIsInANormalBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two binaries")
	}
	dir := t.TempDir()
	for _, cmd := range []string{"kipp", "kipp-release"} {
		out := filepath.Join(dir, cmd)
		build := exec.Command("go", "build", "-o", out, "../../cmd/"+cmd)
		if b, err := build.CombinedOutput(); err != nil {
			t.Fatalf("go build %s: %v\n%s", cmd, err, b)
		}
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("KIPP_TEST_")) {
			t.Errorf("%s holds a KIPP_TEST_ name", cmd)
		}
	}
}
