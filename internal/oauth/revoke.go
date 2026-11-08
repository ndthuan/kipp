package oauth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	kauth "github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/store"
)

// revoke is RFC 7009. The token is the proof of the right to revoke it, and
// a token of either kind ends the whole grant: an app that gives one up has
// no use for the other. An unknown token is no error.
func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := r.ParseForm(); err != nil {
		s.writeError(w, badRequest("invalid_request", "the body is not a form"))
		return
	}
	token := r.PostFormValue("token")
	if token == "" {
		s.writeError(w, badRequest("invalid_request", "token is required"))
		return
	}
	if strings.HasPrefix(token, AccessPrefix) || strings.HasPrefix(token, RefreshPrefix) {
		s.endGrantOf(r.Context(), kauth.HashToken(token), r.PostFormValue("client_id"))
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) endGrantOf(ctx context.Context, hash, clientID string) {
	g, err := s.cfg.Store.OAuthGrantOfToken(ctx, hash)
	switch {
	case errors.Is(err, store.ErrNotFound), err == nil && clientID != "" && clientID != g.ClientID:
		return
	case err != nil:
		s.cfg.Logger.Error("oauth: revoking a token", "err", err)
		return
	}
	if err := s.cfg.Store.DeleteOAuthGrant(ctx, g.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.cfg.Logger.Error("oauth: revoking a token", "err", err)
		return
	}
	s.auditRevoked(ctx, "oauth", "revoked by the app", g)
}
