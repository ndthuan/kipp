package runnerbin

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestAMissingRunnerBinarySaysHowToBuildOne(t *testing.T) {
	_, err := lookup(fstest.MapFS{"bin/README": {}}, "linux", "arm64")
	if err == nil || !strings.Contains(err.Error(), "linux/arm64") || !strings.Contains(err.Error(), "make build") {
		t.Fatal(err)
	}
}

func TestTheRunnerBinaryOfAnEnginesArchitectureIsGiven(t *testing.T) {
	fsys := fstest.MapFS{"bin/kipp-runner_linux_amd64": {Data: []byte("amd")}, "bin/kipp-runner_linux_arm64": {Data: []byte("arm")}}
	got, err := lookup(fsys, "linux", "arm64")
	if err != nil || string(got) != "arm" {
		t.Fatal(string(got), err)
	}
}
