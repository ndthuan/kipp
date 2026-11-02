// Package github is the small slice of GitHub's REST API a multibranch job
// scans with: the repo, its branches, its open pull requests, and whether a
// file is in a commit; and the one thing it says back, the status of a commit. Every error names the call that failed and never the
// token it was made with.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// DefaultAPI is where GitHub's API is.
const DefaultAPI = "https://api.github.com"

const (
	perPage    = 100
	maxPages   = 100 // 10000 branches or pull requests; more is a loop
	maxBody    = 8 << 20
	maxMessage = 200
)

// Client calls one GitHub API with one token.
type Client struct {
	API   string
	Token string
	HTTP  *http.Client
}

// New is a client of the API at api, which is DefaultAPI when empty.
func New(api, token string) *Client {
	if api == "" {
		api = DefaultAPI
	}
	return &Client{API: strings.TrimRight(api, "/"), Token: token, HTTP: &http.Client{Timeout: 30 * time.Second, CheckRedirect: sameHostRedirect}}
}

// sameHostRedirect keeps a call, with its token and its body, on the host it
// was made to: a redirect elsewhere is handed back as the answer.
func sameHostRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Host != via[0].URL.Host || req.URL.Scheme != via[0].URL.Scheme {
		return http.ErrUseLastResponse
	}
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	return nil
}

// Branch is a branch of the repo and the commit it is at.
type Branch struct {
	Name, SHA string
}

// Pull is an open pull request. HeadRepo is the full name of the repo its
// head is in, empty when that repo was deleted.
type Pull struct {
	Number                            int
	Title, HeadRef, HeadSHA, HeadRepo string
	Author, URL                       string
	BaseRef                           string
}

// Error is a call GitHub did not answer with what was asked.
type Error struct {
	Call   string // "GET /repos/acme/site"
	Status int    // 0 when there was no answer
	Msg    string
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("github: %s: %s", e.Call, e.Msg)
	}
	return fmt.Sprintf("github: %s: %d %s", e.Call, e.Status, e.Msg)
}

// Repo is the repo's default branch.
func (c *Client) Repo(ctx context.Context, repo string) (string, error) {
	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	if _, err := c.get(ctx, "/repos/"+repo, nil, &out); err != nil {
		return "", err
	}
	if out.DefaultBranch == "" {
		return "", &Error{Call: "GET /repos/" + repo, Msg: "the answer names no default branch"}
	}
	return out.DefaultBranch, nil
}

type branchJSON struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

// Branch is the commit one branch is at.
func (c *Client) Branch(ctx context.Context, repo, name string) (string, error) {
	var out branchJSON
	if _, err := c.get(ctx, "/repos/"+repo+"/branches/"+escapePath(name), nil, &out); err != nil {
		return "", err
	}
	return out.Commit.SHA, nil
}

// Branches are all the repo's branches.
func (c *Client) Branches(ctx context.Context, repo string) ([]Branch, error) {
	var out []Branch
	err := c.pages(ctx, "/repos/"+repo+"/branches", nil, func(raw json.RawMessage) error {
		var page []branchJSON
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		for _, b := range page {
			out = append(out, Branch{b.Name, b.Commit.SHA})
		}
		return nil
	})
	return out, err
}

