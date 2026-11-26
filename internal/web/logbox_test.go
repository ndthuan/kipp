package web

import (
	"regexp"
	"strings"
	"testing"
)

func TestEveryStageLogIsItsOwnScrollBoxSoALongLogDoesNotSetThePageHeight(t *testing.T) {
	css, err := staticFS.ReadFile("static/kipp.css")
	must(t, err)
	rule := regexp.MustCompile(`(?s)\n  \.log pre \{(.*?)\}`).FindStringSubmatch(string(css))
	if rule == nil {
		t.Fatal("kipp.css has no .log pre rule")
	}
	for _, want := range []string{"max-height: 70vh;", "overflow: auto;", "position: relative;", "white-space: pre-wrap;", "overflow-wrap: anywhere;"} {
		if !strings.Contains(rule[1], want) {
			t.Errorf(".log pre lacks %q: a long log would stretch the page or scroll it sideways", want)
		}
	}
}
