package oauth

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/oauthex"

	kauth "github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/store"
)

// maxClients bounds the table that anybody may add a row to.
const maxClients = 1000

// register is Dynamic Client Registration (RFC 7591) for public clients.
// Anyone may register, so it is counted per client as wrong passwords are
// and held to the same slowing; a registration unused for 30 days is swept.
func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	if wait, ok := s.limit.Reserve("register\x00" + s.cfg.Client(r)); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		s.writeError(w, fail(http.StatusTooManyRequests, "temporarily_unavailable", "too many registrations; try again later"))
		return
	}
	var meta oauthex.ClientRegistrationMetadata
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&meta); err != nil {
		s.writeError(w, badRequest("invalid_client_metadata", "the body is not the client's metadata as JSON"))
		return
	}
	if e := checkRegistration(&meta); e != nil {
		s.writeError(w, e)
		return
	}
	if n, err := s.cfg.Store.CountOAuthClients(r.Context()); err != nil {
		s.writeError(w, s.internal("counting clients", err))
		return
	} else if n >= maxClients {
		s.writeError(w, fail(http.StatusServiceUnavailable, "temporarily_unavailable", "too many clients are registered"))
		return
	}
	id, err := kauth.NewID()
	if err != nil {
		s.writeError(w, s.internal("making a client id", err))
		return
	}
	now := s.cfg.Now()
	name := clientName(meta.ClientName, "An app")
	if _, err := s.cfg.Store.SaveOAuthClient(r.Context(), &store.OAuthClient{ID: id, Name: name, RedirectURIs: meta.RedirectURIs}, now); err != nil {
		s.writeError(w, s.internal("storing a client", err))
		return
	}
	s.audit(r.Context(), "oauth", "oauth.client.register", name, "client_id "+id+", registered at /oauth/register, redirecting to "+strings.Join(meta.RedirectURIs, " "))
	writeJSON(w, http.StatusCreated, &oauthex.ClientRegistrationResponse{
		RedirectURIs: meta.RedirectURIs, TokenEndpointAuthMethod: "none", ClientName: name,
		GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"},
		ClientID: id, ClientIDIssuedAt: now,
	})
}

// checkRegistration refuses what kipp cannot honour. The authentication
// method asked for is not checked: every client is public, and the
// response says so.
func checkRegistration(m *oauthex.ClientRegistrationMetadata) *Error {
	if err := checkRedirects(m.RedirectURIs); err != nil {
		return badRequest("invalid_redirect_uri", "%v", err)
	}
	for _, g := range m.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			return badRequest("invalid_client_metadata", "grant_types may hold authorization_code and refresh_token")
		}
	}
	if len(m.GrantTypes) > 0 && !slices.Contains(m.GrantTypes, "authorization_code") {
		return badRequest("invalid_client_metadata", "grant_types must hold authorization_code")
	}
	for _, t := range m.ResponseTypes {
		if t != "code" {
			return badRequest("invalid_client_metadata", "response_types may hold code only")
		}
	}
	return nil
}
