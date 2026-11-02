package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// What GitHub asks of the JWT an App signs in with: issued 60 seconds in the
// past, to forgive a clock that runs ahead, and no more than 10 minutes into
// the future. Nine keeps a margin for the clock that runs behind.
const (
	jwtBackdate = 60 * time.Second
	jwtLife     = 9 * time.Minute
	// An installation token lasts an hour; one with less than this left
	// is not handed to a scan that may outlast it.
	tokenMargin = 5 * time.Minute
	maxKeyFile  = 64 << 10
)

// App is a GitHub App kipp signs in as: it signs a JWT with the private
// key in a file and trades it for the installation token of an account,
// which it keeps until shortly before it expires. The key file is read
// every time a JWT is signed, so replacing it needs no restart, and it is
// refused when group or others can read it, as ssh refuses a key.
type App struct {
	id      int64
	keyFile string
	api     string
	http    *http.Client
	now     func() time.Time

	mu     sync.Mutex
	tokens map[string]installToken // by the account's lower-cased login
}

type installToken struct {
	value   string
	expires time.Time
}

// NewApp is the App with the ID, whose private key is in keyFile, calling
// the API at api (DefaultAPI when empty).
func NewApp(api string, id int64, keyFile string) *App {
	c := New(api, "")
	return &App{id: id, keyFile: keyFile, api: c.API, http: c.HTTP, now: time.Now, tokens: map[string]installToken{}}
}

// Client is a client for the account called login, with the installation
// token of the App there: a fresh one when the kept one is about to
// expire. Every call is as the installation, so a repo the App is not
// installed on is not reachable by it.
func (a *App) Client(ctx context.Context, login string) (*Client, error) {
	login = strings.ToLower(login)
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.tokens[login]
	if !ok || a.now().Add(tokenMargin).After(t.expires) {
		var err error
		if t, err = a.mint(ctx, login); err != nil {
			delete(a.tokens, login)
			return nil, err
		}
		a.tokens[login] = t
	}
	return &Client{API: a.api, Token: t.value, HTTP: a.http}, nil
}

// mint asks GitHub for a new installation token on the account.
func (a *App) mint(ctx context.Context, login string) (installToken, error) {
	jwt, err := a.JWT()
	if err != nil {
		return installToken{}, err
	}
	as := &Client{API: a.api, Token: jwt, HTTP: a.http}
	id, err := as.installation(ctx, login, a.id)
	if err != nil {
		return installToken{}, err
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	call := "POST /app/installations/" + strconv.FormatInt(id, 10) + "/access_tokens"
	if _, _, err := as.fetch(ctx, http.MethodPost, call, as.API+strings.TrimPrefix(call, "POST "), nil, &out); err != nil {
		return installToken{}, err
	}
	if out.Token == "" || out.ExpiresAt.IsZero() {
		return installToken{}, &Error{Call: call, Msg: "the answer holds no token or no expiry"}
	}
	return installToken{value: out.Token, expires: out.ExpiresAt}, nil
}

// installation is the ID of the App's installation on the account.
func (c *Client) installation(ctx context.Context, login string, app int64) (int64, error) {
	var found int64
	var suspended bool
	err := c.pages(ctx, "/app/installations", nil, func(raw json.RawMessage) error {
		var page []struct {
			ID      int64 `json:"id"`
			Account struct {
				Login string `json:"login"`
			} `json:"account"`
			SuspendedAt *time.Time `json:"suspended_at"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		for _, in := range page {
			if strings.EqualFold(in.Account.Login, login) {
				found, suspended = in.ID, in.SuspendedAt != nil
			}
		}
		return nil
	})
	switch {
	case err != nil:
		return 0, err
	case found == 0:
		return 0, fmt.Errorf("github: the App %d is not installed on %s", app, login)
	case suspended:
		return 0, fmt.Errorf("github: the App %d is suspended on %s", app, login)
	}
	return found, nil
}

// JWT is a token that says the call is the App's own, for the length
// GitHub allows. It is signed with the key file as it is now.
func (a *App) JWT() (string, error) {
	key, err := readKey(a.keyFile)
	if err != nil {
		return "", err
	}
	now := a.now()
	head, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{
		"iat": now.Add(-jwtBackdate).Unix(), "exp": now.Add(jwtLife).Unix(), "iss": strconv.FormatInt(a.id, 10),
	})
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(head) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("the App's key file %s could not sign: %w", a.keyFile, err)
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// CheckKeyFile says why the file is not a key an App can sign with, or
// nil; like every error here it names the file and never what is in it.
func CheckKeyFile(path string) error {
	_, err := readKey(path)
	return err
}

// readKey reads the RSA private key in the PEM file. The file is looked at
// through the descriptor it is read from, so that what was checked is what
// is read; an error names the file and never what is in it.
func readKey(path string) (*rsa.PrivateKey, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("the App's key file %s: %w", path, errors.Unwrap(err))
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	switch {
	case err != nil:
		return nil, fmt.Errorf("the App's key file %s: %w", path, err)
	case !fi.Mode().IsRegular():
		return nil, fmt.Errorf("the App's key file %s is not a file", path)
	case runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0:
		return nil, fmt.Errorf("the App's key file %s is readable by group or others (mode %04o): chmod 600 it", path, fi.Mode().Perm())
	}
	data, err := io.ReadAll(io.LimitReader(f, maxKeyFile))
	if err != nil {
		return nil, fmt.Errorf("the App's key file %s: %w", path, err)
	}
	for {
		var block *pem.Block
		if block, data = pem.Decode(data); block == nil {
			return nil, fmt.Errorf("the App's key file %s holds no private key in PEM", path)
		}
		if key := parseKey(block); key != nil {
			return key, nil
		}
	}
}

// parseKey is the RSA key a PEM block holds, nil if it holds none.
func parseKey(b *pem.Block) *rsa.PrivateKey {
	if k, err := x509.ParsePKCS1PrivateKey(b.Bytes); err == nil {
		return k
	}
	if k, err := x509.ParsePKCS8PrivateKey(b.Bytes); err == nil {
		if rk, ok := k.(*rsa.PrivateKey); ok {
			return rk
		}
	}
	return nil
}

// Apps are the Apps a server signs in as, one for each ID and key file, so
// that the installation tokens one holds are shared by every job that
// names it.
type Apps struct {
	api  string
	mu   sync.Mutex
	apps map[string]*App
}

// NewApps is a set of Apps that call the API at api.
func NewApps(api string) *Apps { return &Apps{api: api, apps: map[string]*App{}} }

// Get is the App with the ID and the key file.
func (s *Apps) Get(id int64, keyFile string) *App {
	k := strconv.FormatInt(id, 10) + "\x00" + keyFile
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.apps[k]
	if a == nil {
		a = NewApp(s.api, id, keyFile)
		s.apps[k] = a
	}
	return a
}
