package config

import (
	"strings"
	"testing"
)

func TestNameOfIsTheNameKeyAndRefusesATextWithout(t *testing.T) {
	name, err := NameOf([]byte("# a job\nname: fresh\nstages:\n  s: {run: 'true'}\n"))
	if err != nil || name != "fresh" {
		t.Fatalf("got %q, %v", name, err)
	}
	if _, err := NameOf([]byte("stages:\n  s: {run: 'true'}\n")); err == nil || !strings.Contains(err.Error(), "name:") {
		t.Fatalf("a text without a name: %v", err)
	}
	if _, err := NameOf([]byte("name: [\n")); err == nil || !strings.Contains(err.Error(), "line") {
		t.Fatalf("a text that is not YAML says where: %v", err)
	}
}
