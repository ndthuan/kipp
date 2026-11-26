package github

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// WebBase is where GitHub's pages are, for the API at api: github.com for
// the default, and for GitHub Enterprise the host its API path hangs off.
func WebBase(api string) string {
	api = strings.TrimRight(api, "/")
	if api == "" || api == DefaultAPI {
		return "https://github.com"
	}
	return strings.TrimSuffix(api, "/api/v3")
}

// ManifestApp is a GitHub App as GitHub hands it over when the manifest
// flow ends: the one time anything shows its private key and secrets.
type ManifestApp struct {
	ID            int64  `json:"id"`
	Slug          string `json:"slug"`
	HTMLURL       string `json:"html_url"`
	PEM           string `json:"pem"`
	WebhookSecret string `json:"webhook_secret"`
}

// ConvertManifest trades the code GitHub redirected back with for the App
// it made from a manifest. The code works once, within an hour.
func ConvertManifest(ctx context.Context, api, code string) (*ManifestApp, error) {
	c := New(api, "")
	var out ManifestApp
	path := "/app-manifests/" + url.PathEscape(code) + "/conversions"
	if _, _, err := c.fetch(ctx, http.MethodPost, "POST /app-manifests/{code}/conversions", c.API+path, struct{}{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
