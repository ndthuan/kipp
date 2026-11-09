// Package cache is the cache's side that runs in a stage: the namespaces a
// build reads and writes, the tar.zst a cache entry is, the hash of files,
// and the kipp cache command that restores and saves entries through the
// server. It holds no database, so that kipp-runner, which carries it into a
// container, does not carry SQLite.
package cache

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

// Namespace is the part of a scope a build reads and writes, named for the
// code the build ran: "default", "b-<branch>" or "j-<job>". It is one file
// name, so that no branch or job name leads out of it.
type Namespace string

// Default is the namespace every build reads and a push to the default
// branch writes.
const Default Namespace = "default"

// longBranch is the length of an encoded branch above which its namespace
// is a hash: a file name holds 255 bytes.
const longBranch = 180

// NamespaceOf is where a build of the job writes its cache. A webhook build
// (pushed: also one made again from it) of a push to the default branch writes default, and so does a build of a
// job that sets writesDefault, whose owner vouches that every build of it
// runs default-branch code. Any other webhook build writes its branch's
// namespace, and every other build writes its job's: a cron build, one
// started by hand, one that runs after another, none of which says what
// code it ran.
func NamespaceOf(pushed bool, branch, defaultBranch, job string, writesDefault bool) Namespace {
	switch {
	case writesDefault:
		return Default
	case pushed && branch != "" && branch == defaultBranch:
		return Default
	case pushed && branch != "":
		return NamespaceOfBranch(branch)
	}
	return Namespace("j-" + job)
}

// NamespaceOfBranch is the namespace of a branch. The branch is encoded,
// so that one named "default" or "j-x" is not another namespace.
func NamespaceOfBranch(branch string) Namespace {
	enc := base64.RawURLEncoding.EncodeToString([]byte(branch))
	if len(enc) > longBranch {
		sum := sha256.Sum256([]byte(branch))
		return Namespace("h-" + hex.EncodeToString(sum[:]))
	}
	return Namespace("b-" + enc)
}
