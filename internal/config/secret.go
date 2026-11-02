package config

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

// The kinds of a secret: a one-line value (the default), the bytes of a
// file, and a user with a password.
const (
	SecretText     = "text"
	SecretFile     = "file"
	SecretUserPass = "userpass"
)

// The bounds of a file secret, and of a user name.
const (
	MinSecretFile = 1
	MaxSecretFile = 1 << 20
	MaxSecretUser = 256
)

// MinMaskedLine is the shortest line of a file secret that is masked, as
// MinSecret is for a one-line value.
const MinMaskedLine = MinSecret

// The bounds of a secret's value. MinSecret keeps a value from being so
// short that masking it would blank ordinary words out of every log.
const (
	MinSecret = 8
	MaxSecret = 64 << 10
)

// CheckSecret says why a secret may not be stored under name with value,
// or nil. The refusal never holds the value.
func CheckSecret(name, value string) error {
	if err := CheckSecretName(name); err != nil {
		return err
	}
	return CheckSecretValue(value)
}

// CheckSecretName says why name may not name a secret, or nil: a stage
// reads the secret as a variable of that name.
func CheckSecretName(name string) error {
	if !ValidEnvName(name) {
		return fmt.Errorf("%q is not a name a shell accepts for a variable", name)
	}
	return nil
}

// CheckSecretValue says why value may not be a secret's, or nil. It is the
// one rule for the CLI and the pages alike.
func CheckSecretValue(value string) error {
	switch {
	case len(value) > MaxSecret:
		return fmt.Errorf("a secret is at most %d bytes", MaxSecret)
	case strings.ContainsAny(value, "\r\n"):
		return errors.New("a secret is one line")
	case len(value) < MinSecret:
		return fmt.Errorf("a secret is at least %d characters: a shorter one would be masked out of ordinary words in every log", MinSecret)
	}
	return nil
}

// CheckSecretFile says why data may not be a file secret's bytes, or nil.
func CheckSecretFile(data []byte) error {
	switch {
	case len(data) < MinSecretFile:
		return errors.New("a file secret is not empty")
	case len(data) > MaxSecretFile:
		return fmt.Errorf("a file secret is at most %d bytes", MaxSecretFile)
	}
	return nil
}

// CheckSecretUser says why user may not be a userpass secret's user, or nil.
func CheckSecretUser(user string) error {
	switch {
	case user == "":
		return errors.New("a user is not empty")
	case len(user) > MaxSecretUser:
		return fmt.Errorf("a user is at most %d bytes", MaxSecretUser)
	case strings.ContainsAny(user, "\r\n:"):
		return errors.New("a user is one line and holds no colon")
	}
	return nil
}

// SecretMasks are the strings a log must never show of a file secret's
// bytes: each line of 8 or more characters once surrounding whitespace is
// trimmed. Bytes that are not UTF-8 are not masked.
func SecretMasks(data []byte) []string {
	if !utf8.Valid(data) {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		if utf8.RuneCountInString(line) >= MinMaskedLine && !seen[line] {
			seen[line] = true
			out = append(out, line)
		}
	}
	return out
}

// DerivedSecretNames are the variables a userpass secret called name adds
// beside name itself.
func DerivedSecretNames(name string) []string { return []string{name + "_USR", name + "_PSW"} }

// The scopes of a secret: which jobs may list it. global is every job,
// group:<name> the jobs of that group, job:<pattern> the jobs whose name
// path.Match reads the pattern against.
const (
	ScopeGlobal      = "global"
	ScopeGroupPrefix = "group:"
	ScopeJobPrefix   = "job:"
)

// CheckScope says why s is not a scope, or nil: a group is a name, and a
// job's pattern is one path.Match can read.
func CheckScope(s string) error {
	switch {
	case s == ScopeGlobal:
		return nil
	case strings.HasPrefix(s, ScopeGroupPrefix):
		if !ValidName(strings.TrimPrefix(s, ScopeGroupPrefix)) {
			return fmt.Errorf("scope %q: a group name is lower-case letters, digits, dot, dash and underscore", s)
		}
		return nil
	case strings.HasPrefix(s, ScopeJobPrefix):
		pattern := strings.TrimPrefix(s, ScopeJobPrefix)
		if pattern == "" || strings.ContainsAny(pattern, "\r\n") {
			return fmt.Errorf("scope %q: a job pattern is one non-empty line", s)
		}
		if _, err := path.Match(pattern, ""); err != nil {
			return fmt.Errorf("scope %q: %w", s, err)
		}
		return nil
	}
	return fmt.Errorf("scope %q: use %s, %s<name> or %s<pattern>", s, ScopeGlobal, ScopeGroupPrefix, ScopeJobPrefix)
}

// CheckScopes says why scopes may not be a secret's, or nil: at least one,
// each a scope.
func CheckScopes(scopes []string) error {
	if len(scopes) == 0 {
		return errors.New("a secret has at least one scope")
	}
	var errs []error
	for _, s := range scopes {
		errs = append(errs, CheckScope(s))
	}
	return errors.Join(errs...)
}

// ScopesReach reports whether a secret with these scopes may be listed by
// j. A job matches by its own name, then by the name of the job file it
// comes from: a child by its parent's, a repo of an owner job by the owner
// job's. Its group is its own, which a child has from its parent.
func ScopesReach(scopes []string, j *Job) bool {
	for _, s := range scopes {
		switch {
		case s == ScopeGlobal:
			return true
		case strings.HasPrefix(s, ScopeGroupPrefix):
			if j.Group != "" && j.Group == strings.TrimPrefix(s, ScopeGroupPrefix) {
				return true
			}
		case strings.HasPrefix(s, ScopeJobPrefix):
			if matchesJob(strings.TrimPrefix(s, ScopeJobPrefix), j) {
				return true
			}
		}
	}
	return false
}

// matchesJob reports whether a path.Match pattern names j: by its own name,
// then by the name of the job file it comes from (a child's parent, a repo
// job's owner job). A pattern path.Match cannot read matches nothing.
func matchesJob(pattern string, j *Job) bool {
	for _, n := range []string{j.Name, j.Parent, j.Folder} {
		if ok, err := path.Match(pattern, n); n != "" && err == nil && ok {
			return true
		}
	}
	return false
}
