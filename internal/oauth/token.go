package oauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"

	kauth "github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/store"
)

// tokenResponse is RFC 6749's answer to a token request.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// mint makes the pair of tokens a grant is given: their values, and the
// hashes the store keeps of them.
func mint() (access, refresh string, is store.OAuthIssue, err error) {
	a, err := kauth.NewID()
	if err != nil {
		return "", "", is, err
	}
	r, err := kauth.NewID()
	if err != nil {
		return "", "", is, err
	}
	access, refresh = AccessPrefix+a, RefreshPrefix+r
	return access, refresh, store.OAuthIssue{
		AccessHash: kauth.HashToken(access), RefreshHash: kauth.HashToken(refresh), AccessTTL: AccessTTL, RefreshTTL: RefreshTTL,
	}, nil
}

func respond(w http.ResponseWriter, access, refresh string, scopes []string) {
	writeJSON(w, http.StatusOK, tokenResponse{AccessToken: access, TokenType: "Bearer", ExpiresIn: int(AccessTTL.Seconds()), RefreshToken: refresh, Scope: strings.Join(scopes, " ")})
}

// token is the token endpoint. Only the body is read: a parameter in the
// address is not a form field.
func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := r.ParseForm(); err != nil {
		s.writeError(w, badRequest("invalid_request", "the body is not a form"))
		return
	}
	var e *Error
	switch g := r.PostFormValue("grant_type"); g {
	case "authorization_code":
		e = s.exchangeCode(w, r)
	case "refresh_token":
		e = s.refresh(w, r)
	case "":
		e = badRequest("invalid_request", "grant_type is missing")
	default:
		e = badRequest("unsupported_grant_type", "grant_type is authorization_code or refresh_token")
	}
	if e != nil {
		s.writeError(w, e)
	}
}

// pkceMatches checks the verifier against the challenge the authorization
// request carried: S256, the only method.
func pkceMatches(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 || strings.Trim(verifier, base64URL+".~") != "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) == 1
}

func (s *Server) internal(msg string, err error) *Error {
	s.cfg.Logger.Error("oauth: "+msg, "err", err)
	return fail(http.StatusInternalServerError, "server_error", "internal error")
}

func (s *Server) exchangeCode(w http.ResponseWriter, r *http.Request) *Error {
	form := r.PostFormValue
	code := form("code")
	if code == "" || form("code_verifier") == "" || form("client_id") == "" || form("redirect_uri") == "" {
		return badRequest("invalid_request", "code, code_verifier, client_id and redirect_uri are required")
	}
	now := s.cfg.Now()
	key := kauth.HashToken(code)
	// The code is spent before anything about the request is judged: a
	// guess at the verifier costs the code.
	ac, reused := s.codes.take(key, now)
	switch {
	case ac == nil:
		return badRequest("invalid_grant", "the code is unknown or has expired")
	case reused:
		if ac.grantID != 0 {
			s.endGrantOfReusedCode(r, ac)
		}
		return badRequest("invalid_grant", "the code was already used")
	case form("client_id") != ac.clientID || form("redirect_uri") != ac.redirectURI:
		return badRequest("invalid_grant", "the code was not issued to this client and redirect_uri")
	case form("resource") != "" && canonicalResource(form("resource")) != ac.resource:
		return badRequest("invalid_target", "resource must be %s", s.resource)
	case !pkceMatches(form("code_verifier"), ac.challenge):
		return badRequest("invalid_grant", "code_verifier does not match the code_challenge")
	}
	access, refresh, is, err := mint()
	if err != nil {
		return s.internal("minting tokens", err)
	}
	c, e := s.client(r.Context(), ac.clientID)
	if e != nil {
		return badRequest("invalid_grant", "the client is no longer known")
	}
	g := &store.OAuthGrant{UserID: ac.userID, ClientID: ac.clientID, ClientName: c.Name, Resource: ac.resource, Scopes: ac.scopes, ExpiresAt: now.Add(GrantTTL)}
	switch err := s.cfg.Store.CreateOAuthGrant(r.Context(), g, is, now); {
	case errors.Is(err, store.ErrNotFound):
		return badRequest("invalid_grant", "the user is gone")
	case err != nil:
		return s.internal("storing a grant", err)
	}
	s.codes.bind(key, g.ID)
	s.audit(r.Context(), ac.userName, "oauth.grant.approve", ac.userName, "app "+strconv.Quote(g.ClientName)+", scopes "+strings.Join(g.Scopes, " "))
	respond(w, access, refresh, g.Scopes)
	return nil
}

func (s *Server) endGrantOfReusedCode(r *http.Request, ac *authCode) {
	err := s.cfg.Store.DeleteOAuthGrant(r.Context(), ac.grantID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.cfg.Logger.Error("oauth: ending the grant of a code used twice", "err", err)
	}
	s.audit(r.Context(), "oauth", "oauth.code.reuse", ac.userName, "app "+strconv.Quote(ac.clientID)+", its grant ended")
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) *Error {
	form := r.PostFormValue
	token, clientID := form("refresh_token"), form("client_id")
	if token == "" || clientID == "" {
		return badRequest("invalid_request", "refresh_token and client_id are required")
	}
	if res := form("resource"); res != "" && canonicalResource(res) != s.resource {
		return badRequest("invalid_target", "resource must be %s", s.resource)
	}
	if !strings.HasPrefix(token, RefreshPrefix) {
		return badRequest("invalid_grant", "the refresh token is unknown or has expired")
	}
	access, refresh, is, err := mint()
	if err != nil {
		return s.internal("minting tokens", err)
	}
	g, err := s.cfg.Store.RotateOAuthRefresh(r.Context(), kauth.HashToken(token), clientID, is, s.cfg.Now())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return badRequest("invalid_grant", "the refresh token is unknown or has expired")
	case errors.Is(err, store.ErrOAuthReuse):
		var who string
		if u, uerr := s.cfg.Store.UserByID(r.Context(), g.UserID); uerr == nil {
			who = u.Name
		}
		s.audit(r.Context(), "oauth", "oauth.refresh.reuse", who, "app "+strconv.Quote(g.ClientName)+", a refresh token used twice; its grant ended")
		return badRequest("invalid_grant", "the refresh token was already used; the grant is ended")
	case err != nil:
		return s.internal("rotating a refresh token", err)
	}
	respond(w, access, refresh, g.Scopes)
	return nil
}
