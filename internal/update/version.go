package update

import (
	"regexp"
	"strconv"
)

var releaseTag = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// Release reports whether v is a release's tag, vX.Y.Z; a pre-release tag,
// a dev build and a git describe string are not.
func Release(v string) bool { return releaseTag.MatchString(v) }

// triple is the numbers of a release tag, false when v is not one.
func triple(v string) ([3]int, bool) {
	m := releaseTag.FindStringSubmatch(v)
	if m == nil {
		return [3]int{}, false
	}
	var out [3]int
	for i := range out {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// Newer reports whether release tag a is a later version than release tag b;
// false when either is not a release tag.
func Newer(a, b string) bool {
	x, okA := triple(a)
	y, okB := triple(b)
	if !okA || !okB {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return false
}
