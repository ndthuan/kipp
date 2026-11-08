//go:build !kipptest

package update

// target is where the release lookup goes: always the constants.
func target() (base string, hosts []string, insecure bool) {
	return releaseAPI, defaultHosts, false
}
