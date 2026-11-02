package config

import (
	"net/url"
	"strings"
)

// RedactURL is a repo as an error or a log may say it: a URL without the
// user and password it may carry, since a log is read by whoever may view
// the job. Anything else is as it was.
func RedactURL(repo string) string {
	scheme, rest, at, ok := userinfo(repo)
	if !ok {
		return repo
	}
	return scheme + "://" + rest[at+1:]
}

// URLCredentials are the parts of a repo's URL a clone's log masks: the
// password, or over HTTP the user alone, which is then a token. The user
// of an ssh URL (git@) is no secret, and masking it would mask the word.
func URLCredentials(repo string) []string {
	scheme, rest, at, ok := userinfo(repo)
	if !ok {
		return nil
	}
	info := rest[:at]
	user, pass, hasPass := strings.Cut(info, ":")
	switch {
	case hasPass && pass != "":
		return withUnescaped(info, pass)
	case !hasPass && (strings.EqualFold(scheme, "https") || strings.EqualFold(scheme, "http")):
		return withUnescaped(user)
	}
	return nil
}

// userinfo finds the user and password of a URL: rest is what follows
// "://", and its userinfo ends at the at sign at index at, the last one
// before the path, so that a password holding an at sign is all taken.
func userinfo(repo string) (scheme, rest string, at int, ok bool) {
	scheme, rest, found := strings.Cut(repo, "://")
	if !found {
		return "", "", 0, false
	}
	authority := rest
	if end := strings.IndexAny(rest, "/?#"); end >= 0 {
		authority = rest[:end]
	}
	at = strings.LastIndex(authority, "@")
	return scheme, rest, at, at >= 0
}

// withUnescaped adds to values each one percent-decoded, where that is
// another: git may print either.
func withUnescaped(values ...string) []string {
	out := values
	for _, v := range values {
		if u, err := url.PathUnescape(v); err == nil && u != v {
			out = append(out, u)
		}
	}
	return out
}
