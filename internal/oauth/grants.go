package oauth

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	kauth "github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/store"
)

// Verified is a token that checked out.
type Verified struct {
	User   *store.User
	Grant  *store.OAuthGrant
	Scopes []string
}

// Has reports whether the grant holds scope.
func (v *Verified) Has(scope string) bool {
	return slices.Contains(v.Scopes, scope)
}

// Verify resolves an access token. A token that is not one, is unknown,
// expired or revoked, or was issued for another resource than this server's
// is ErrInvalidToken.
func (s *Server) Verify(ctx context.Context, token string) (*Verified, error) {
	if !strings.HasPrefix(token, AccessPrefix) {
		return nil, ErrInvalidToken
	}
	g, u, err := s.cfg.Store.OAuthAccess(ctx, kauth.HashToken(token), s.cfg.Now())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, ErrInvalidToken
	case err != nil:
		return nil, err
	case g.Resource != s.resource:
		return nil, ErrInvalidToken
	}
	return &Verified{User: u, Grant: g, Scopes: g.Scopes}, nil
}

// Grants are the apps a user has let in.
func (s *Server) Grants(ctx context.Context, userID int64) ([]*store.OAuthGrant, error) {
	return s.cfg.Store.OAuthGrants(ctx, userID, s.cfg.Now())
}

// Revoke ends a grant of owner's, for actor, and everything the app holds
// of it. A grant that is not the owner's, or is gone, is store.ErrNotFound.
func (s *Server) Revoke(ctx context.Context, actor string, owner *store.User, id int64) error {
	g, err := s.cfg.Store.OAuthGrant(ctx, owner.ID, id)
	if err != nil {
		return err
	}
	if err := s.cfg.Store.DeleteOAuthGrant(ctx, g.ID); err != nil {
		return err
	}
	s.auditRevoked(ctx, actor, "revoked by "+actor, g)
	return nil
}

func (s *Server) auditRevoked(ctx context.Context, actor, detail string, g *store.OAuthGrant) {
	target := ""
	if u, err := s.cfg.Store.UserByID(ctx, g.UserID); err == nil {
		target = u.Name
	}
	s.audit(ctx, actor, "oauth.grant.revoke", target, "app "+strconv.Quote(g.ClientName)+", "+detail)
}
