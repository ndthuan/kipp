package auth

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"maps"
	"strings"
	"sync"

	"github.com/ndthuan/kipp/internal/store"
)

// BuildTokenPrefix is what a build's token starts with, so that one found
// in a log or a repo is told from a user's, a runner's and a cache token.
const BuildTokenPrefix = "kippb_"

// BuildTokens are the tokens of the builds that run, held in memory only:
// a token dies with Revoke of its build and with the server, whose builds
// die with it. They are looked up by the SHA-256 of the token, which is
// never kept.
type BuildTokens struct {
	mu      sync.Mutex
	tokens  map[string]*buildToken // by the hash of the token
	byBuild map[int64]string       // a build's token hash
}

type buildToken struct {
	id        int64
	principal store.BuildPrincipal
}

// NewBuildTokens is an empty set of tokens.
func NewBuildTokens() *BuildTokens {
	return &BuildTokens{tokens: map[string]*buildToken{}, byBuild: map[int64]string{}}
}

// Mint is the token of build id, number number of job (of the multibranch
// job parent, for a child), with perms as its grants. A build holds one: a
// second Mint replaces the first.
func (t *BuildTokens) Mint(id int64, job string, number int, parent string, perms map[string]store.Perm) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system's random source failing is not a state to run on
	}
	plain := BuildTokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	h := HashToken(plain)
	t.mu.Lock()
	defer t.mu.Unlock()
	if old, ok := t.byBuild[id]; ok {
		delete(t.tokens, old)
	}
	t.tokens[h] = &buildToken{id: id, principal: store.BuildPrincipal{Job: job, Number: number, Parent: parent, Perms: maps.Clone(perms)}}
	t.byBuild[id] = h
	return plain
}

// Regrant gives build id's token perms in place of what it held. A build
// with no token is left as it is.
func (t *BuildTokens) Regrant(id int64, perms map[string]store.Perm) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if tok := t.tokens[t.byBuild[id]]; tok != nil {
		tok.principal.Perms = maps.Clone(perms)
	}
}

// Revoke ends the token of build id, and does nothing for a build with none.
func (t *BuildTokens) Revoke(id int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.tokens, t.byBuild[id])
	delete(t.byBuild, id)
}

// user is the principal of plain, or store.ErrNotFound for a token nobody
// holds, as for a user's token.
func (t *BuildTokens) user(plain string) (*store.User, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tok := t.tokens[HashToken(plain)]
	if tok == nil {
		return nil, store.ErrNotFound
	}
	p := tok.principal
	p.Perms = maps.Clone(p.Perms)
	return &store.User{ID: -tok.id, Name: fmt.Sprintf("build %s#%d", p.Job, p.Number), Build: &p}, nil
}

// isBuildToken reports whether plain is shaped like a build's token.
func isBuildToken(plain string) bool { return strings.HasPrefix(plain, BuildTokenPrefix) }
