//go:build kipptest

package update

import "testing"

func TestTheKipptestBuildTakesTheAPIBaseTheEnvironmentNames(t *testing.T) {
	base, hosts, insecure := target()
	if base != releaseAPI || insecure || len(hosts) != len(defaultHosts) {
		t.Fatal("without the variable the target is GitHub's")
	}
	t.Setenv("KIPP_TEST_UPDATE_API", "http://host.docker.internal:9999")
	base, hosts, insecure = target()
	if base != "http://host.docker.internal:9999" || !insecure || hosts[0] != "host.docker.internal" {
		t.Fatalf("%q %v %v", base, hosts, insecure)
	}
}
