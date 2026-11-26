package web

import (
	"regexp"
	"strings"
	"testing"
)

// A button that stands alone in a page's content is bordered; the quiet look
// is for the header, menus and the small actions inside a table row.
func TestStandaloneButtonsOnAccountAndAdminPagesAreBordered(t *testing.T) {
	tag := regexp.MustCompile(`<(?:a|button|summary)\b[^>]*>`)
	for _, f := range []string{"account.html", "server.html", "user.html"} {
		raw, err := templateFS.ReadFile("templates/" + f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range tag.FindAllString(string(raw), -1) {
			if strings.Contains(m, "action-quiet") && !strings.Contains(m, "action-small") {
				t.Errorf("%s: standalone control is quiet: %s", f, m)
			}
		}
	}
}
