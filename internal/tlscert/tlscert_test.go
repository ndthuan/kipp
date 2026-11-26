package tlscert

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var names = []string{"localhost", "127.0.0.1", "::1"}

func leaf(t *testing.T, dir string, now time.Time, names []string) *x509.Certificate {
	t.Helper()
	c, err := SelfSigned(dir, names, now)
	if err != nil {
		t.Fatal(err)
	}
	x, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func TestASelfSignedCertificateCoversEveryNameAndItsKeyIsTheOwnersAlone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	now := time.Now()
	x := leaf(t, dir, now, names)
	for _, n := range names {
		if err := x.VerifyHostname(n); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
	if x.NotAfter.Before(now.Add(300*24*time.Hour)) || x.IsCA {
		t.Fatalf("lasts until %s, CA %v", x.NotAfter, x.IsCA)
	}
	for name, want := range map[string]os.FileMode{KeyFile: 0o600, CertFile: 0o600, ".": 0o700} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s has mode %v, want %v", name, fi.Mode().Perm(), want)
		}
	}
}

func TestTheSameCertificateIsKeptUntilItNearsItsEndOrMissesAName(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	first := leaf(t, dir, now, names)
	if again := leaf(t, dir, now.Add(24*time.Hour), names); !again.Equal(first) {
		t.Fatal("a certificate that still serves was made anew")
	}
	if renewed := leaf(t, dir, first.NotAfter.Add(-29*24*time.Hour), names); renewed.Equal(first) {
		t.Fatal("a certificate in its last month was kept")
	}
	wider := append(append([]string{}, names...), "ci.example.com")
	x := leaf(t, dir, now, wider)
	if err := x.VerifyHostname("ci.example.com"); err != nil {
		t.Fatalf("a name the certificate missed was not added: %v", err)
	}
}

func TestAKeptCertificateThatDoesNotParseIsMadeAnew(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, CertFile), []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	leaf(t, dir, time.Now(), names)
}
