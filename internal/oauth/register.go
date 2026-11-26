package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/oauthex"

	kauth "github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/store"
)

const maxClients = 1000

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	from := s.cfg.Client(r)
	if wait, ok := s.signups.take(from); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		s.writeError(w, fail(http.StatusTooManyRequests, "temporarily_unavailable", "too many registrations; try again later"))
		return
	}
	var meta oauthex.ClientRegistrationMetadata
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&meta); err != nil {
		s.signups.refund(from)
		s.writeError(w, badRequest("invalid_client_metadata", "the body is not the client's metadata as JSON"))
		return
	}
	if e := checkRegistration(&meta); e != nil {
		s.signups.refund(from)
		s.writeError(w, e)
		return
	}
	if e := s.room(r.Context()); e != nil {
		s.writeError(w, e)
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

func (s *Server) room(ctx context.Context) *Error {
	n, err := s.cfg.Store.CountOAuthClients(ctx)
	if err != nil {
		return s.internal("counting clients", err)
	}
	if n < maxClients {
		return nil
	}
	gone, err := s.cfg.Store.DeleteIdlestOAuthClient(ctx)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return fail(http.StatusServiceUnavailable, "temporarily_unavailable", "too many clients are registered")
	case err != nil:
		return s.internal("making room for a client", err)
	}
	s.cfg.Logger.Info("oauth: the table of clients is full, so the one unused for longest that no grant names was removed", "client_id", gone.ID, "name", gone.Name)
	return nil
}

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
