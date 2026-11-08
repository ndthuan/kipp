package oauth

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"

	kauth "github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/store"
)

// ErrTooManyCodes is the answer to an approval while the table of codes
// waiting to be exchanged is full.
var ErrTooManyCodes = errors.New("too many authorization codes are waiting to be exchanged")

// AuthRequest is an authorization request that checked out: the app is
// known, the address to send the user back to is one it named, and PKCE is
// there.
type AuthRequest struct {
	Client      *Client
	RedirectURI string
	State       string
	Challenge   string
	Resource    string
	// Scopes are what the app asked for, kipp:read always among them.
	Scopes []string
}

// WantsBuild reports whether the app asked to be able to start builds.
func (r *AuthRequest) WantsBuild() bool { return slices.Contains(r.Scopes, ScopeBuild) }

// Fields are the parameters of the request, to be carried through the
// consent form and checked again when it comes back.
func (r *AuthRequest) Fields() url.Values {
	return url.Values{
		"response_type": {"code"}, "client_id": {r.Client.ID}, "redirect_uri": {r.RedirectURI}, "state": {r.State},
		"code_challenge": {r.Challenge}, "code_challenge_method": {"S256"}, "resource": {r.Resource}, "scope": {strings.Join(r.Scopes, " ")},
	}
}

// AuthError is a request that cannot be granted. With a RedirectURI the
// app is told, at that address; without one the user is, on a page: the
// address is not one to trust.
type AuthError struct {
	Code, Description string
	RedirectURI       string
	State             string
}

func (e *AuthError) Error() string { return e.Code + ": " + e.Description }

// single are the parameters that may be given once only: a request that
// repeats one is read differently by different parties.
var single = []string{"client_id", "redirect_uri", "response_type", "state", "code_challenge", "code_challenge_method", "scope", "resource"}

// canonicalResource is a resource indicator as the audience is written.
func canonicalResource(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	out := strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + u.EscapedPath()
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out
}

// ParseAuthorize checks an authorization request, from the query of the
// GET or the fields of the consent form.
func (s *Server) ParseAuthorize(ctx context.Context, v url.Values) (*AuthRequest, *AuthError) {
	for _, k := range single {
		if len(v[k]) > 1 {
			return nil, &AuthError{Code: "invalid_request", Description: k + " is given more than once"}
		}
	}
	c, e := s.client(ctx, v.Get("client_id"))
	if e != nil {
		return nil, &AuthError{Code: e.Code, Description: e.Description}
	}
	uri := v.Get("redirect_uri")
	if uri == "" || !redirectAllowed(c.RedirectURIs, uri) {
		return nil, &AuthError{Code: "invalid_request", Description: "redirect_uri is not one of the addresses " + c.Name + " registered"}
	}
	state := v.Get("state")
	bad := func(code, desc string) (*AuthRequest, *AuthError) {
		return nil, &AuthError{Code: code, Description: desc, RedirectURI: uri, State: state}
	}
	if v.Get("response_type") != "code" {
		return bad("unsupported_response_type", "response_type must be code")
	}
	ch := v.Get("code_challenge")
	if v.Get("code_challenge_method") != "S256" || !validChallenge(ch) {
		return bad("invalid_request", "PKCE is required, with code_challenge_method S256 and a code_challenge")
	}
	res := s.resource
	if r := v.Get("resource"); r != "" {
		if canonicalResource(r) != s.resource {
			return bad("invalid_target", "resource must be "+s.resource)
		}
	}
	scopes, ok := parseScopes(v.Get("scope"))
	if !ok {
		return bad("invalid_scope", "the scopes are "+strings.Join(Scopes(), " "))
	}
	if err := s.cfg.Store.TouchOAuthClient(ctx, c.ID, s.cfg.Now()); err != nil {
		s.cfg.Logger.Warn("oauth: a client's use was not recorded", "err", err)
	}
	return &AuthRequest{Client: c, RedirectURI: uri, State: state, Challenge: ch, Resource: res, Scopes: scopes}, nil
}

// parseScopes reads a scope parameter: space-separated, only kipp's own,
// read always granted and the only scope when none is asked for.
func parseScopes(raw string) ([]string, bool) {
	scopes := []string{ScopeRead}
	for sc := range strings.FieldsSeq(raw) {
		switch sc {
		case ScopeRead:
		case ScopeBuild:
			if !slices.Contains(scopes, ScopeBuild) {
				scopes = append(scopes, ScopeBuild)
			}
		default:
			return nil, false
		}
	}
	return scopes, true
}

// validChallenge: a SHA-256 in base64url, the only form S256 takes.
func validChallenge(ch string) bool {
	return len(ch) == 43 && strings.Trim(ch, base64URL) == ""
}

const base64URL = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// answer is the address that sends the user back to the app with params.
func (s *Server) answer(redirectURI string, params url.Values) string {
	u, err := url.Parse(redirectURI)
	if err != nil { // it was checked against the registered ones
		return redirectURI
	}
	q := u.Query()
	for k, v := range params {
		if len(v) > 0 && v[0] != "" {
			q.Set(k, v[0])
		}
	}
	q.Set("iss", s.cfg.BaseURL)
	u.RawQuery = q.Encode()
	return u.String()
}

// ErrorRedirect is the address that tells the app a request was refused.
func (s *Server) ErrorRedirect(e *AuthError) string {
	return s.answer(e.RedirectURI, url.Values{"error": {e.Code}, "error_description": {e.Description}, "state": {e.State}})
}

// Deny is the address that tells the app the user said no.
func (s *Server) Deny(r *AuthRequest) string {
	return s.ErrorRedirect(&AuthError{Code: "access_denied", Description: "the user denied the request", RedirectURI: r.RedirectURI, State: r.State})
}

// Approve gives a code to the app, at its address: for kipp:read, and for
// kipp:build when the app asked for it and build is true.
func (s *Server) Approve(r *AuthRequest, u *store.User, build bool) (string, error) {
	scopes := []string{ScopeRead}
	if build && r.WantsBuild() {
		scopes = append(scopes, ScopeBuild)
	}
	code, err := kauth.NewID()
	if err != nil {
		return "", err
	}
	now := s.cfg.Now()
	ac := &authCode{clientID: r.Client.ID, redirectURI: r.RedirectURI, challenge: r.Challenge, resource: r.Resource,
		userID: u.ID, userName: u.Name, scopes: scopes, expires: now.Add(CodeTTL)}
	if !s.codes.put(kauth.HashToken(code), ac, now) {
		return "", ErrTooManyCodes
	}
	return s.answer(r.RedirectURI, url.Values{"code": {code}, "state": {r.State}}), nil
}
