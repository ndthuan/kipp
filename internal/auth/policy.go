package auth

import (
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/ndthuan/kipp/internal/config"
)

// The bounds of a password a person chooses, from NIST SP 800-63B rev. 4,
// sec. 3.1.1.2 (read 2026-10-17): at least 15 characters where the password
// is the one factor, as it is in kipp, and at least 64 accepted. The upper
// bound only keeps a request from making the hash work on megabytes.
const (
	minPassword = 15
	maxPassword = 256
)

// maxUserName keeps a name short enough for a page's header and a log line.
const maxUserName = 64

//go:embed blocklist.txt
var blocklistFile string

// blocklist is the embedded list as a sorted slice, split on first use so
// that a command which sets no password pays nothing for it.
var blocklist = sync.OnceValue(func() []string {
	return strings.Split(strings.TrimSuffix(blocklistFile, "\n"), "\n")
})

// CheckPassword says why a password a person chose may not be set, or nil.
// There is no rule on which kinds of character it holds: NIST asks for
// length and a blocklist instead. context is words a password may not
// contain in any case, the user's name and "kipp"; an empty one is skipped.
// It is checked when a password is set, never at login, so a stored one
// from before the rule still logs in.
func CheckPassword(pw string, context ...string) error {
	n := utf8.RuneCountInString(pw)
	switch {
	case n < minPassword:
		return fmt.Errorf("a password is at least %d characters", minPassword)
	case n > maxPassword:
		return fmt.Errorf("a password is at most %d characters", maxPassword)
	}
	lower := strings.ToLower(pw)
	if _, found := slices.BinarySearch(blocklist(), lower); found {
		return errors.New("this password is on a list of common or leaked ones; choose another")
	}
	for _, w := range context {
		if w != "" && strings.Contains(lower, strings.ToLower(w)) {
			return fmt.Errorf("a password may not contain %q", w)
		}
	}
	return nil
}

// ValidUserName reports whether a new user may be called name: a name a
// job can have, so it is safe in a path, a header and a log, of 1 to 64
// characters. A user made before the rule keeps their name.
func ValidUserName(name string) bool {
	return len(name) <= maxUserName && config.ValidName(name)
}

// CheckUserName says why a new user may not be called name, or nil, in the
// words the CLI and the pages both answer with.
func CheckUserName(name string) error {
	if !ValidUserName(name) {
		return fmt.Errorf("%q is not a name a user can have: 1 to 64 lower-case letters, digits, dot, dash and underscore, starting with a letter or digit", name)
	}
	return nil
}
