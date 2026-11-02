package agentbin

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestAMissingAgentBinarySaysHowToBuildOne(t *testing.T) {
	_, err := lookup(fstest.MapFS{"bin/README": {}}, "linux", "arm64")
	if err == nil || !strings.Contains(err.Error(), "linux/arm64") || !strings.Contains(err.Error(), "make build") {
		t.Fatal(err)
	}
}

func TestTheAgentBinaryOfAnEnginesArchitectureIsGiven(t *testing.T) {
	fsys := fstest.MapFS{"bin/kipp-agent_linux_amd64": {Data: []byte("amd")}, "bin/kipp-agent_linux_arm64": {Data: []byte("arm")}}
	got, err := lookup(fsys, "linux", "arm64")
	if err != nil || string(got) != "arm" {
		t.Fatal(string(got), err)
	}
}
