// Package mcp serves kipp to an MCP client (a Claude chat, Claude Code) at
// /mcp, when kipp serve is given --mcp. A caller is a kipp user, found by
// the API token it sends, and every tool answers through the same
// permission-checked operations as the REST API, so what a caller sees and
// does is what that user's grants allow, and no more.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ndthuan/kipp/internal/api"
	kauth "github.com/ndthuan/kipp/internal/auth"
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
type Ceiling struct{ View, Build, Cancel bool }

// AllGrants is the ceiling of a credential that acts as its user.
var AllGrants = Ceiling{View: true, Build: true, Cancel: true}

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

// Config is what the server is given.
type Config struct {
	Service *api.Service
	Auth    *kauth.Service
	// Verify resolves a request's token; nil is APITokens(Auth).
	Verify  Verifier
	Version string
	// BaseURL is the address of kipp's pages, empty when --base-url was not
	// given; builds link to it only when it is.
	BaseURL string
	Logger  *slog.Logger
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

// callerKey is where the verified caller travels in the token's extras.
const callerKey = "kipp.caller"

// Handler is the streamable HTTP endpoint. It is stateless: every request
// stands alone and carries its token, so no session outlives a revoked
// token and nothing is kept between requests. A request without a good
// bearer token is 401 with a Bearer challenge; sessions and cookies are not
// read at all.
func (s *Server) Handler() http.Handler {
	srv := s.newServer()
	h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, &sdk.StreamableHTTPOptions{Stateless: true})
	authed := auth.RequireBearerToken(s.verify, nil)(h)
	return http.MaxBytesHandler(challenging(authed), api.MaxBody)
}

// verify is the SDK's token check, with the caller carried in its result.
func (s *Server) verify(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	c, err := s.cfg.Verify(ctx, token)
	switch {
	case errors.Is(err, ErrInvalidToken):
		return nil, err
	case err != nil:
		s.log.Error("mcp: verifying a token", "err", err)
		return nil, errors.New("internal error")
	}
	// The token is looked up again on every request, so the expiry the SDK
	// asks for only has to lie ahead of this one.
	return &auth.TokenInfo{UserID: c.User.Name, Expiration: time.Now().Add(time.Minute), Extra: map[string]any{callerKey: c}}, nil
}

// callerOf is the caller the request was verified as.
func callerOf(req *sdk.CallToolRequest) (*Caller, error) {
	if ex := req.GetExtra(); ex != nil && ex.TokenInfo != nil {
		if c, ok := ex.TokenInfo.Extra[callerKey].(*Caller); ok {
			return c, nil
		}
	}
	return nil, errors.New("not signed in")
}

// challenging adds the Bearer challenge to a 401 that has none, so that a
// client can tell a missing token from a refusal.
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

// refusal is the error a tool answers a caller with: the API's errors in
// words for a model, and a server fault as no more than that, logged.
func (s *Server) refusal(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, api.ErrNotFound):
		return errors.New("not found")
	case errors.Is(err, api.ErrForbidden):
		return errors.New("not allowed: your token's user lacks the grant this needs")
	case errors.Is(err, api.ErrBadInput):
		return fmt.Errorf("bad input: %s", trimSentinel(err, api.ErrBadInput))
	case errors.Is(err, api.ErrConflict):
		return fmt.Errorf("refused: %s", trimSentinel(err, api.ErrConflict))
	case errors.Is(err, api.ErrQuiet):
		return errors.New("kipp is draining before a restart and takes no new build; try again shortly")
	}
	s.log.Error("mcp", "err", err)
	return errors.New("internal error")
}

func trimSentinel(err, sentinel error) string {
	return strings.TrimPrefix(err.Error(), sentinel.Error()+": ")
}
