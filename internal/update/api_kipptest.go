//go:build kipptest

package update

import (
	"net/url"
	"os"
)

// The end-to-end smoke serves a release lookup of its own and names its base
// URL here, plain HTTP allowed and its host let through; a build without
// the kipptest tag has no such door.
func target() (base string, hosts []string, insecure bool) {
	api := os.Getenv("KIPP_TEST_UPDATE_API")
	u, err := url.Parse(api)
	if api == "" || err != nil || u.Host == "" {
		return releaseAPI, defaultHosts, false
	}
	return api, append([]string{u.Hostname()}, defaultHosts...), true
}
