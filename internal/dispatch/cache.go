package dispatch

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/ndthuan/kipp/internal/cache"
	"github.com/ndthuan/kipp/internal/cachesrv"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/store"
)

// cacheVars are the variables of a stage that names cache_scopes: where the
// cache is, a token of the build that reaches those scopes and nothing else,
// and the first scope as the one a command names by default. The token dies
// with the build. url is where this stage reaches the server: loopback for
// one on the server, the operator's --agent-url for one on an agent, which
// is never guessed.
func (d *Dispatcher) cacheVars(b *store.Build, e *buildEnv, s *config.Stage, url string) ([]string, error) {
	if len(s.CacheScopes) == 0 && len(s.Cache) == 0 {
		return nil, nil
	}
	if d.opts.Cache == nil {
		return nil, errors.New("this server keeps no cache, so the stage's cache or cache_scopes cannot be served")
	}
	scopes := d.cacheScopes(b, s)
	if url == "" {
		if d.remote(s) {
			return nil, errors.New("a stage on an agent reaches the cache through --agent-url, which this server was not given")
		}
		url = d.opts.BaseURL
	}
	token := d.opts.Cache.Mint(cachesrv.Grant{Build: b.ID, Scopes: scopes, Namespace: e.cacheNS})
	return []string{
		cache.URLEnv + "=" + url,
		cache.TokenEnv + "=" + token,
		cache.ScopeEnv + "=" + scopes[0],
	}, nil
}

// defaultScope is the scope of a cache that names none, and the one a
// command is given: the stage's first cache_scopes, else the job's name.
func defaultScope(b *store.Build, s *config.Stage) string {
	if len(s.CacheScopes) > 0 {
		return s.CacheScopes[0]
	}
	return b.Job
}

// cacheScopes are the scopes the build's token reaches for the stage: its
// cache_scopes, the declared caches' scopes, and the default one first.
func (d *Dispatcher) cacheScopes(b *store.Build, s *config.Stage) []string {
	scopes := []string{defaultScope(b, s)}
	for _, sc := range slices.Concat(s.CacheScopes, scopesOf(s)) {
		if !slices.Contains(scopes, sc) {
			scopes = append(scopes, sc)
		}
	}
	return scopes
}

func scopesOf(s *config.Stage) []string {
	var out []string
	for _, c := range s.Cache {
		if c.Scope != "" {
			out = append(out, c.Scope)
		}
	}
	return out
}

// declaredCaches are the stage's declared caches with keys expanded
// against env. One whose key is no key once expanded is said in out and
// left out: the stage runs without it.
func declaredCaches(b *store.Build, s *config.Stage, env []string, out io.Writer) []cache.Declared {
	vars := map[string]string{"KIPP_STAGE": s.Name}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vars[k] = v
		}
	}
	lookup := func(n string) (string, bool) { v, ok := vars[n]; return v, ok }
	var specs []cache.Declared
	for i, c := range s.Cache {
		x, err := c.Expand(lookup)
		if err != nil {
			_, _ = fmt.Fprintf(out, "kipp: cache %d skipped: %v\n", i+1, err)
			continue
		}
		scope := x.Scope
		if scope == "" {
			scope = defaultScope(b, s)
		}
		specs = append(specs, cache.Declared{Key: x.Key, RestoreKeys: x.RestoreKeys, Paths: x.Paths, Scope: scope})
	}
	return specs
}

// cacheEnv is the environment cache restore and save read for a stage run
// on the server, in dir.
func cacheEnv(vars []string, dir string, out io.Writer) cache.Env {
	home, _ := os.UserHomeDir()
	return cache.Env{
		Getenv: func(k string) string {
			for _, kv := range slices.Backward(vars) {
				if v, ok := strings.CutPrefix(kv, k+"="); ok {
					return v
				}
			}
			return ""
		},
		Dir: dir, Home: home, Stdout: out, Stderr: out,
	}
}

// localCache is the declared caches of a stage run on the server: restored
// before its command, saved after one that ended well.
type localCache struct {
	specs []cache.Declared
	hits  []bool
	env   cache.Env
}

func (c *localCache) begin(specs []cache.Declared, env cache.Env) {
	c.specs, c.env = specs, env
	c.hits = cache.RestoreDeclared(specs, env)
}

func (c *localCache) save() {
	if len(c.specs) > 0 {
		cache.SaveDeclared(c.specs, c.hits, c.env)
	}
}
