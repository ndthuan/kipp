package docker

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// Client is one engine.
type Client struct {
	t    *transport
	http *http.Client
}

// Dial reaches the engine at url: unix:///path/to/docker.sock,
// tcp://host:port with TLS, or ssh://user@host[:port][?socket=/path].
func Dial(ctx context.Context, url string, o Options) (*Client, error) {
	t, err := dial(ctx, url, o)
	if err != nil {
		return nil, err
	}
	return &Client{t: t, http: &http.Client{Transport: t.rt}}, nil
}

// Close ends the connections to the engine.
func (c *Client) Close() error { return c.t.close() }

// APIError is an answer of the engine that is not a success.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("the engine answered %d: %s", e.Status, e.Message)
}

// IsNotFound reports whether err is the engine saying there is no such
// thing.
func IsNotFound(err error) bool {
	ae, ok := errors.AsType[*APIError](err)
	return ok && ae.Status == http.StatusNotFound
}

// do makes one call. The answer is the caller's to close.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, body io.Reader, contentType string) (*http.Response, error) {
	u := c.t.base + "/" + APIVersion + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	defer func() { _ = resp.Body.Close() }()
	var msg struct{ Message string }
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if json.Unmarshal(data, &msg) != nil || msg.Message == "" {
		msg.Message = strings.TrimSpace(string(data))
	}
	return nil, &APIError{Status: resp.StatusCode, Message: msg.Message}
}

// call makes a call with in as its JSON body (none when nil) and decodes
// the answer into out (nothing when nil).
func (c *Client) call(ctx context.Context, method, path string, q url.Values, in, out any) error {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ct = bytes.NewReader(b), "application/json"
	}
	resp, err := c.do(ctx, method, path, q, body, ct)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Version is what an engine says it is.
type Version struct {
	Version, Os, Arch string
}

// Version asks the engine what it is: the probe of whether it is there.
func (c *Client) Version(ctx context.Context) (Version, error) {
	var v Version
	err := c.call(ctx, http.MethodGet, "/version", nil, nil, &v)
	return v, err
}

// Pull fetches the image when the engine does not have it, saying what it
// does to out, a line for each step of each layer.
func (c *Client) Pull(ctx context.Context, image string, out io.Writer) error {
	err := c.call(ctx, http.MethodGet, "/images/"+url.PathEscape(image)+"/json", nil, nil, nil)
	if err == nil || !IsNotFound(err) {
		return err
	}
	_, _ = fmt.Fprintf(out, "kipp: pulling %s\n", image)
	resp, err := c.do(ctx, http.MethodPost, "/images/create", url.Values{"fromImage": {withTag(image)}}, nil, "")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return progress(resp.Body, out)
}

// withTag is image with :latest when it names neither a tag nor a digest:
// a pull with neither fetches every tag of the image.
func withTag(image string) string {
	if strings.Contains(image, "@") {
		return image
	}
	last := image[strings.LastIndex(image, "/")+1:]
	if strings.Contains(last, ":") {
		return image
	}
	return image + ":latest"
}

// progress renders the JSON lines of a pull, one line a status that
// changed; an error line is the pull's error.
func progress(r io.Reader, out io.Writer) error {
	dec := json.NewDecoder(r)
	last := map[string]string{}
	for {
		var m struct{ Status, ID, Error string }
		if err := dec.Decode(&m); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		if m.Error != "" {
			return errors.New(m.Error)
		}
		if m.Status == "" || last[m.ID] == m.Status {
			continue
		}
		last[m.ID] = m.Status
		line := m.Status
		if m.ID != "" {
			line = m.ID + ": " + line
		}
		_, _ = fmt.Fprintf(out, "kipp: %s\n", line)
	}
}

// CreateSpec is a container kipp starts: no ports, no mounts and no
// privileges are offered, so none can be asked for.
type CreateSpec struct {
	Image      string
	Cmd        []string
	Env        []string
	Labels     map[string]string
	WorkingDir string
}

