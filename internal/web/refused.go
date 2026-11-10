package web

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/ndthuan/kipp/internal/api"
)

// nameRule is a name rule of the server as a browser is told it, on the
// field that takes such a name: Pattern for its pattern attribute (read
// with the v flag, so a literal - in a class is escaped), Max for its
// maxlength when the rule has one, Title for what the browser says when
// the pattern is not met. The rule itself is the server's; a test holds
// each pattern to it.
type nameRule struct {
	Pattern, Title string
	Max            int
}

// nameRules are by the rule: job for config.ValidName, which engines and
// runners are named by too, user for auth.ValidUserName, secret for
// config.ValidEnvName, app for config.CheckAppName.
var nameRules = map[string]nameRule{
	"job": {Pattern: `[a-z0-9][a-z0-9._\-]*`,
		Title: "Lower-case letters, digits, dots, hyphens and underscores, starting with a letter or a digit"},
	"user": {Pattern: `[a-z0-9][a-z0-9._\-]*`, Max: 64,
		Title: "1 to 64 lower-case letters, digits, dots, hyphens and underscores, starting with a letter or a digit"},
	"secret": {Pattern: `[A-Za-z_][A-Za-z0-9_]*`,
		Title: "Letters, digits and underscores, not starting with a digit"},
	"app": {Pattern: `[a-z0-9][a-z0-9\-]*`,
		Title: "Lower-case letters, digits and hyphens, starting with a letter or a digit"},
}

// nameRuleOf is the rule called name; the pages name only rules there are.
func nameRuleOf(name string) (nameRule, error) {
	if r, ok := nameRules[name]; ok {
		return r, nil
	}
	return nameRule{}, errors.New("no name rule " + name)
}

// refusedAs answers err with show, the form's page, when err is a refusal
// of what the form holds, and with the error page for any other error: a
// page the user may not see, the password step, the server.
func (s *Site) refusedAs(w http.ResponseWriter, r *http.Request, err error, show func(code int, msg string)) {
	if !errors.Is(err, api.ErrBadInput) && !errors.Is(err, api.ErrConflict) {
		s.fail(w, r, err)
		return
	}
	show(refusal(w, err))
}

// errorData is where the error page leads back to, and what its link says.
type errorData struct{ Back, Label string }

// backFrom is the page of this site r came from, by the path of its
// Referer when that is this site's, and the jobs otherwise. The query is
// left behind: the page is to be seen again, not a form filled again.
func backFrom(r *http.Request) errorData {
	ref, err := url.Parse(r.Referer())
	if err == nil && (ref.Scheme == "http" || ref.Scheme == "https") && ref.Host == r.Host {
		if p := ref.EscapedPath(); localPath(p) == p && p != "" {
			return errorData{Back: p, Label: "Back"}
		}
	}
	return errorData{Back: "/", Label: "Back to the jobs"}
}

// errorPage answers with the error page: title, the words of msg, and a
// link back.
func (s *Site) errorPage(w http.ResponseWriter, r *http.Request, code int, title, msg string) {
	s.render(w, r, "error", code, page{Title: title, Error: msg, Data: backFrom(r)})
}
