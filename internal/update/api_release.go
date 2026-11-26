//go:build !kipptest

package update

func target() (base string, hosts []string, insecure bool) {
	return releaseAPI, defaultHosts, false
}