// Create makes the container and returns its id.
func (c *Client) Create(ctx context.Context, s CreateSpec) (string, error) {
	body := map[string]any{
		"Image": s.Image, "Cmd": s.Cmd, "Env": s.Env, "Labels": s.Labels, "WorkingDir": s.WorkingDir,
		"HostConfig": map[string]any{"Init": true},
	}
	var out struct {
		ID string `json:"Id"`
	}
	err := c.call(ctx, http.MethodPost, "/containers/create", nil, body, &out)
	return out.ID, err
}

// PutFile writes one file into the container's filesystem at name, a
// path from its root, with the directories above it. The engine refuses an
// archive into a directory that is not there, so the archive goes in at
// the root and carries them.
func (c *Client) PutFile(ctx context.Context, id, name string, mode fs.FileMode, data []byte) error {
	name = strings.TrimPrefix(path.Clean("/"+name), "/")
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	now := time.Now()
	dirs := strings.Split(name, "/")
	for i := 1; i < len(dirs); i++ {
		hd := &tar.Header{Name: strings.Join(dirs[:i], "/") + "/", Mode: 0o755, Typeflag: tar.TypeDir, ModTime: now}
		if err := tw.WriteHeader(hd); err != nil {
			return err
		}
	}
	hd := &tar.Header{Name: name, Mode: int64(mode.Perm()), Size: int64(len(data)), Typeflag: tar.TypeReg, ModTime: now}
	if err := tw.WriteHeader(hd); err != nil {
		return err
	}
	if _, err := tw.Write(data); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPut, "/containers/"+url.PathEscape(id)+"/archive", url.Values{"path": {"/"}}, &buf, "application/x-tar")
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// Start starts the container.
func (c *Client) Start(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil, nil, nil)
}

// Kill kills the container; one that is not running is no error.
func (c *Client) Kill(ctx context.Context, id string) error {
	err := c.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/kill", nil, nil, nil)
	if ae, ok := errors.AsType[*APIError](err); ok && (ae.Status == http.StatusConflict || ae.Status == http.StatusNotFound) {
		return nil
	}
	return err
}

// Remove removes the container, running or not; one that is gone is no
// error.
func (c *Client) Remove(ctx context.Context, id string) error {
	err := c.call(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id), url.Values{"force": {"1"}}, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// Wait blocks until the container stops, and is its exit code.
func (c *Client) Wait(ctx context.Context, id string) (int, error) {
	var out struct {
		StatusCode int
		Error      *struct{ Message string }
	}
	if err := c.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/wait", nil, nil, &out); err != nil {
		return -1, err
	}
	if out.Error != nil && out.Error.Message != "" {
		return out.StatusCode, errors.New(out.Error.Message)
	}
	return out.StatusCode, nil
}

// Logs writes what the container printed, stdout and stderr, to out: the
// agent's own words, which say why a container that never connected did
// not.
func (c *Client) Logs(ctx context.Context, id string, out io.Writer) error {
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/logs", url.Values{"stdout": {"1"}, "stderr": {"1"}, "tail": {"200"}}, nil, "")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return demux(resp.Body, out)
}

// demux copies the engine's multiplexed stream, frames of an 8-byte
// header and a payload, into out.
func demux(r io.Reader, out io.Writer) error {
	br := bufio.NewReader(r)
	var hd [8]byte
	for {
		if _, err := io.ReadFull(br, hd[:]); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		if _, err := io.CopyN(out, br, int64(binary.BigEndian.Uint32(hd[4:]))); err != nil {
			return err
		}
	}
}

// Container is one container as a list shows it.
type Container struct {
	ID      string
	Labels  map[string]string
	Running bool
}

// List is the containers, running or not, that carry label (key=value).
func (c *Client) List(ctx context.Context, label string) ([]Container, error) {
	filters, err := json.Marshal(map[string][]string{"label": {label}})
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID     string `json:"Id"`
		Labels map[string]string
		State  string
	}
	if err := c.call(ctx, http.MethodGet, "/containers/json", url.Values{"all": {"1"}, "filters": {string(filters)}}, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(raw))
	for _, r := range raw {
		out = append(out, Container{ID: r.ID, Labels: r.Labels, Running: r.State == "running"})
	}
	return out, nil
}
