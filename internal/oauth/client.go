package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ndthuan/kipp/internal/store"
)

// Client is an app that may ask for access.
type Client struct {
	ID           string
	Name         string
	RedirectURIs []string
	CIMD         bool
}

const (
	maxDocument     = 64 << 10
	maxRedirects    = 10
	maxURILen       = 2048
	maxNameRunes    = 100
	minCache        = time.Hour
	maxCache        = 24 * time.Hour
	maxCachedClient = 256
)

func loopbackURI(u *url.URL) bool {
	return u.Scheme == "http" && isLoopbackHost(u.Hostname())
}

func parseRedirect(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil || raw == "" || len(raw) > maxURILen || strings.ContainsFunc(raw, unicode.IsControl) || strings.ContainsRune(raw, ' '):
		return nil, errors.New("not a URL")
	case u.Host == "" || u.User != nil || u.Fragment != "" || strings.Contains(raw, "#"):
		return nil, errors.New("needs a host, and no credentials or fragment")
	case u.Scheme == "https", loopbackURI(u):
		return u, nil
	}
	return nil, errors.New("must be https, or http on localhost or 127.0.0.1")
}

func redirectAllowed(registered []string, given string) bool {
	if slices.Contains(registered, given) {
		return true
	}
	g, err := parseRedirect(given)
	if err != nil || !loopbackURI(g) {
		return false
	}
	for _, r := range registered {
		u, err := url.Parse(r)
		if err == nil && loopbackURI(u) && u.Hostname() == g.Hostname() && u.EscapedPath() == g.EscapedPath() && u.RawQuery == g.RawQuery {
			return true
		}
	}
	return false
}

func clientName(name, fallback string) string {
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, name))
	if r := []rune(name); len(r) > maxNameRunes {
		name = string(r[:maxNameRunes])
	}
	if name == "" {
		return fallback
	}
	return name
}

func (s *Server) client(ctx context.Context, id string) (*Client, *Error) {
	switch {
	case id == "":
		return nil, badRequest("invalid_request", "client_id is missing")
	case strings.HasPrefix(id, "https://"):
		return s.fetchClient(ctx, id)
	case strings.Contains(id, "://"):
		return nil, fail(http.StatusUnauthorized, "invalid_client", "a client_id that is a URL must be https")
	}
	c, err := s.cfg.Store.OAuthClient(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, fail(http.StatusUnauthorized, "invalid_client", "unknown client_id")
	case err != nil:
		s.cfg.Logger.Error("oauth: reading a client", "err", err)
		return nil, fail(http.StatusInternalServerError, "server_error", "internal error")
	}
	return &Client{ID: c.ID, Name: c.Name, RedirectURIs: c.RedirectURIs}, nil
}

type cimdCache struct {
	mu sync.Mutex
	m  map[string]cached
}

type cached struct {
	c       *Client
	expires time.Time
}

func newCIMDCache() *cimdCache { return &cimdCache{m: map[string]cached{}} }

func (c *cimdCache) get(id string, now time.Time) *Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[id]
	if !ok || !now.Before(e.expires) {
		delete(c.m, id)
		return nil
	}
	return e.c
}

func (c *cimdCache) put(cl *Client, ttl time.Duration, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= maxCachedClient {
		for k, e := range c.m {
			if !now.Before(e.expires) {
				delete(c.m, k)
			}
		}
		for k := range c.m {
			if len(c.m) < maxCachedClient {
				break
			}
			delete(c.m, k)
		}
	}
	c.m[cl.ID] = cached{cl, now.Add(ttl)}
}

func cacheFor(h http.Header) time.Duration {
	ttl := minCache
	for d := range strings.SplitSeq(h.Get("Cache-Control"), ",") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(strings.ToLower(d)), "max-age="); ok {
			if n, err := strconv.Atoi(v); err == nil {
				ttl = time.Duration(n) * time.Second
			}
		}
	}
	return min(max(ttl, minCache), maxCache)
}

