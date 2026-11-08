// Package enginetest is a fake GitHub release and a fake server for the
// tests of the upgrade engine and of whatever calls it. It is for tests only.
package enginetest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite" // the driver, no cgo

	"github.com/ndthuan/kipp/internal/update"
	"github.com/ndthuan/kipp/internal/upgrade/backup"
	"github.com/ndthuan/kipp/internal/upgrade/engine"
)

// The versions of the fixture: the server runs Running, and the release
// that the fake GitHub has is Newer.
const (
	Running = "v1.0.0"
	Newer   = "v1.2.0"
)

// Release is a fake GitHub: the latest-release lookup and the files of one
// release, signed by a key of its own.
type Release struct {
	t   *testing.T
	Srv *httptest.Server
	// Pub is the public key file the release is signed by.
	Pub []byte
	// Files are the release's files by name; a test may alter them.
	Files map[string][]byte
	// Hits counts the requests made.
	Hits atomic.Int32
	sec  update.SecretKey
}

// GoodBinary is a script that says it is kipp ver.
func GoodBinary(ver string) string {
	return "#!/bin/sh\n[ \"$1\" = version ] && echo 'kipp " + ver + "'\n"
}

// ArchiveOf is a release archive for this platform holding script as kipp.
func ArchiveOf(t *testing.T, ver, script string) []byte {
	t.Helper()
	dir := update.ArchiveDir(ver, runtime.GOOS, runtime.GOARCH)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, f := range []struct{ name, body string }{{dir + "/README.md", "hello"}, {dir + "/kipp", script}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(tw.Close(), zw.Close()); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// NewRelease serves a signed release Newer whose binary is script.
func NewRelease(t *testing.T, script string) *Release {
	t.Helper()
	pub, sec, err := update.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	r := &Release{t: t, Pub: pub.Marshal(), Files: map[string][]byte{}, sec: sec}
	r.SetArchive(ArchiveOf(t, Newer, script))
	r.Srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.Srv.Close)
	return r
}

// SetArchive replaces the archive of this platform and signs the list of
// checksums again.
func (r *Release) SetArchive(archive []byte) {
	name := update.ArchiveName(Newer, runtime.GOOS, runtime.GOARCH)
	sum := sha256.Sum256(archive)
	sums := []byte(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	r.Files[name], r.Files[update.SumsFile], r.Files[update.SigFile] = archive, sums, r.sec.Sign(sums, "file:SHA256SUMS")
}

func (r *Release) serve(w http.ResponseWriter, req *http.Request) {
	r.Hits.Add(1)
	if req.URL.Path == "/repos/ndthuan/kipp/releases/latest" {
		_, _ = fmt.Fprintf(w, `{"tag_name":%q,"html_url":"x","assets":[{"name":"SHA256SUMS.sig"}]}`, Newer)
		return
	}
	file := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
	body, ok := r.Files[file]
	if !ok || !strings.Contains(req.URL.Path, "/releases/download/"+Newer+"/") {
		http.NotFound(w, req)
		return
	}
	_, _ = w.Write(body)
}

// Checker is an update checker of a server running current, pointed at the
// fake GitHub and told what it has.
func (r *Release) Checker(current string) *update.Checker {
	u, _ := url.Parse(r.Srv.URL)
	c := update.New(update.Options{Current: current, Base: r.Srv.URL, DownloadBase: r.Srv.URL,
		Hosts: []string{u.Hostname()}, Insecure: true, Timeout: 5 * time.Second})
	if err := c.Check(context.Background()); err != nil {
		r.t.Fatal(err)
	}
	return c
}

// Server stands in for the dispatcher and the restart.
type Server struct {
	mu    sync.Mutex
	quiet bool
	busy  int // builds running
	calls []bool
	// Switched counts the restarts asked for.
	Switched atomic.Int32
}

// SetQuiet is the dispatcher's, recorded.
func (f *Server) SetQuiet(_ context.Context, on bool) (engine.Counts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quiet = on
	f.calls = append(f.calls, on)
	return engine.Counts{Quiet: f.quiet, Running: f.busy}, nil
}

// Counts is what the dispatcher says of quiet and the builds.
func (f *Server) Counts(context.Context) (engine.Counts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return engine.Counts{Quiet: f.quiet, Running: f.busy}, nil
}

// SetBusy sets how many builds run.
func (f *Server) SetBusy(n int) {
	f.mu.Lock()
	f.busy = n
	f.mu.Unlock()
}

// QuietCalls is every call to SetQuiet, as the state asked for.
func (f *Server) QuietCalls() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.calls...)
}

// NewServer is a Server with nothing running.
func NewServer() *Server { return &Server{} }

// Switch is the restart, counted.
func (f *Server) Switch() { f.Switched.Add(1) }

// SetQuietOn sets the quiet mode as an operator might have.
func (f *Server) SetQuietOn(on bool) {
	f.mu.Lock()
	f.quiet = on
	f.mu.Unlock()
}

// Engine makes a data directory with a database and an engine for a server
// running Running on it, with this fake release and server. tweak may change
// the options first.
func Engine(t *testing.T, rel *Release, f *Server, tweak func(*engine.Options)) (*engine.Engine, string) {
	t.Helper()
	data := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(data, backup.DBFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE t (v TEXT); INSERT INTO t VALUES ('a')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	o := engine.Options{Data: data, Version: Running, Checker: rel.Checker(Running), Key: rel.Pub,
		InstallType: func() string { return update.Archive },
		SetQuiet:    f.SetQuiet, Counts: f.Counts, Switch: f.Switch,
		Free: func(string) (uint64, bool) { return 1 << 40, true },
		Poll: 5 * time.Millisecond,
	}
	if tweak != nil {
		tweak(&o)
	}
	return engine.New(o), data
}
