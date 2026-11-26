package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestEngineContainersBelongToTheDataDirectoryNotToTheBaseURL(t *testing.T) {
	one, two := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	if first, again := containerOwner(one), containerOwner(bytes.Repeat([]byte{1}, 32)); first != again {
		t.Fatal("the owner of one data directory's containers changes between starts")
	}
	if containerOwner(one) == containerOwner(two) {
		t.Fatal("two data directories own each other's containers")
	}
	if id := containerOwner(one); len(id) != 32 || strings.Contains(id, "0101") {
		t.Fatalf("owner %q", id)
	}
}
