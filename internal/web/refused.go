package web

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/ndthuan/kipp/internal/api"
)

type nameRule struct {
	Pattern, Title string
	Max            int
}

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

func nameRuleOf(name string) (nameRule, error) {
	if r, ok := nameRules[name]; ok {
		return r, nil
	}
	return nameRule{}, errors.New("no name rule " + name)
}

func (s *Site) refusedAs(w http.ResponseWriter, r *http.Request, err error, show func(code int, msg string)) {
	if !errors.Is(err, api.ErrBadInput) && !errors.Is(err, api.ErrConflict) {
		s.fail(w, r, err)
		return
	}
	show(refusal(w, err))
}

type errorData struct{ Back, Label string }

func backFrom(r *http.Request) errorData {
	ref, err := url.Parse(r.Referer())
	if err == nil && (ref.Scheme == "http" || ref.Scheme == "https") && ref.Host == r.Host {
		if p := ref.EscapedPath(); localPath(p) == p && p != "" {
			return errorData{Back: p, Label: "Back"}
		}
	}
	return errorData{Back: "/", Label: "Back to the jobs"}
}

func (s *Site) errorPage(w http.ResponseWriter, r *http.Request, code int, title, msg string) {
	s.render(w, r, "error", code, page{Title: title, Error: msg, Data: backFrom(r)})
}
