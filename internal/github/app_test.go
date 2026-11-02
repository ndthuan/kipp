package github

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var (
	keyOnce sync.Once
	keys    [2]*rsa.PrivateKey
)

// testKey is one of two RSA keys, made once for all the tests.
func testKey(t *testing.T, i int) *rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() {
		for n := range keys {
			k, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				panic(err)
			}
			keys[n] = k
		}
	})
	return keys[i]
}

func pkcs1(k *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

func writeKey(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // the umask does not decide the mode under test
		t.Fatal(err)
	}
}

// verifyJWT checks the signature with the key and returns the claims.
func verifyJWT(t *testing.T, token string, pub *rsa.PublicKey) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", token)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		return nil
	}
	head, _ := base64.RawURLEncoding.DecodeString(parts[0])
	if string(head) != `{"alg":"RS256","typ":"JWT"}` {
		t.Fatalf("header %s", head)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

var t0 = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

// clock is a time that moves when a test says.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeApp is GitHub as an App sees it: it knows one key, lists its
// installations and mints tokens.
type fakeApp struct {
	t     *testing.T
	pub   *rsa.PublicKey
	clk   *clock
	mints atomic.Int32
	// installs are the installations by account login; suspended ones are
	// listed too.
	installs  map[string]int64
	suspended map[string]bool
	seen      []string
}

func (f *fakeApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	claims := verifyJWT(f.t, bearer, f.pub)
	if claims == nil {
		http.Error(w, `{"message":"A JSON web token could not be decoded"}`, http.StatusUnauthorized)
		return
	}
	f.seen = append(f.seen, bearer)
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
		var out []map[string]any
		for login, id := range f.installs {
			in := map[string]any{"id": id, "account": map[string]string{"login": login}}
			if f.suspended[login] {
				in["suspended_at"] = "2026-10-01T00:00:00Z"
			}
			out = append(out, in)
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app/installations/") && strings.HasSuffix(r.URL.Path, "/access_tokens"):
		n := f.mints.Add(1)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": fmt.Sprintf("ghs_install_%d_%s", n, strings.Split(r.URL.Path, "/")[3]), "expires_at": f.clk.now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	default:
		http.NotFound(w, r)
	}
}

func newFakeApp(t *testing.T, mode os.FileMode) (*App, *fakeApp, string) {
	t.Helper()
	clk := &clock{t: t0}
	fake := &fakeApp{t: t, pub: &testKey(t, 0).PublicKey, clk: clk, installs: map[string]int64{"acme": 77, "other": 88}, suspended: map[string]bool{}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	path := filepath.Join(t.TempDir(), "acme.pem")
	writeKey(t, path, pkcs1(testKey(t, 0)), mode)
	app := NewApp(srv.URL, 123456, path)
	app.now = clk.now
	return app, fake, path
}

func TestAnAppSignsAJWTGitHubCanVerify(t *testing.T) {
	app, _, _ := newFakeApp(t, 0o600)
	jwt, err := app.JWT()
	if err != nil {
		t.Fatal(err)
	}
	claims := verifyJWT(t, jwt, &testKey(t, 0).PublicKey)
	if claims == nil {
		t.Fatal("the signature does not verify with the public key")
	}
	iat, exp := int64(claims["iat"].(float64)), int64(claims["exp"].(float64))
	if iat != t0.Add(-60*time.Second).Unix() || exp != t0.Add(9*time.Minute).Unix() || claims["iss"] != "123456" {
		t.Fatalf("claims %v", claims)
	}
	if exp-t0.Unix() >= 600 || exp-iat > 600 {
		t.Fatalf("GitHub takes a JWT of 10 minutes at most: iat %d exp %d", iat, exp)
	}
	if verifyJWT(t, jwt, &testKey(t, 1).PublicKey) != nil {
		t.Fatal("another key verified it")
	}
}

func TestTheKeyFileIsRefusedWhenGroupOrOthersCanReadIt(t *testing.T) {
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o660, 0o666, 0o601} {
		app, _, path := newFakeApp(t, mode)
		_, err := app.JWT()
		if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "readable by group or others") {
			t.Errorf("mode %04o: error %v", mode, err)
		}
	}
	for _, mode := range []os.FileMode{0o600, 0o400} {
		app, _, _ := newFakeApp(t, mode)
		if _, err := app.JWT(); err != nil {
			t.Errorf("mode %04o: %v", mode, err)
		}
	}
}

func TestReplacingTheKeyFileChangesTheNextJWTWithoutARestart(t *testing.T) {
	app, _, path := newFakeApp(t, 0o600)
	first, err := app.JWT()
	if err != nil {
		t.Fatal(err)
	}
	if verifyJWT(t, first, &testKey(t, 0).PublicKey) == nil {
		t.Fatal("first JWT does not verify with the first key")
	}
	writeKey(t, path, pkcs1(testKey(t, 1)), 0o600)
	second, err := app.JWT()
	if err != nil {
		t.Fatal(err)
	}
	if verifyJWT(t, second, &testKey(t, 1).PublicKey) == nil || verifyJWT(t, second, &testKey(t, 0).PublicKey) != nil {
		t.Fatal("the next JWT is not signed with the new key")
	}
}

