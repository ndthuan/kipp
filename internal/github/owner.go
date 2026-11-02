package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// OwnerRepo is a repo of an owner as its listing says it.
type OwnerRepo struct {
	FullName, Name, DefaultBranch string
	Archived, Fork, Private       bool
}

// Listing is the repos of an owner, and whether they are all of them the
// token can reach: another user's account lists its public repos only.
type Listing struct {
	Repos      []OwnerRepo
	PublicOnly bool
}

// The kinds of owner GitHub has.
const (
	kindUser = "User"
	kindOrg  = "Organization"
)

// OwnerRepos lists the repos of the user or organization called login:
// an organization's through its own listing, the token's own user's
// through the listing of what the token owns (private repos too), and any
// other user's through their public ones, which Listing says.
func (c *Client) OwnerRepos(ctx context.Context, login string) (*Listing, error) {
	var who struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	}
	if _, err := c.get(ctx, "/users/"+url.PathEscape(login), nil, &who); err != nil {
		return nil, err
	}
	out := &Listing{}
	path, q := "/users/"+url.PathEscape(login)+"/repos", url.Values{"type": {"owner"}}
	switch who.Type {
	case kindOrg:
		path, q = "/orgs/"+url.PathEscape(login)+"/repos", url.Values{"type": {"all"}}
	case kindUser:
		var me struct {
			Login string `json:"login"`
		}
		if _, err := c.get(ctx, "/user", nil, &me); err != nil {
			return nil, err
		}
		if strings.EqualFold(me.Login, login) {
			path, q = "/user/repos", url.Values{"affiliation": {"owner"}}
		} else {
			out.PublicOnly = true
		}
	}
	err := c.pages(ctx, path, q, func(raw json.RawMessage) error {
		var page []repoJSON
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		for _, r := range page {
			// The token's own listing is of what it owns, never of the
			// organizations it belongs to; a login is one owner.
			if r.Owner.Login != "" && !strings.EqualFold(r.Owner.Login, login) {
				continue
			}
			out.Repos = append(out.Repos, r.repo())
		}
		return nil
	})
	return out, err
}

// repoJSON is a repo as GitHub's listings give it.
type repoJSON struct {
	FullName      string `json:"full_name"`
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch"`
	Archived      bool   `json:"archived"`
	Fork          bool   `json:"fork"`
	Private       bool   `json:"private"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
}

func (r repoJSON) repo() OwnerRepo {
	return OwnerRepo{FullName: r.FullName, Name: r.Name, DefaultBranch: r.DefaultBranch, Archived: r.Archived, Fork: r.Fork, Private: r.Private}
}

// InstallationRepos lists the repos an App's installation can reach: all
// the account's, or the ones it was installed on. An installation token has
// no user, so the listings of a user's or an organization's repos are not
// its to call.
func (c *Client) InstallationRepos(ctx context.Context) (*Listing, error) {
	out := &Listing{}
	err := c.pages(ctx, "/installation/repositories", nil, func(raw json.RawMessage) error {
		var page struct {
			Repositories []repoJSON `json:"repositories"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		for _, r := range page.Repositories {
			out.Repos = append(out.Repos, r.repo())
		}
		return nil
	})
	return out, err
}

// Hook is a webhook of a repo as GitHub lists it; the secret is never
// handed back.
type Hook struct {
	ID     int64
	URL    string
	Events []string
	Active bool
	JSON   bool // delivered as application/json
}

// HookSpec is the webhook kipp wants on a repo.
type HookSpec struct {
	URL, Secret string
	Events      []string
}

type hookConfig struct {
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
	Secret      string `json:"secret,omitempty"`
	InsecureSSL string `json:"insecure_ssl"`
}

type hookBody struct {
	Name   string     `json:"name,omitempty"`
	Active bool       `json:"active"`
	Events []string   `json:"events"`
	Config hookConfig `json:"config"`
}

// Hooks are the repo's webhooks. A token without the Webhooks permission
// is answered 404 or 403, as an *Error.
func (c *Client) Hooks(ctx context.Context, repo string) ([]Hook, error) {
	var out []Hook
	err := c.pages(ctx, "/repos/"+repo+"/hooks", nil, func(raw json.RawMessage) error {
		var page []struct {
			ID     int64    `json:"id"`
			Active bool     `json:"active"`
			Events []string `json:"events"`
			Config struct {
				URL         string `json:"url"`
				ContentType string `json:"content_type"`
			} `json:"config"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		for _, h := range page {
			out = append(out, Hook{ID: h.ID, URL: h.Config.URL, Events: h.Events, Active: h.Active, JSON: h.Config.ContentType == "json"})
		}
		return nil
	})
	return out, err
}

func (s HookSpec) body(create bool) hookBody {
	b := hookBody{Active: true, Events: s.Events, Config: hookConfig{URL: s.URL, ContentType: "json", Secret: s.Secret, InsecureSSL: "0"}}
	if create {
		b.Name = "web"
	}
	return b
}

// CreateHook adds the webhook to the repo.
func (c *Client) CreateHook(ctx context.Context, repo string, s HookSpec) error {
	path := "/repos/" + repo + "/hooks"
	_, _, err := c.fetch(ctx, http.MethodPost, "POST "+path, c.API+path, s.body(true), nil)
	return err
}

// UpdateHook makes the repo's webhook id what s says.
func (c *Client) UpdateHook(ctx context.Context, repo string, id int64, s HookSpec) error {
	path := "/repos/" + repo + "/hooks/" + strconv.FormatInt(id, 10)
	_, _, err := c.fetch(ctx, http.MethodPatch, "PATCH "+path, c.API+path, s.body(false), nil)
	return err
}