func validDocumentURL(id string) (*url.URL, error) {
	u, err := url.Parse(id)
	switch {
	case err != nil || len(id) > maxURILen || strings.ContainsFunc(id, unicode.IsControl):
		return nil, errors.New("not a URL")
	case u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || strings.Contains(id, "#"):
		return nil, errors.New("must be an https URL with a host, and no credentials or fragment")
	case u.Path == "" || u.Path == "/":
		return nil, errors.New("must have a path")
	case slices.Contains(strings.Split(u.EscapedPath(), "/"), ".") || slices.Contains(strings.Split(u.EscapedPath(), "/"), ".."):
		return nil, errors.New("must not have dot segments")
	}
	return u, nil
}

type document struct {
	ClientID     string   `json:"client_id"`
	ClientName   string   `json:"client_name"`
	RedirectURIs []string `json:"redirect_uris"`
}

func (s *Server) fetchClient(ctx context.Context, id string) (*Client, *Error) {
	now := s.cfg.Now()
	if c := s.cimd.get(id, now); c != nil {
		return c, nil
	}
	u, err := validDocumentURL(id)
	if err != nil {
		return nil, fail(http.StatusUnauthorized, "invalid_client", "client_id: %v", err)
	}
	if src, ok := ctx.Value(sourceKey{}).(string); ok {
		if _, ok := s.limit.Reserve("fetch\x00" + src); !ok {
			return nil, fail(http.StatusTooManyRequests, "temporarily_unavailable", "too many new clients from this address; try again later")
		}
	}
	doc, ttl, err := s.getDocument(ctx, u)
	if err != nil {
		s.cfg.Logger.Warn("oauth: a client's metadata document was refused", "client_id", id, "err", err)
		return nil, fail(http.StatusUnauthorized, "invalid_client", "the client's metadata document could not be used: %v", docProblem(err))
	}
	c := &Client{ID: id, CIMD: true, Name: clientName(doc.ClientName, u.Hostname()), RedirectURIs: doc.RedirectURIs}
	created, err := s.cfg.Store.SaveOAuthClient(ctx, &store.OAuthClient{ID: c.ID, Name: c.Name, CIMD: true, RedirectURIs: c.RedirectURIs}, now)
	if err != nil {
		s.cfg.Logger.Error("oauth: keeping a client", "err", err)
		return nil, fail(http.StatusInternalServerError, "server_error", "internal error")
	}
	if created {
		s.audit(ctx, "oauth", "oauth.client.register", c.Name, "client_id "+c.ID+", by its metadata document")
	}
	s.cimd.put(c, ttl, now)
	return c, nil
}

type sourceKey struct{}

// WithSource is ctx marked with where r comes from, so that a client
// document kipp is made to fetch for it is counted against that source:
// anybody may name any public https URL as a client_id, and an unmarked
// context (a token request, whose code was already made through a marked
// one) is not counted.
func (s *Server) WithSource(ctx context.Context, r *http.Request) context.Context {
	return context.WithValue(ctx, sourceKey{}, s.cfg.Client(r))
}

func docProblem(err error) string {
	if p, ok := errors.AsType[problem](err); ok {
		return string(p)
	}
	return "it could not be fetched"
}

type problem string

func (p problem) Error() string { return string(p) }

func (s *Server) getDocument(ctx context.Context, u *url.URL) (*document, time.Duration, error) {
	hc := *s.cfg.Fetch
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || len(via) > 3 {
			return errors.New("a redirect to anything but https, or too many")
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	res, err := hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, 0, problem(fmt.Sprintf("it answered %d", res.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxDocument+1))
	switch {
	case err != nil:
		return nil, 0, err
	case len(body) > maxDocument:
		return nil, 0, problem("it is larger than 64 KiB")
	}
	var doc document
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, 0, problem("it is not a JSON object")
	}
	if doc.ClientID != u.String() {
		return nil, 0, problem("its client_id is not its own URL")
	}
	if err := checkRedirects(doc.RedirectURIs); err != nil {
		return nil, 0, problem(err.Error())
	}
	return &doc, cacheFor(res.Header), nil
}

func checkRedirects(uris []string) error {
	if len(uris) == 0 || len(uris) > maxRedirects {
		return fmt.Errorf("redirect_uris holds 1 to %d addresses", maxRedirects)
	}
	for _, r := range uris {
		if _, err := parseRedirect(r); err != nil {
			return fmt.Errorf("redirect_uris: %w", err)
		}
	}
	return nil
}
