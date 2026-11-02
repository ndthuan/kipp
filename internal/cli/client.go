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
	// errRefused is a 401 or 403 from the server.
	errRefused = errors.New("the server refused the token")
	// errRedirect is a redirect, which is never followed: it would carry
	// the token to wherever it leads, and a 301 turns a POST into a GET.
	errRedirect = errors.New("the server answers with a redirect")
	// errNoAnswer is a request that got no answer, or a 5xx: what a server
	// that is restarting gives, and may not give the next time.
	errNoAnswer = errors.New("no answer from the server")
)

func noRedirect(req *http.Request, _ []*http.Request) error {
	return fmt.Errorf("%w to %s; if that is the server, give it as --url", errRedirect, req.URL)
}

// client talks to a running server as one token.
type client struct {
	url, token string
	http       *http.Client
}

// clientFlags adds --url and --token to a command and returns what
// resolves them, once the flags are parsed, into a client.
func clientFlags(fs *flag.FlagSet) func() (*client, error) {
	url := fs.String("url", "", "server URL (default $KIPP_URL, then "+defaultURL+")")
	token := fs.String("token", "", "API token (default $KIPP_TOKEN, then the file $XDG_CONFIG_HOME/kipp/token);\nan argument shows in the process list, so prefer either of those")
	return func() (*client, error) {
		tok, err := resolveToken(*token)
		if err != nil {
			return nil, err
		}
		base := cmp.Or(*url, os.Getenv("KIPP_URL"), defaultURL)
		// No request waits on a build, so one this slow has hung.
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

// readTokenFile refuses a file its owner is not alone in reading: a token
// does whatever its user may.
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

// do sends one request as the token to an API path (without its prefix) and
// returns the answer when it is a 2xx; anything else is an error carrying
// the server's own message. One worth sending again is an errNoAnswer.
func (c *client) do(method, path string, body io.Reader) (*http.Response, []byte, error) {
	var sent []byte
	if body != nil {
		var err error
		if sent, err = io.ReadAll(body); err != nil {
			return nil, nil, err
		}
	}
	res, data, err := c.send(method, api.PrefixV1+path, sent)
	if err != nil || res.StatusCode < 300 {
		return res, data, err
	}
	return nil, nil, refusal(res, data)
}

// send makes one request and returns the answer whatever its status; the
// error is for a request that got none.
func (c *client) send(method, path string, body []byte) (*http.Response, []byte, error) {
	var sent io.Reader
	if body != nil {
		sent = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.url+path, sent)
	if err != nil {
		return nil, nil, err
	}
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

// refusal is the error for an answer that is not a 2xx: the server's own
// message (a problem's detail) when it sent one, the status line when not.
func refusal(res *http.Response, data []byte) error {
	var e struct {
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(data, &e)
	if e.Detail == "" {
		e.Detail = res.Status
	}
	switch {
	case res.StatusCode >= 500:
		return fmt.Errorf("%w: %s", errNoAnswer, e.Detail)
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %s", errRefused, e.Detail)
	}
	return errors.New(e.Detail)
}

// call sends body as JSON, when there is one, and decodes the answer into
// out, when that is asked for.
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

// chunk is one of a build's logs from offset, the offset after it, and
// whether the server says more is to come.
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