func TestTheKeyFileMayHoldPKCS8AndNothingElse(t *testing.T) {
	app, _, path := newFakeApp(t, 0o600)
	der, err := x509.MarshalPKCS8PrivateKey(testKey(t, 0))
	if err != nil {
		t.Fatal(err)
	}
	writeKey(t, path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)
	if _, err := app.JWT(); err != nil {
		t.Fatalf("PKCS8: %v", err)
	}
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecDER, _ := x509.MarshalPKCS8PrivateKey(ec)
	for name, body := range map[string][]byte{
		"empty":     nil,
		"garbage":   []byte("not a key, but secret-looking-text-0123"),
		"elliptic":  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER}),
		"a cert":    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("x")}),
		"truncated": pkcs1(testKey(t, 0))[:100],
	} {
		writeKey(t, path, body, 0o600)
		_, err := app.JWT()
		if err == nil || !strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "secret-looking-text") {
			t.Errorf("%s: error %v", name, err)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := app.JWT(); err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("missing file: error %v", err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := app.JWT(); err == nil || !strings.Contains(err.Error(), "not a file") {
		t.Errorf("directory: error %v", err)
	}
}

func TestAnInstallationTokenIsKeptUntilNearItsExpiryThenRenewed(t *testing.T) {
	app, fake, _ := newFakeApp(t, 0o600)
	clk := fake.clk
	ctx := context.Background()
	c1, err := app.Client(ctx, "acme")
	if err != nil || c1.Token != "ghs_install_1_77" {
		t.Fatalf("%v %+v", err, c1)
	}
	clk.add(54 * time.Minute) // 6 minutes are left: more than the margin
	c2, err := app.Client(ctx, "ACME")
	if err != nil || c2.Token != c1.Token || fake.mints.Load() != 1 {
		t.Fatalf("a token with time left is kept: %v %+v, %d mints", err, c2, fake.mints.Load())
	}
	clk.add(2 * time.Minute) // 4 minutes are left: renewed
	c3, err := app.Client(ctx, "acme")
	if err != nil || c3.Token != "ghs_install_2_77" || fake.mints.Load() != 2 {
		t.Fatalf("a token about to expire is renewed: %v %+v, %d mints", err, c3, fake.mints.Load())
	}
	other, err := app.Client(ctx, "other")
	if err != nil || other.Token != "ghs_install_3_88" {
		t.Fatalf("another account has a token of its own: %v %+v", err, other)
	}
	for _, jwt := range fake.seen {
		if strings.HasPrefix(jwt, "ghs_") {
			t.Fatalf("the App's own calls carry a JWT: %q", jwt)
		}
	}
}

func TestManyCallsAtOnceMintOneToken(t *testing.T) {
	app, fake, _ := newFakeApp(t, 0o600)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if _, err := app.Client(context.Background(), "acme"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := fake.mints.Load(); n != 1 {
		t.Fatalf("%d tokens minted for one account", n)
	}
}

func TestAnAppNotInstalledOrSuspendedOnAnAccountSaysSo(t *testing.T) {
	app, fake, _ := newFakeApp(t, 0o600)
	fake.suspended["other"] = true
	if _, err := app.Client(context.Background(), "nobody"); err == nil || !strings.Contains(err.Error(), "not installed on nobody") {
		t.Errorf("not installed: %v", err)
	}
	if _, err := app.Client(context.Background(), "other"); err == nil || !strings.Contains(err.Error(), "suspended on other") {
		t.Errorf("suspended: %v", err)
	}
	if fake.mints.Load() != 0 {
		t.Fatal("a token was minted")
	}
}

func TestAnAppWhoseKeyGitHubDoesNotKnowIsRefusedWithoutTheJWTInTheError(t *testing.T) {
	app, _, path := newFakeApp(t, 0o600)
	writeKey(t, path, pkcs1(testKey(t, 1)), 0o600) // not the key the fake knows
	jwt, err := app.JWT()
	if err != nil {
		t.Fatal(err)
	}
	_, err = app.Client(context.Background(), "acme")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("error %v", err)
	}
	if strings.Contains(err.Error(), jwt) || strings.Contains(err.Error(), "BEGIN") {
		t.Fatalf("the error holds a credential: %v", err)
	}
}

func TestAppsGiveOneAppForAnIDAndAKeyFile(t *testing.T) {
	s := NewApps("")
	a, b := s.Get(1, "/k.pem"), s.Get(1, "/k.pem")
	if a != b || s.Get(2, "/k.pem") == a || s.Get(1, "/other.pem") == a {
		t.Fatal("an App is one for its ID and key file")
	}
}

func TestCheckKeyFileSaysWhatIsWrongWithAKeyWithoutItsContent(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.pem")
	writeKey(t, good, pkcs1(testKey(t, 0)), 0o600)
	if err := CheckKeyFile(good); err != nil {
		t.Fatal(err)
	}
	loose := filepath.Join(dir, "loose.pem")
	writeKey(t, loose, pkcs1(testKey(t, 0)), 0o644)
	junk := filepath.Join(dir, "junk.pem")
	writeKey(t, junk, []byte("THE-SECRET-BODY"), 0o600)
	for path, says := range map[string]string{loose: "readable by group or others", junk: "no private key in PEM", filepath.Join(dir, "none.pem"): "none.pem"} {
		err := CheckKeyFile(path)
		if err == nil || !strings.Contains(err.Error(), says) || strings.Contains(err.Error(), "THE-SECRET-BODY") {
			t.Errorf("%s: %v", path, err)
		}
	}
}
