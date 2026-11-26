//go:build kipptest

package update

import (
	"net/url"
	"os"
)

func target() (base string, hosts []string, insecure bool) {
	api := os.Getenv("KIPP_TEST_UPDATE_API")
	u, err := url.Parse(api)
	if api == "" || err != nil || u.Host == "" {
		return releaseAPI, defaultHosts, false
	}
	return api, append([]string{u.Hostname()}, defaultHosts...), true
}
