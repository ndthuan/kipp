package config

import (
	"slices"
	"testing"
)

func TestAURLIsSaidWithoutItsUserAndPassword(t *testing.T) {
	for in, want := range map[string]string{
		"https://user:pw@host/o/r.git": "https://host/o/r.git",
		"https://token@host/o/r.git":   "https://host/o/r.git",
		"ssh://git@host:22/o/r.git":    "ssh://host:22/o/r.git",
		"https://host/o/r.git?x=a@b":   "https://host/o/r.git?x=a@b",
		"https://u:p@ss@host/o":        "https://host/o",
		"https://u:pw@host":            "https://host",
		"o/r":                          "o/r",
		"git@github.com:o/r.git":       "git@github.com:o/r.git",
		"/srv/git/r.git":               "/srv/git/r.git",
	} {
		if got := RedactURL(in); got != want {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
}

func TestTheCredentialsOfAURLAreItsPasswordOrAnHTTPUserAlone(t *testing.T) {
	for in, want := range map[string][]string{
		"https://user:pw@host/o":    {"user:pw", "pw"},
		"https://user:p%40w@host/o": {"user:p%40w", "p%40w", "user:p@w", "p@w"},
		"https://ghp_token@host/o":  {"ghp_token"},
		"ssh://git@host/o":          nil,
		"ssh://deploy:pw@host/o":    {"deploy:pw", "pw"},
		"https://user:@host/o":      nil,
		"https://host/o":            nil,
		"git@github.com:o/r.git":    nil,
	} {
		if got := URLCredentials(in); !slices.Equal(got, want) {
			t.Errorf("%s: got %q, want %q", in, got, want)
		}
	}
}
