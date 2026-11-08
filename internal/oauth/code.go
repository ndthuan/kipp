package oauth

import (
	"sync"
	"time"
)

// authCode is what a code stands for until it is exchanged. Codes live in
// memory: they last a minute, and a restart costs a user one more consent.
type authCode struct {
	clientID, redirectURI, challenge, resource string
	userID                                     int64
	userName                                   string
	scopes                                     []string
	expires                                    time.Time
	used                                       bool
	// grantID is the grant the exchange made, so that a second use of the
	// code can end it.
	grantID int64
}

// maxCodes bounds the table: a code is made by a user in sudo mode, and
// this is only the limit past which something is wrong.
const maxCodes = 10_000

type codes struct {
	mu sync.Mutex
	m  map[string]*authCode
}

func newCodes() *codes { return &codes{m: map[string]*authCode{}} }

// put stores a code by the hash of its value, and says whether there was
// room. Codes that ended long ago are dropped on the way.
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

// take hands out a code once. A code that is unknown or has run out is
// nil; one already taken comes back with reused set, for its grant to be
// ended.
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

// bind records the grant a code made.
func (c *codes) bind(key string, grantID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[key]; ok {
		e.grantID = grantID
	}
}