// OpenPulls are the repo's open pull requests.
func (c *Client) OpenPulls(ctx context.Context, repo string) ([]Pull, error) {
	var out []Pull
	q := url.Values{"state": {"open"}}
	err := c.pages(ctx, "/repos/"+repo+"/pulls", q, func(raw json.RawMessage) error {
		var page []struct {
			Number int    `json:"number"`
			Title  string `json:"title"`
			URL    string `json:"html_url"`
			User   struct {
				Login string `json:"login"`
			} `json:"user"`
			Head struct {
				Ref  string `json:"ref"`
				SHA  string `json:"sha"`
				Repo *struct {
					FullName string `json:"full_name"`
				} `json:"repo"`
			} `json:"head"`
			Base struct {
				Ref string `json:"ref"`
			} `json:"base"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		for _, p := range page {
			pull := Pull{Number: p.Number, Title: p.Title, Author: p.User.Login, URL: p.URL, HeadRef: p.Head.Ref, HeadSHA: p.Head.SHA, BaseRef: p.Base.Ref}
			if p.Head.Repo != nil {
				pull.HeadRepo = p.Head.Repo.FullName
			}
			out = append(out, pull)
		}
		return nil
	})
	return out, err
}

// HasFile reports whether path is in the commit: the contents API answers
// 200 or 404, and anything else is an error.
func (c *Client) HasFile(ctx context.Context, repo, path, sha string) (bool, error) {
	status, err := c.get(ctx, "/repos/"+repo+"/contents/"+escapePath(path), url.Values{"ref": {sha}}, nil)
	var ge *Error
	if errors.As(err, &ge) && ge.Status == http.StatusNotFound {
		return false, nil
	}
	return status == http.StatusOK && err == nil, err
}

// The states a commit status takes, and the most GitHub keeps of its
// description.
const maxDescription = 140

var (
	statusStates = []string{"error", "failure", "pending", "success"}
	commitSHA    = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
)

// Status is what is said of a commit: a state, the context it is said
// under, a line of description and, if there is one, the page to read more
// at.
type Status struct {
	State, Context, Description, TargetURL string
}

// PostStatus says the status of the commit at sha. The state and the sha are
// checked before anything is sent, so that neither can make a call to another
// path; a description longer than GitHub keeps is cut.
func (c *Client) PostStatus(ctx context.Context, repo, sha string, s Status) error {
	path := "/repos/" + repo + "/statuses/" + sha
	call := "POST " + path
	switch {
	case !commitSHA.MatchString(sha):
		return &Error{Call: call, Msg: "not a commit"}
	case !slices.Contains(statusStates, s.State):
		return &Error{Call: call, Msg: fmt.Sprintf("state %q is not one of %s", s.State, strings.Join(statusStates, ", "))}
	case s.Context == "":
		return &Error{Call: call, Msg: "no context"}
	}
	body := struct {
		State       string  `json:"state"`
		Context     string  `json:"context"`
		Description string  `json:"description"`
		TargetURL   *string `json:"target_url,omitempty"`
	}{State: s.State, Context: s.Context, Description: cut(s.Description, maxDescription)}
	if s.TargetURL != "" {
		body.TargetURL = &s.TargetURL
	}
	_, _, err := c.fetch(ctx, http.MethodPost, call, c.API+path, body, nil)
	return err
}

// cut is s at most n bytes long, on a character.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// escapePath escapes each segment of a path and keeps the slashes.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// get calls path and decodes the answer into out, when there is one.
func (c *Client) get(ctx context.Context, path string, q url.Values, out any) (int, error) {
	u := c.API + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	status, _, err := c.fetch(ctx, http.MethodGet, "GET "+path, u, nil, out)
	return status, err
}

// pages calls path once for every page of its list, with the first page
// asked for perPage items, and hands each page's items to take. It follows
// the Link header's next only to the API's own host, so that the token
// goes nowhere else.
func (c *Client) pages(ctx context.Context, path string, q url.Values, take func(json.RawMessage) error) error {
	if q == nil {
		q = url.Values{}
	}
	q.Set("per_page", strconv.Itoa(perPage))
	next := c.API + path + "?" + q.Encode()
	for range maxPages {
		var raw json.RawMessage
		_, link, err := c.fetch(ctx, http.MethodGet, "GET "+path, next, nil, &raw)
		if err != nil {
			return err
		}
		if err := take(raw); err != nil {
			return &Error{Call: "GET " + path, Msg: "unreadable answer: " + c.redact(err.Error())}
		}
		if next = nextLink(link); next == "" {
			return nil
		}
		if !c.sameHost(next) {
			return &Error{Call: "GET " + path, Msg: "the next page is on another host: not followed"}
		}
	}
	return &Error{Call: "GET " + path, Msg: "more pages than are read"}
}

func (c *Client) sameHost(raw string) bool {
	a, err1 := url.Parse(c.API)
	b, err2 := url.Parse(raw)
	return err1 == nil && err2 == nil && a.Scheme == b.Scheme && a.Host == b.Host
}

// nextLink is the rel="next" URL of a Link header, "" when there is none.
func nextLink(header string) string {
	for part := range strings.SplitSeq(header, ",") {
		target, rest, ok := strings.Cut(strings.TrimSpace(part), ";")
		if ok && strings.Contains(rest, `rel="next"`) {
			return strings.Trim(strings.TrimSpace(target), "<>")
		}
	}
	return ""
}

// fetch makes one call, a GET or a POST of in as JSON; only the answer a
// GET or a POST is meant to get is a success. call names it in an error.
func (c *Client) fetch(ctx context.Context, method, call, u string, in, out any) (int, string, error) {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return 0, "", &Error{Call: call, Msg: "unwritable body: " + c.redact(err.Error())}
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return 0, "", &Error{Call: call, Msg: c.redact(err.Error())}
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err // the URL is in the call already
		}
		return 0, "", &Error{Call: call, Msg: c.redact(err.Error())}
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return resp.StatusCode, "", &Error{Call: call, Status: resp.StatusCode, Msg: "reading the answer: " + c.redact(err.Error())}
	}
	ok := http.StatusOK
	if method == http.MethodPost {
		ok = http.StatusCreated
	}
	if resp.StatusCode != ok {
		return resp.StatusCode, "", &Error{Call: call, Status: resp.StatusCode, Msg: c.failure(resp, answer)}
	}
	if out != nil {
		if err := json.Unmarshal(answer, out); err != nil {
			return resp.StatusCode, "", &Error{Call: call, Status: resp.StatusCode, Msg: "unreadable answer: " + c.redact(err.Error())}
		}
	}
	return resp.StatusCode, resp.Header.Get("Link"), nil
}

// failure says why GitHub refused, from the answer's message, and when a
// spent rate limit is back.
func (c *Client) failure(resp *http.Response, body []byte) string {
	if resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.StatusCode == http.StatusTooManyRequests {
		msg := "rate limit exceeded"
		if n, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			msg += ", resets at " + time.Unix(n, 0).UTC().Format(time.RFC3339)
		}
		return msg
	}
	var m struct {
		Message string `json:"message"`
	}
	text := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &m) == nil && m.Message != "" {
		text = m.Message
	}
	if text == "" {
		text = http.StatusText(resp.StatusCode)
	}
	if len(text) > maxMessage {
		text = text[:maxMessage]
	}
	return c.redact(text)
}

// redact takes the token out of text an answer or a library wrote.
func (c *Client) redact(text string) string {
	if c.Token == "" {
		return text
	}
	return strings.ReplaceAll(text, c.Token, "***")
}
