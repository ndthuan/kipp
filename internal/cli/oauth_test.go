package cli

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestMcpOAuthNeedsMcpAndAnHttpsBaseURL(t *testing.T) {
	data, jobs, _ := dataAndJobs(t, map[string]string{"hello.yml": helloJob})
	for what, c := range map[string]struct {
		args []string
		says string
	}{
		"without --mcp":           {[]string{"--mcp-oauth", "--base-url", "https://ci.example.com"}, "--mcp-oauth needs --mcp"},
		"without --base-url":      {[]string{"--mcp", "--mcp-oauth"}, "--mcp-oauth needs --base-url"},
		"a plain http base URL":   {[]string{"--mcp", "--mcp-oauth", "--base-url", "http://ci.example.com"}, "https://"},
		"a base URL with a path":  {[]string{"--mcp", "--mcp-oauth", "--base-url", "https://ci.example.com/kipp"}, "no path"},
		"a base URL on a network": {[]string{"--mcp", "--mcp-oauth", "--base-url", "http://192.168.0.5:8085"}, "https://"},
	} {
		var out, errOut syncBuffer
		args := append([]string{"--data", data, "--jobs", jobs, "--listen", "127.0.0.1:0"}, c.args...)
		eq(t, what, serve(soon(t), args, &out, &errOut), 2)
		contains(t, errOut.String(), c.says)
	}
}

func TestMcpOAuthStartsWithAnHttpsOrLocalBaseURL(t *testing.T) {
	for _, base := range []string{"https://ci.example.com", "http://localhost:8085", "http://127.0.0.1:8085"} {
		s := &served{t: t, stderr: &syncBuffer{}, args: []string{"--mcp", "--mcp-oauth", "--base-url", base}}
		s.data, s.jobs, s.token = dataAndJobs(t, map[string]string{"hello.yml": helloJob})
		s.serve()
		res, body := s.getNoFollow("/.well-known/oauth-authorization-server")
		eq(t, base+" status", res.StatusCode, http.StatusOK)
		contains(t, body, `"issuer":"`+strings.TrimSuffix(base, "/")+`"`)
	}
}

// get is a GET that follows no redirect.
func (s *served) getNoFollow(path string) (*http.Response, string) {
	s.t.Helper()
	res, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Get(s.url + path)
	must(s.t, err)
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	must(s.t, err)
	return res, string(b)
}

// Without --mcp-oauth, none of the routes is there: each answers as a path
// nobody serves, and /mcp does not point to any metadata.
func TestTheOAuthRoutesAreNotThereWithoutTheFlag(t *testing.T) {
	for _, flags := range [][]string{nil, {"--mcp"}} {
		s := &served{t: t, stderr: &syncBuffer{}, args: flags}
		s.data, s.jobs, s.token = dataAndJobs(t, map[string]string{"hello.yml": helloJob})
		s.serve()
		unknown, _ := s.getNoFollow("/nothing-here")
		for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp",
			"/.well-known/oauth-authorization-server", "/oauth/authorize", "/oauth/token", "/oauth/register", "/oauth/revoke"} {
			res, body := s.getNoFollow(p)
			eq(t, p+" status", res.StatusCode, unknown.StatusCode)
			eq(t, p+" location", res.Header.Get("Location"), strings.Replace(unknown.Header.Get("Location"), url.QueryEscape("/nothing-here"), url.QueryEscape(p), 1))
			lacks(t, body, "issuer", "client_id")
			post, _ := s.postJSON(p, "")
			eq(t, p+" POST", post.StatusCode, http.StatusSeeOther)
		}
		res, _ := s.postMCP("")
		eq(t, "challenge", res.Header.Get("WWW-Authenticate"), map[bool]string{true: "Bearer"}[len(flags) > 0])
	}
}

func TestMcpOAuthPointsAUnauthorizedCallToTheResourceMetadata(t *testing.T) {
	s := &served{t: t, stderr: &syncBuffer{}, args: []string{"--mcp", "--mcp-oauth", "--base-url", "https://ci.example.com"}}
	s.data, s.jobs, s.token = dataAndJobs(t, map[string]string{"hello.yml": helloJob})
	s.serve()
	res, _ := s.postMCP("")
	eq(t, "status", res.StatusCode, http.StatusUnauthorized)
	eq(t, "challenge", res.Header.Get("WWW-Authenticate"), `Bearer resource_metadata="https://ci.example.com/.well-known/oauth-protected-resource/mcp"`)
	// An API token still works, and an OAuth-shaped token nobody holds does not.
	res, body := s.postMCP(s.token)
	eq(t, "api token", res.StatusCode, http.StatusOK)
	contains(t, body, "run_build")
	res, _ = s.postMCP("kippo_nobody")
	eq(t, "unknown oauth token", res.StatusCode, http.StatusUnauthorized)
	// The REST API takes no OAuth token.
	req, err := http.NewRequest(http.MethodGet, s.url+"/api/v1/jobs", nil)
	must(t, err)
	req.Header.Set("Authorization", "Bearer kippo_nobody")
	r, err := http.DefaultClient.Do(req)
	must(t, err)
	_ = r.Body.Close()
	eq(t, "api with an oauth token", r.StatusCode, http.StatusUnauthorized)
}
