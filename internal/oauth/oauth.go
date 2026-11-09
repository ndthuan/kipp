// Package oauth is the authorization server that lets an MCP client such as
// Claude sign in to kipp's /mcp endpoint: OAuth 2.1 with PKCE for public
// clients, a client being named by the URL of its metadata document or
// registered at /oauth/register. Its pieces that need a browser session
// (the consent page, the list of connected apps) are served by package web;
// this package holds the rules and the endpoints that do not.
package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/oauthex"

	kauth "github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/store"
)

// The two scopes. Reading is always granted; building is the user's to
// grant or not.
const (
	ScopeRead  = "kipp:read"
	ScopeBuild = "kipp:build"
)

// AccessPrefix and RefreshPrefix tell the tokens apart from API tokens
// (kipp_) and runner tokens (kippa_), and from each other.
const (
	AccessPrefix  = "kippo_"
	RefreshPrefix = "kippr_"
)

// How long things last. A grant is the consent: it ends GrantTTL after it
// was given however often it was refreshed.
const (
	AccessTTL  = time.Hour
	RefreshTTL = 30 * 24 * time.Hour
	GrantTTL   = 90 * 24 * time.Hour
	CodeTTL    = time.Minute
)

// maxBody caps what the endpoints read.
const maxBody = 64 << 10

// ErrInvalidToken is a token nobody holds: unknown, expired, revoked, not
// an access token or not for this server's resource.
var ErrInvalidToken = errors.New("invalid token")

// Config is what the server is given.
type Config struct {
	Store *store.Store
	// BaseURL is the issuer: the origin kipp is reached at, as ParseBaseURL
	// returns it.
	BaseURL string
	// ResourcePath is where the protected resource is, like /mcp; its URL
	// is the audience of every token.
	ResourcePath string
	Logger       *slog.Logger
	// Client says where a request comes from, for the limit on
	// registrations; nil counts every request as one client.
	Client func(*http.Request) string
	// Fetch is the HTTP client that fetches a client's metadata document;
	// nil is GuardedClient, which refuses private addresses. A test serving
	// a client from a local server passes its own.
	Fetch *http.Client
	Now   func() time.Time
}

// Server is the authorization server.
type Server struct {
	cfg      Config
	resource string
	codes    *codes
	limit    *kauth.Throttle
	cimd     *cimdCache
}

// New makes the server.
func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Fetch == nil {
		cfg.Fetch = GuardedClient()
	}
	if cfg.Client == nil {
		cfg.Client = func(*http.Request) string { return "" }
	}
	return &Server{cfg: cfg, resource: cfg.BaseURL + cfg.ResourcePath, codes: newCodes(), limit: kauth.NewThrottle(cfg.Now), cimd: newCIMDCache()}
}

// ParseBaseURL checks the address kipp is reached at and returns it as the
// issuer is written: https, or http on this machine alone, an origin with
// no path, query or credentials.
func ParseBaseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/"):
		return "", errors.New("must be an origin like https://ci.example.com, with no path")
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopbackHost(u.Hostname()):
	default:
		return "", errors.New("must be an https:// URL (http:// only for localhost or 127.0.0.1)")
	}
	return u.Scheme + "://" + strings.ToLower(u.Host), nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Issuer is the authorization server's identifier.
func (s *Server) Issuer() string { return s.cfg.BaseURL }

// Resource is the protected resource tokens are for.
func (s *Server) Resource() string { return s.resource }

// ResourceMetadataURL is where the resource's metadata is, which a 401
// from it points to.
func (s *Server) ResourceMetadataURL() string {
	return s.cfg.BaseURL + "/.well-known/oauth-protected-resource" + s.cfg.ResourcePath
}

// Scopes are what a client may ask for.
func Scopes() []string { return []string{ScopeRead, ScopeBuild} }

// Register mounts the endpoints that need no browser session. The consent
// page, /oauth/authorize, is the site's.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.resourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource"+s.cfg.ResourcePath, s.resourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.serverMetadata)
	mux.HandleFunc("POST /oauth/token", s.token)
	mux.HandleFunc("POST /oauth/register", s.register)
	mux.HandleFunc("POST /oauth/revoke", s.revoke)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) resourceMetadata(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	writeJSON(w, http.StatusOK, oauthex.ProtectedResourceMetadata{
		Resource:               s.resource,
		AuthorizationServers:   []string{s.cfg.BaseURL},
		ScopesSupported:        Scopes(),
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "kipp",
	})
}

// serverMetadata is RFC 8414's document, written here and not with the
// SDK's type, whose jwks_uri is required and kipp signs nothing.
type serverMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint"`
	RevocationEndpoint                string   `json:"revocation_endpoint"`
	ScopesSupported                   []string `json:"scopes_supported"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	ClientIDMetadataDocumentSupported bool     `json:"client_id_metadata_document_supported"`
	// IssParameterSupported: every authorization response carries iss
	// (RFC 9207), so that a client can tell which server answered.
	IssParameterSupported bool `json:"authorization_response_iss_parameter_supported"`
}

func (s *Server) serverMetadata(w http.ResponseWriter, _ *http.Request) {
	b := s.cfg.BaseURL
	w.Header().Set("Access-Control-Allow-Origin", "*")
	writeJSON(w, http.StatusOK, serverMetadata{
		Issuer: b, AuthorizationEndpoint: b + "/oauth/authorize", TokenEndpoint: b + "/oauth/token",
		RegistrationEndpoint: b + "/oauth/register", RevocationEndpoint: b + "/oauth/revoke",
		ScopesSupported: Scopes(), ResponseTypesSupported: []string{"code"},
		GrantTypesSupported:               []string{"authorization_code", "refresh_token"},
		CodeChallengeMethodsSupported:     []string{"S256"},
		TokenEndpointAuthMethodsSupported: []string{"none"},
		ClientIDMetadataDocumentSupported: true, IssParameterSupported: true,
	})
}

// Error is an answer an endpoint gives in RFC 6749's words.
type Error struct {
	Status      int    `json:"-"`
	Code        string `json:"error"`
	Description string `json:"error_description,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Description }

func fail(status int, code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Description: fmt.Sprintf(format, args...)}
}

func badRequest(code, format string, args ...any) *Error {
	return fail(http.StatusBadRequest, code, format, args...)
}

func (s *Server) writeError(w http.ResponseWriter, e *Error) {
	writeJSON(w, e.Status, e)
}

// audit records a change to the access apps have. A row that cannot be
// written is logged: it does not undo the change.
func (s *Server) audit(ctx context.Context, actor, action, target, detail string) {
	err := s.cfg.Store.AppendAudit(context.WithoutCancel(ctx), store.Audit{At: s.cfg.Now(), Actor: actor, Action: action, Target: target, Detail: detail})
	if err != nil {
		s.cfg.Logger.Error("audit: a change was made and not recorded", "actor", actor, "action", action, "target", target, "err", err)
	}
}
