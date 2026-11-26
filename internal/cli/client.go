package cli

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ndthuan/kipp/internal/api"
)

const defaultURL = "http://127.0.0.1:8085"

var (
	errRefused  = errors.New("the server refused the token")
	errRedirect = errors.New("the server answers with a redirect")
	errNoAnswer = errors.New("no answer from the server")
)

func noRedirect(req *http.Request, _ []*http.Request) error {
	return fmt.Errorf("%w to %s; if that is the server, give it as --url", errRedirect, req.URL)
}

type client struct {
	url, token string
	http       *http.Client
}

func clientFlags(fs *flag.FlagSet) func() (*client, error) {
	url := fs.String("url", "", "server URL (default $KIPP_URL, then "+defaultURL+")")
	token := fs.String("token", "", "API token (default $KIPP_TOKEN, then the file $XDG_CONFIG_HOME/kipp/token);\nan argument shows in the process list, so prefer either of those")
	return func() (*client, error) {
		tok, err := resolveToken(*token)
		if err != nil {
			return nil, err
		}
		base := cmp.Or(*url, os.Getenv("KIPP_URL"), defaultURL)
		return &client{url: strings.TrimRight(base, "/"), token: tok, http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: noRedirect}}, nil
	}
}

func resolveToken(given string) (string, error) {
	if tok := cmp.Or(given, os.Getenv("KIPP_TOKEN")); tok != "" {
		return tok, nil
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", refused("no token: give --token or set KIPP_TOKEN")
		}
		dir = filepath.Join(home, ".config")
	}
	return readTokenFile(filepath.Join(dir, "kipp", "token"))
}

func readTokenFile(path string) (string, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", refused("no token: give --token, set KIPP_TOKEN or write it to " + path)
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return "", refused(fmt.Sprintf("%s: mode %v lets group or others read the token; chmod 600 it", path, perm))
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", refused(path + " is empty")
	}
	return tok, nil
}

func (c *client) do(method, path string, body io.Reader) (*http.Response, []byte, error) {
	var sent []byte
	if body != nil {
		var err error
		if sent, err = io.ReadAll(body); err != nil {
			return nil, nil, err
		}
	}
	return c.doWith(method, path, sent, nil)
}

func (c *client) doWith(method, path string, body []byte, hdr http.Header) (*http.Response, []byte, error) {
	res, data, err := c.sendWith(method, api.PrefixV1+path, body, hdr)
	if err != nil || res.StatusCode < 300 {
		return res, data, err
	}
	return nil, nil, refusal(res, data)
}

func (c *client) sendWith(method, path string, body []byte, hdr http.Header) (*http.Response, []byte, error) {
	var sent io.Reader
	if body != nil {
		sent = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.url+path, sent)
	if err != nil {
		return nil, nil, err
	}
	maps.Copy(req.Header, hdr)
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if errors.Is(err, errRedirect) {
		return nil, nil, err
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", errNoAnswer, err)
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", errNoAnswer, err)
	}
	return res, data, nil
}

type answered struct {
	status int
	msg    string
}

func (e *answered) Error() string { return e.msg }

func answeredWith(err error, status int) bool {
	a, ok := errors.AsType[*answered](err)
	return ok && a.status == status
}

func refusal(res *http.Response, data []byte) error {
	var e struct {
		Detail string   `json:"detail"`
		Errors []string `json:"errors"`
	}
	_ = json.Unmarshal(data, &e)
	if len(e.Errors) > 0 {
		e.Detail = "refused:\n  " + strings.Join(e.Errors, "\n  ")
	}
	if e.Detail == "" {
		e.Detail = res.Status
	}
	switch {
	case res.StatusCode >= 500:
		return fmt.Errorf("%w: %s", errNoAnswer, e.Detail)
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %s", errRefused, e.Detail)
	}
	return &answered{status: res.StatusCode, msg: e.Detail}
}

func (c *client) call(method, path string, body, out any) error {
	var sent io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		sent = bytes.NewReader(data)
	}
	_, data, err := c.do(method, path, sent)
	if err != nil || out == nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func (c *client) chunk(build int64, log string, offset int64) (data []byte, next int64, more bool, err error) {
	res, data, err := c.do(http.MethodGet, fmt.Sprintf("/builds/%d/stages/%s/log?offset=%d", build, log, offset), nil)
	if err != nil {
		return nil, offset, false, err
	}
	if next, err = strconv.ParseInt(res.Header.Get("X-Kipp-Offset"), 10, 64); err != nil {
		return nil, offset, false, fmt.Errorf("the server gave no log offset: %w", err)
	}
	return data, next, res.Header.Get("X-Kipp-More") == "true", nil
}
