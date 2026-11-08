package releasetool

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type result struct {
	code        int
	out, errOut string
}

func run(t *testing.T, env map[string]string, pub []byte, args ...string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(args, &out, &errOut, func(k string) string { return env[k] }, pub)
	return result{code, out.String(), errOut.String()}
}

// newKeys makes a key pair in dir and returns the paths.
func newKeys(t *testing.T, dir string) (secret, pub string) {
	t.Helper()
	secret, pub = filepath.Join(dir, "release.key"), filepath.Join(dir, "release.pub")
	if r := run(t, nil, nil, "keygen", "-secret", secret, "-pub", pub); r.code != 0 {
		t.Fatalf("keygen: %+v", r)
	}
	return secret, pub
}

func TestKeygenWritesTheSecretKeyAtMode0600AndPrintsNothingSecret(t *testing.T) {
	dir := t.TempDir()
	r := run(t, nil, nil, "keygen", "-secret", filepath.Join(dir, "release.key"), "-pub", filepath.Join(dir, "release.pub"))
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	info, err := os.Stat(filepath.Join(dir, "release.key"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode().Perm())
	}
	secret, err := os.ReadFile(filepath.Join(dir, "release.key"))
	if err != nil {
		t.Fatal(err)
	}
	pub, err := os.ReadFile(filepath.Join(dir, "release.pub"))
	if err != nil {
		t.Fatal(err)
	}
	b64 := strings.Split(strings.TrimSpace(string(secret)), "\n")[1]
	for _, leak := range []string{b64, string(secret)} {
		if strings.Contains(r.out+r.errOut, leak) {
			t.Error("the secret key was printed")
		}
	}
	if bytes.Contains(pub, []byte(b64)) {
		t.Error("the secret key is in the public key file")
	}
}

func TestKeygenNeverReplacesAKeyAndNeedsAPath(t *testing.T) {
	dir := t.TempDir()
	secret, pub := newKeys(t, dir)
	before, _ := os.ReadFile(secret)
	if r := run(t, nil, nil, "keygen", "-secret", secret, "-pub", pub); r.code == 0 {
		t.Error("an existing secret key was replaced")
	}
	if after, _ := os.ReadFile(secret); !bytes.Equal(before, after) {
		t.Error("the secret key changed")
	}
	if r := run(t, nil, nil, "keygen"); r.code == 0 {
		t.Error("keygen without -secret ran")
	}
}

func writeSums(t *testing.T, dir string) (sums, archive string) {
	t.Helper()
	data := []byte("an archive")
	sum := sha256.Sum256(data)
	archive = filepath.Join(dir, "kipp_1.0.0_linux_amd64.tar.gz")
	sums = filepath.Join(dir, "SHA256SUMS")
	if err := os.WriteFile(archive, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sums, []byte(hex.EncodeToString(sum[:])+"  kipp_1.0.0_linux_amd64.tar.gz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return sums, archive
}

func TestASignatureMadeBySignVerifiesWithVerifyFromAFileOrTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	secret, pubPath := newKeys(t, dir)
	sums, archive := writeSums(t, dir)
	pub, _ := os.ReadFile(pubPath)

	if r := run(t, nil, nil, "sign", "-sums", sums, "-key", secret); r.code != 0 {
		t.Fatalf("sign from file: %+v", r)
	}
	if r := run(t, nil, pub, "verify", "-sums", sums, "-archive", archive); r.code != 0 {
		t.Fatalf("verify: %+v", r)
	}

	key, _ := os.ReadFile(secret)
	if err := os.Remove(sums + ".sig"); err != nil {
		t.Fatal(err)
	}
	if r := run(t, map[string]string{"RELEASE_SIGNING_KEY": string(key)}, nil, "sign", "-sums", sums); r.code != 0 {
		t.Fatalf("sign from the environment: %+v", r)
	}
	if r := run(t, nil, nil, "verify", "-sums", sums, "-pub", pubPath); r.code != 0 || !strings.Contains(r.out, "signature ok") {
		t.Fatalf("verify without an archive: %+v", r)
	}
}

func TestVerifyNamesWhichCheckFailed(t *testing.T) {
	dir := t.TempDir()
	secret, pubPath := newKeys(t, dir)
	sums, archive := writeSums(t, dir)
	pub, _ := os.ReadFile(pubPath)
	run(t, nil, nil, "sign", "-sums", sums, "-key", secret)

	if err := os.WriteFile(archive, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := run(t, nil, pub, "verify", "-sums", sums, "-archive", archive); r.code != 1 || !strings.Contains(r.errOut, "checksum") {
		t.Errorf("tampered archive: %+v", r)
	}
	if err := os.WriteFile(sums, []byte("00  x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := run(t, nil, pub, "verify", "-sums", sums); r.code != 1 || !strings.Contains(r.errOut, "signature") {
		t.Errorf("tampered list: %+v", r)
	}
}

func TestSignNeedsAKeyAndTakesNoDamagedOne(t *testing.T) {
	dir := t.TempDir()
	sums, _ := writeSums(t, dir)
	if r := run(t, nil, nil, "sign", "-sums", sums); r.code == 0 || !strings.Contains(r.errOut, "RELEASE_SIGNING_KEY") {
		t.Errorf("no key: %+v", r)
	}
	if r := run(t, map[string]string{"RELEASE_SIGNING_KEY": "garbage"}, nil, "sign", "-sums", sums); r.code == 0 {
		t.Errorf("garbage key: %+v", r)
	}
	if _, err := os.Stat(sums + ".sig"); err == nil {
		t.Error("a signature was written without a key")
	}
}

func TestUnknownCommandsAndFlagsAreRefused(t *testing.T) {
	for _, args := range [][]string{nil, {"nope"}, {"sign", "-bogus"}, {"verify", "-sums", "/nonexistent"}} {
		if r := run(t, nil, nil, args...); r.code == 0 {
			t.Errorf("%v ran", args)
		}
	}
}
