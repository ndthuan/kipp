package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// releaseAPI and releasePath are the one lookup kipp makes: the latest
	// release that is not a pre-release.
	releaseAPI  = "https://api.github.com"
	releasePath = "/repos/ndthuan/kipp/releases/latest"
	// SigFile is the asset that makes a release one kipp may upgrade to.
	SigFile = "SHA256SUMS.sig"
	// CacheFile is the answer kept in the data directory, so that a restart
	// does not ask again inside the interval.
	CacheFile = "update-check.json"

	// Interval is how often the server asks.
	Interval       = 12 * time.Hour
	defaultTimeout = 15 * time.Second
	maxReply       = 1 << 20
	maxRedirects   = 5
)

// defaultHosts are the hosts a lookup, and the download after it, may reach
// or be redirected to.
var defaultHosts = []string{"api.github.com", "github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com"}

// Options are what a Checker needs. A test gives Base, Hosts and Insecure;
// the server takes them from Production, which has no way to change them.
type Options struct {
	// Current is the running version; one that is not a release's makes a
	// Checker that never asks.
	Current string
	// Dir is the data directory, where the answer is kept; empty keeps it
	// in memory only.
	Dir string
	// Base is the API's URL, Hosts the hostnames a request and its
	// redirects may reach, and Insecure lets plain HTTP through.
	Base     string
	Hosts    []string
	Insecure bool
	Interval time.Duration
	Timeout  time.Duration
	Now      func() time.Time
	Log      *slog.Logger
}

// Production is the options of the server's own checker.
func Production(current, dir string, log *slog.Logger) Options {
	base, hosts, insecure := target()
	return Options{Current: current, Dir: dir, Base: base, Hosts: hosts, Insecure: insecure, Log: log}
}

// Status is what the checker knows. Latest is empty unless a release newer
// than the running one was found.
type Status struct {
	Latest   string
	NotesURL string
	// Upgradable is true when that release carries the signature file.
	Upgradable bool
	// CheckedAt is when the answer was had, zero before any.
	CheckedAt time.Time
}

// found is a release as kept: the newest, whether or not it is newer than
// the running kipp.
type found struct {
	Version   string    `json:"version"`
	NotesURL  string    `json:"notes_url"`
	Signed    bool      `json:"signed"`
	CheckedAt time.Time `json:"checked_at"`
}

// Checker asks for the latest release now and then.
type Checker struct {
	o      Options
	client *http.Client

	mu  sync.Mutex
	got found
}

// New makes a Checker and reads the answer kept in the data directory.
func New(o Options) *Checker {
	if o.Interval <= 0 {
		o.Interval = Interval
	}
	if o.Timeout <= 0 {
		o.Timeout = defaultTimeout
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	c := &Checker{o: o}
	c.client = &http.Client{Timeout: o.Timeout, CheckRedirect: c.redirect}
	c.got = c.load()
	return c
}

// Status is the answer as of now.
func (c *Checker) Status() Status {
	c.mu.Lock()
	g := c.got
	c.mu.Unlock()
	st := Status{CheckedAt: g.CheckedAt}
	if Newer(g.Version, c.o.Current) {
		st.Latest, st.NotesURL, st.Upgradable = g.Version, g.NotesURL, g.Signed
	}
	return st
}

// Run asks at once, unless the kept answer is younger than the interval,
// and then every interval, until ctx ends.
func (c *Checker) Run(ctx context.Context) {
	if !Release(c.o.Current) {
		return
	}
	wait := time.Duration(0)
	c.mu.Lock()
	if age := c.o.Now().Sub(c.got.CheckedAt); !c.got.CheckedAt.IsZero() && age >= 0 && age < c.o.Interval {
		wait = c.o.Interval - age
	}
	c.mu.Unlock()
	for {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		if err := c.Check(ctx); err != nil {
			c.o.Log.Debug("update check", "err", err)
		}
		wait = c.o.Interval
	}
}

// latestRelease is the part of the API's answer that is read.
type latestRelease struct {
	Tag        string `json:"tag_name"`
	HTMLURL    string `json:"html_url"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
	} `json:"assets"`
}

// Check asks once. On any failure the answer kept stays as it was.
func (c *Checker) Check(ctx context.Context) error {
	if !Release(c.o.Current) {
		return nil
	}
	rel, err := c.fetch(ctx)
	if err != nil {
		return err
	}
	if rel.Draft || rel.Prerelease || !Release(rel.Tag) {
		return fmt.Errorf("the latest release %q is not a stable vX.Y.Z", rel.Tag)
	}
	g := found{Version: rel.Tag, NotesURL: rel.HTMLURL, CheckedAt: c.o.Now().UTC()}
	for _, a := range rel.Assets {
		if a.Name == SigFile {
			g.Signed = true
		}
	}
	c.mu.Lock()
	c.got = g
	c.mu.Unlock()
	c.save(g)
	return nil
}

func (c *Checker) fetch(ctx context.Context) (*latestRelease, error) {
	u, err := c.allowed(c.o.Base + releasePath)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "kipp/"+c.o.Current)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the release lookup answered %d", resp.StatusCode)
	}
	var rel latestRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReply)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("the release lookup: %w", err)
	}
	return &rel, nil
}

// allowed parses raw and holds it to the scheme and the hosts.
func (c *Checker) allowed(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" && (!c.o.Insecure || u.Scheme != "http") {
		return nil, fmt.Errorf("%s is not an https URL", u.Redacted())
	}
	if !slices.Contains(c.o.Hosts, u.Hostname()) {
		return nil, fmt.Errorf("%s is not a host releases come from", u.Hostname())
	}
	return u, nil
}

func (c *Checker) redirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return errors.New("too many redirects")
	}
	_, err := c.allowed(req.URL.String())
	return err
}

func (c *Checker) path() string { return filepath.Join(c.o.Dir, CacheFile) }

// load reads the kept answer; one that is unreadable, or dated in the
// future, is none.
func (c *Checker) load() found {
	if c.o.Dir == "" {
		return found{}
	}
	b, err := os.ReadFile(c.path())
	if err != nil {
		return found{}
	}
	var g found
	if json.Unmarshal(b, &g) != nil || g.CheckedAt.After(c.o.Now()) || !Release(g.Version) {
		return found{}
	}
	return g
}

// save keeps g by an atomic rename; a failure is only worth a debug line,
// the next start asks again.
func (c *Checker) save(g found) {
	if c.o.Dir == "" {
		return
	}
	b, err := json.Marshal(g)
	if err == nil {
		err = writeAtomic(c.path(), append(b, '\n'))
	}
	if err != nil {
		c.o.Log.Debug("update check: keeping the answer", "err", err)
	}
}

func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), strings.TrimSuffix(filepath.Base(path), ".json")+"-*.tmp")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}
