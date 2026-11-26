package oauth

import (
	"sync"
	"time"
)

type authCode struct {
	clientID, redirectURI, challenge, resource string
	userID                                     int64
	userName                                   string
	scopes                                     []string
	expires                                    time.Time
	used                                       bool
	grantID                                    int64
}

const maxCodes = 10_000

type codes struct {
	mu sync.Mutex
	m  map[string]*authCode
}

func newCodes() *codes { return &codes{m: map[string]*authCode{}} }

func (c *codes) put(key string, ac *authCode, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.m {
		if now.After(e.expires.Add(CodeTTL)) {
			delete(c.m, k)
		}
	}
	if len(c.m) >= maxCodes {
		return false
	}
	c.m[key] = ac
	return true
}

func (c *codes) take(key string, now time.Time) (ac *authCode, reused bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	switch {
	case !ok:
		return nil, false
	case !now.Before(e.expires):
		delete(c.m, key)
		return nil, false
	case e.used:
		return e, true
	}
	e.used = true
	return e, false
}

func (c *codes) bind(key string, grantID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[key]; ok {
		e.grantID = grantID
	}
}
