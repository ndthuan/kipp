package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// What a stage is given to reach the cache, and what the server calls it.
const (
	// URLEnv is the server's address as the stage reaches it.
	URLEnv = "KIPP_CACHE_URL"
	// TokenEnv is the build's token.
	TokenEnv = "KIPP_CACHE_TOKEN"
	// ScopeEnv is the scope a command without --scope uses: the first of
	// the stage's cache_scopes.
	ScopeEnv = "KIPP_CACHE_SCOPE"
	// TokenPrefix is what a cache token starts with, so that one found in a
	// log is known for what it is.
	TokenPrefix = "kippc_"
	// Prefix is where the server serves the cache: /api/v1/cache/<scope>/...
	Prefix = "/api/v1/cache/"
	// EntryMax is the most an entry may come to, compressed.
	EntryMax = 10 << 30
)

var keyRe = regexp.MustCompile(`^[A-Za-z0-9._+=-]{1,512}$`)

// ValidKey reports whether s may be a cache key: it is a file name on the
// server and a line on the Cache page.
func ValidKey(s string) bool { return keyRe.MatchString(s) }

// Found is the server's answer to a lookup: the entry to fetch, and whether
// its key is the one asked for or the newest that starts with a prefix.
type Found struct {
	ID    int64  `json:"id"`
	Key   string `json:"key"`
	Exact bool   `json:"exact"`
	Size  int64  `json:"size"`
}

// errMiss is a lookup that found nothing, which is no failure.
var errMiss = errors.New("no entry")

// client talks to the server's cache endpoints with a build's token.
type client struct {
	base, token, scope string
	http               *http.Client
}

func newClient(getenv func(string) string, scope string, hc *http.Client) (*client, error) {
	base, token := strings.TrimRight(getenv(URLEnv), "/"), getenv(TokenEnv)
	if base == "" || token == "" {
		return nil, errors.New("this stage has no cache: its job declares no cache_scopes")
	}
	if scope == "" {
		scope = getenv(ScopeEnv)
	}
	if scope == "" {
		return nil, errors.New("no scope: pass --scope, or declare cache_scopes in the job")
	}
	if hc == nil {
		// No overall timeout: an entry is up to 10 GiB. A server that does
		// not answer is caught by the dial and by the lookup's own deadline.
		hc = &http.Client{Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			DialContext:         (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
			TLSHandshakeTimeout: 15 * time.Second,
		}}
	}
	return &client{base: base, token: token, scope: scope, http: hc}, nil
}

func (c *client) request(ctx context.Context, method, path string, q url.Values, body io.Reader) (*http.Response, error) {
	u := c.base + Prefix + url.PathEscape(c.scope) + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	return c.http.Do(req)
}

// lookup asks for key, then for the newest entry of each prefix in order.
func (c *client) lookup(ctx context.Context, key string, prefixes []string) (*Found, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	q := url.Values{"key": {key}, "restore": prefixes}
	resp, err := c.request(ctx, http.MethodGet, "/lookup", q, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		var f Found
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&f); err != nil {
			return nil, fmt.Errorf("the server's answer is not a cache entry: %w", err)
		}
		return &f, nil
	case http.StatusNotFound:
		return nil, errMiss
	}
	return nil, statusError(resp)
}

// fetch opens the entry's archive; the caller closes it.
func (c *client) fetch(ctx context.Context, id int64) (io.ReadCloser, error) {
	resp, err := c.request(ctx, http.MethodGet, "/entry/"+strconv.FormatInt(id, 10), nil, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, statusError(resp)
	}
	return resp.Body, nil
}

// put sends an entry as it is made: body is read until it ends. It
// reports whether the server kept it, false when the key was already there.
func (c *client) put(ctx context.Context, key string, body io.Reader) (bool, error) {
	resp, err := c.request(ctx, http.MethodPut, "/"+url.PathEscape(key), nil, body)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusCreated:
		return true, nil
	case http.StatusOK:
		return false, nil
	}
	return false, statusError(resp)
}

// statusError is a refusal, with the reason the server gave: the detail of
// its problem, or the text of a reply that is not one.
func statusError(resp *http.Response) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var p struct {
		Detail string `json:"detail"`
	}
	if json.Unmarshal(msg, &p) == nil && p.Detail != "" {
		msg = []byte(p.Detail)
	}
	if m := strings.TrimSpace(string(msg)); m != "" {
		return fmt.Errorf("the server answered %d: %s", resp.StatusCode, m)
	}
	return fmt.Errorf("the server answered %d", resp.StatusCode)
}
