package cli

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ndthuan/kipp/internal/mcp"
)

const mcpCall = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

func (s *served) postMCP(token string) (*http.Response, string) {
	s.t.Helper()
	return s.postJSON(mcp.Path, token)
}

// postJSON posts a JSON-RPC call without following a redirect.
func (s *served) postJSON(path, token string) (*http.Response, string) {
	s.t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.url+path, strings.NewReader(mcpCall))
	must(s.t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	must(s.t, err)
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	must(s.t, err)
	return res, string(body)
}

// Without --mcp the path is like any other nobody serves: the pages answer
// it as they answer every path that is not there, and nothing MCP is run.
func TestMcpIsNotThereWithoutTheFlag(t *testing.T) {
	s := start(t, map[string]string{"hello.yml": helloJob})
	for _, token := range []string{"", s.token} {
		res, body := s.postMCP(token)
		lacks(t, body, "jsonrpc", "tools")
		unknown, _ := s.postJSON("/nothing-here", token)
		eq(t, "/mcp answers as an unknown path", res.StatusCode, unknown.StatusCode)
		eq(t, "/mcp redirects as an unknown path", res.Header.Get("Location"), unknown.Header.Get("Location"))
		eq(t, "no challenge", res.Header.Get("WWW-Authenticate"), "")
	}
}

func TestMcpServesATokensUserWithTheFlag(t *testing.T) {
	s := &served{t: t, stderr: &syncBuffer{}, args: []string{"--mcp", "--base-url", "https://ci.example.com"}}
	s.data, s.jobs, s.token = dataAndJobs(t, map[string]string{"hello.yml": helloJob})
	s.serve()

	res, _ := s.postMCP("")
	eq(t, "status without a token", res.StatusCode, http.StatusUnauthorized)
	eq(t, "challenge", res.Header.Get("WWW-Authenticate"), "Bearer")
	res, _ = s.postMCP("kipp_not-a-token")
	eq(t, "status with a garbage token", res.StatusCode, http.StatusUnauthorized)

	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	httpClient := &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+s.token)
		return http.DefaultTransport.RoundTrip(r)
	})}
	session, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: s.url + mcp.Path, HTTPClient: httpClient, DisableStandaloneSSE: true}, nil)
	must(t, err)
	defer func() { _ = session.Close() }()
	tools, err := session.ListTools(ctx, nil)
	must(t, err)
	eq(t, "tools", len(tools.Tools), 12)
	run, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "run_build", Arguments: map[string]any{"job": "hello"}})
	must(t, err)
	eq(t, "run_build failed", run.IsError, false)
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
