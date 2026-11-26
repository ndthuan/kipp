// Package mcp serves kipp to an MCP client (a Claude chat, Claude Code) at
// /mcp, when kipp serve is given --mcp. A caller is a kipp user, found by
// the API token it sends, and every tool answers through the same
// permission-checked operations as the REST API, so what a caller sees and
// does is what that user's grants allow, and no more.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ndthuan/kipp/internal/api"
	kauth "github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/oauth"
	"github.com/ndthuan/kipp/internal/store"
)

// Path is where the server is mounted.
const Path = "/mcp"

// ErrInvalidToken is what a Verifier answers for a credential nobody holds:
// unknown, revoked or expired. Any other error is the server's fault.
var ErrInvalidToken = auth.ErrInvalidToken

// Ceiling is the most a credential may do, however much its user may: a
// tool acts only where the user's grant and the ceiling both allow. An API
// token acts as its user in full (AllGrants); a credential that is to do
// less, like an OAuth token granted read access only, carries a lower one.
//
// Jobs is the right to read and change job definitions. An OAuth grant has
// it only through kipp:jobs; kipp:build never carries it.
type Ceiling struct{ View, Build, Cancel, Jobs bool }

// AllGrants is the ceiling of a credential that acts as its user.
var AllGrants = Ceiling{View: true, Build: true, Cancel: true, Jobs: true}

// Allows reports whether the ceiling lets a credential do action (view,
// build or cancel).
func (c Ceiling) Allows(action string) bool {
	switch action {
	case "view":
		return c.View
	case "build":
		return c.Build
	case "cancel":
		return c.Cancel
	}
	return false
}

// Caller is who a request is made by: a kipp user, and the most the
// credential it came with may do for them.
type Caller struct {
	User    *store.User
	Ceiling Ceiling
}

// Verifier resolves the bearer token of a request into its caller. It
// answers ErrInvalidToken, wrapped or not, for a token nobody holds. A
// further way to authenticate is another Verifier that supplies its own
// Ceiling.
type Verifier func(ctx context.Context, token string) (*Caller, error)

// APITokens verifies kipp's own API tokens, the ones the REST API takes: the
// token's user, with every grant they have. The token's last use is
// recorded by the lookup, as it is for the API.
func APITokens(a *kauth.Service) Verifier {
	return func(ctx context.Context, token string) (*Caller, error) {
		u, err := a.UserFromToken(ctx, token)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return nil, ErrInvalidToken
		case err != nil:
			return nil, err
		}
		return &Caller{User: u, Ceiling: AllGrants}, nil
	}
}

// OAuthTokens adds the access tokens of an authorization server to a
// Verifier: a token it issued is the user of its grant, under a ceiling of
// what the grant's scopes allow (kipp:build is what lets a build be started
// or cancelled, kipp:jobs what lets jobs be read and changed, and reading
// is always granted), and any other token is
// left to next. The user's grants still apply beneath the ceiling.
func OAuthTokens(o *oauth.Server, next Verifier) Verifier {
	return func(ctx context.Context, token string) (*Caller, error) {
		if !strings.HasPrefix(token, oauth.AccessPrefix) {
			return next(ctx, token)
		}
		v, err := o.Verify(ctx, token)
		switch {
		case errors.Is(err, oauth.ErrInvalidToken):
			return nil, ErrInvalidToken
		case err != nil:
			return nil, err
		}
		build := v.Has(oauth.ScopeBuild)
		return &Caller{User: v.User, Ceiling: Ceiling{View: true, Build: build, Cancel: build, Jobs: v.Has(oauth.ScopeJobs)}}, nil
	}
}

// Config is what the server is given.
type Config struct {
	Service             *api.Service
	Auth                *kauth.Service
	Verify              Verifier
	Version             string
	ResourceMetadataURL string
	BaseURL             string
	Logger              *slog.Logger
}

// Server is the MCP endpoint.
type Server struct {
	cfg Config
	log *slog.Logger
}

