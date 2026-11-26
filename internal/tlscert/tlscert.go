// Package tlscert makes and keeps the self-signed certificate kipp serve
// answers --tls-listen with when no certificate is given.
package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"time"
)

// CertFile and KeyFile are the names of the certificate and its key in the
// directory SelfSigned keeps them in.
const (
	CertFile = "cert.pem"
	KeyFile  = "key.pem"
)

const (
	lifetime    = 365 * 24 * time.Hour
	renewBefore = 30 * 24 * time.Hour
)

// SelfSigned is the certificate kept in dir for names, made anew when there
// is none, it does not parse, it ends within 30 days, or it misses a name.
func SelfSigned(dir string, names []string, now time.Time) (tls.Certificate, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return tls.Certificate{}, err
	}
	defer func() { _ = root.Close() }()
	if c, ok := kept(root, names, now); ok {
		return c, nil
	}
	certPEM, keyPEM, err := generate(names, now)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := root.WriteFile(KeyFile, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	if err := root.WriteFile(CertFile, certPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

func kept(root *os.Root, names []string, now time.Time) (tls.Certificate, bool) {
	certPEM, err := root.ReadFile(CertFile)
	if err != nil {
		return tls.Certificate{}, false
	}
	keyPEM, err := root.ReadFile(KeyFile)
	if err != nil {
		return tls.Certificate{}, false
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, false
	}
	x, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil || now.Add(renewBefore).After(x.NotAfter) {
		return tls.Certificate{}, false
	}
	for _, n := range names {
		if x.VerifyHostname(n) != nil {
			return tls.Certificate{}, false
		}
	}
	return c, true
}

func generate(names []string, now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "kipp"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(lifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, n)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), nil
}