// New makes the endpoint; Handler serves it.
func New(cfg Config) *Server {
	if cfg.Verify == nil {
		cfg.Verify = APITokens(cfg.Auth)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Server{cfg: cfg, log: cfg.Logger}
}

const callerKey = "kipp.caller"

// Handler is the streamable HTTP endpoint. It is stateless: every request
// stands alone and carries its token, so no session outlives a revoked
// token and nothing is kept between requests. A request without a good
// bearer token is 401 with a Bearer challenge; sessions and cookies are not
// read at all.
func (s *Server) Handler() http.Handler {
	servers := map[kind]*sdk.Server{plain: s.newServer(plain), jobsRead: s.newServer(jobsRead), jobsEdit: s.newServer(jobsEdit)}
	h := sdk.NewStreamableHTTPHandler(func(r *http.Request) *sdk.Server { return servers[s.kindFor(auth.TokenInfoFromContext(r.Context()))] }, &sdk.StreamableHTTPOptions{Stateless: true, DisableLocalhostProtection: true})
	var opts *auth.RequireBearerTokenOptions
	if s.cfg.ResourceMetadataURL != "" {
		opts = &auth.RequireBearerTokenOptions{ResourceMetadataURL: s.cfg.ResourceMetadataURL}
	}
	authed := auth.RequireBearerToken(s.verify, opts)(s.scopeGate(s.hostGuard(h)))
	return http.MaxBytesHandler(challenging(authed), api.MaxBody)
}

func (s *Server) hostGuard(next http.Handler) http.Handler {
	public := ""
	if u, err := url.Parse(s.cfg.BaseURL); err == nil {
		public = strings.ToLower(u.Hostname())
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok && local != nil && onLoopback(local.String()) {
			if h := hostName(r.Host); !onLoopback(h) && (public == "" || h != public) {
				http.Error(w, fmt.Sprintf("Forbidden: invalid Host header %q", r.Host), http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func hostName(hostport string) string {
	h, _, err := net.SplitHostPort(hostport)
	if err != nil {
		h = hostport
	}
	return strings.ToLower(strings.Trim(h, "[]"))
}

func onLoopback(hostport string) bool {
	h := hostName(hostport)
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) verify(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	c, err := s.cfg.Verify(ctx, token)
	switch {
	case errors.Is(err, ErrInvalidToken):
		return nil, err
	case err != nil:
		s.log.Error("mcp: verifying a token", "err", err)
		return nil, errors.New("internal error")
	}
	return &auth.TokenInfo{UserID: c.User.Name, Expiration: time.Now().Add(time.Minute), Extra: map[string]any{callerKey: c}}, nil
}

func callerOf(req *sdk.CallToolRequest) (*Caller, error) {
	if ex := req.GetExtra(); ex != nil && ex.TokenInfo != nil {
		if c, ok := ex.TokenInfo.Extra[callerKey].(*Caller); ok {
			return c, nil
		}
	}
	return nil, errors.New("not signed in")
}

var toolActions = map[string]string{"run_build": "build", "build_again": "build", "cancel_build": "cancel"}

func (s *Server) scopeGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := auth.TokenInfoFromContext(r.Context())
		if info == nil || r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		c, _ := info.Extra[callerKey].(*Caller)
		if c == nil || c.User == nil || c.Ceiling == AllGrants {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if missing := missingScopes(body, c); len(missing) > 0 {
			challenge := fmt.Sprintf("Bearer error=\"insufficient_scope\", scope=%q", scopesWith(c.Ceiling, missing))
			if s.cfg.ResourceMetadataURL != "" {
				challenge += fmt.Sprintf(", resource_metadata=%q", s.cfg.ResourceMetadataURL)
			}
			w.Header().Set("WWW-Authenticate", challenge)
			http.Error(w, "insufficient scope: this needs "+strings.Join(missing, " "), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func missingScopes(body []byte, c *Caller) []string {
	type call struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	var calls []call
	if err := json.Unmarshal(body, &calls); err != nil {
		var one call
		if json.Unmarshal(body, &one) != nil {
			return nil
		}
		calls = []call{one}
	}
	var build, jobs bool
	for _, cl := range calls {
		if cl.Method != "tools/call" {
			continue
		}
		if action, ok := toolActions[cl.Params.Name]; ok && !c.Ceiling.Allows(action) {
			build = true
		}
		if jobTools[cl.Params.Name] && c.User.Admin && !c.Ceiling.Jobs {
			jobs = true
		}
	}
	var missing []string
	if build {
		missing = append(missing, oauth.ScopeBuild)
	}
	if jobs {
		missing = append(missing, oauth.ScopeJobs)
	}
	return missing
}

func scopesWith(c Ceiling, missing []string) string {
	out := []string{oauth.ScopeRead}
	if c.Build || c.Cancel || slices.Contains(missing, oauth.ScopeBuild) {
		out = append(out, oauth.ScopeBuild)
	}
	if c.Jobs || slices.Contains(missing, oauth.ScopeJobs) {
		out = append(out, oauth.ScopeJobs)
	}
	return strings.Join(out, " ")
}

func challenging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&challenge{ResponseWriter: w}, r)
	})
}

type challenge struct{ http.ResponseWriter }

// WriteHeader adds the challenge to a 401 that lacks one.
func (c *challenge) WriteHeader(code int) {
	if code == http.StatusUnauthorized && c.Header().Get("WWW-Authenticate") == "" {
		c.Header().Set("WWW-Authenticate", "Bearer")
	}
	c.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the real writer.
func (c *challenge) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// Flush keeps a streamed answer streaming through the wrapper.
func (c *challenge) Flush() { _ = http.NewResponseController(c.ResponseWriter).Flush() }

func (s *Server) refusal(err error) error {
	var told *toldError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &told):
		return told
	case errors.Is(err, context.Canceled):
		return errors.New("cancelled before it was answered")
	case errors.Is(err, api.ErrNotFound):
		return errors.New("not found")
	case errors.Is(err, api.ErrSelfStart):
		return errors.New("not allowed: a build cannot start its own job")
	case errors.Is(err, api.ErrForbidden):
		return errors.New("not allowed: your token's user lacks the grant this needs")
	case errors.Is(err, api.ErrBadInput):
		return fmt.Errorf("bad input: %s", trimSentinel(err, api.ErrBadInput))
	case errors.Is(err, api.ErrConflict):
		return fmt.Errorf("refused: %s", trimSentinel(err, api.ErrConflict))
	case errors.Is(err, api.ErrTooManyStreams):
		return errors.New("refused: " + api.ErrTooManyStreams.Error() + "; wait for one to end")
	case errors.Is(err, api.ErrQuiet):
		return errors.New("kipp is draining before a restart and takes no new build; try again shortly")
	}
	s.log.Error("mcp", "err", err)
	return errors.New("internal error")
}

func trimSentinel(err, sentinel error) string {
	return strings.TrimPrefix(err.Error(), sentinel.Error()+": ")
}
