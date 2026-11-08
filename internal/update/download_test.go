package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ndthuan/kipp/internal/waitfor"
)

// downloader is a checker whose downloads come from a server that answers
// every file with handler.
func downloader(t *testing.T, h http.HandlerFunc) *Checker {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return New(Options{Current: "v1.0.0", Base: srv.URL, DownloadBase: srv.URL, Hosts: []string{u.Hostname()}, Insecure: true, DownloadTimeout: waitfor.Safety})
}

func TestAFileIsFetchedFromTheAddressBuiltFromTheRepositoryAndTheTag(t *testing.T) {
	var got string
	c := downloader(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
		_, _ = w.Write([]byte("body"))
	})
	b, err := c.Fetch(context.Background(), "v1.2.3", "SHA256SUMS", 100)
	if err != nil || string(b) != "body" {
		t.Fatalf("%q, %v", b, err)
	}
	if got != "/ndthuan/kipp/releases/download/v1.2.3/SHA256SUMS" {
		t.Fatalf("asked for %s", got)
	}
}

func TestAFileOverTheCapIsRefusedWhetherOrNotTheServerSaysItsSize(t *testing.T) {
	big := strings.Repeat("x", 101)
	sized := downloader(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(big)) })
	if _, err := sized.Fetch(context.Background(), "v1.2.3", "a.tar.gz", 100); err == nil || !strings.Contains(err.Error(), "100") {
		t.Fatalf("a sized body: %v", err)
	}
	chunked := downloader(t, func(w http.ResponseWriter, _ *http.Request) {
		w.(http.Flusher).Flush() // no Content-Length
		_, _ = w.Write([]byte(big))
	})
	if _, err := chunked.Fetch(context.Background(), "v1.2.3", "a.tar.gz", 100); err == nil || !strings.Contains(err.Error(), "over") {
		t.Fatalf("an unsized body: %v", err)
	}
	exact := downloader(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(big[:100])) })
	if _, err := exact.Fetch(context.Background(), "v1.2.3", "a.tar.gz", 100); err != nil {
		t.Fatalf("a body of exactly the cap: %v", err)
	}
}

func TestARedirectOffTheReleaseHostsIsRefused(t *testing.T) {
	c := downloader(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://evil.example.com/kipp", http.StatusFound)
	})
	_, err := c.Fetch(context.Background(), "v1.2.3", "a.tar.gz", 100)
	if err == nil || !strings.Contains(err.Error(), "evil.example.com") {
		t.Fatalf("%v", err)
	}
}

func TestARedirectToAnotherReleaseHostIsFollowed(t *testing.T) {
	var moved bool
	c := downloader(t, func(w http.ResponseWriter, r *http.Request) {
		if !moved {
			moved = true
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("here"))
	})
	if b, err := c.Fetch(context.Background(), "v1.2.3", "a.tar.gz", 100); err != nil || string(b) != "here" {
		t.Fatalf("%q, %v", b, err)
	}
}

func TestNothingButAReleaseFileOfAReleaseTagIsFetched(t *testing.T) {
	var hits int
	c := downloader(t, func(http.ResponseWriter, *http.Request) { hits++ })
	for _, tc := range [][2]string{{"v1.2.3", "../x"}, {"v1.2.3", "a/b"}, {"v1.2.3", ""}, {"v1.2.3", "a?b"}, {"main", "SHA256SUMS"}, {"v1.2.3-rc1", "SHA256SUMS"}, {"v1.2.3/../..", "x"}} {
		if _, err := c.Fetch(context.Background(), tc[0], tc[1], 100); err == nil {
			t.Errorf("fetched %q of %q", tc[1], tc[0])
		}
	}
	if hits != 0 {
		t.Fatalf("%d requests", hits)
	}
}

func TestAFileTheServerDoesNotHaveIsAnError(t *testing.T) {
	c := downloader(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	if _, err := c.Fetch(context.Background(), "v1.2.3", "a.tar.gz", 100); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("%v", err)
	}
}

func TestProductionDownloadsFromGitHubAndOnlyAKipptestBuildFollowsTheLookup(t *testing.T) {
	o := Production("v1.0.0", "", nil)
	if testBuild {
		return // a test build's base comes from the environment
	}
	if o.DownloadBase != "https://github.com" || o.Insecure {
		t.Fatalf("%+v", o)
	}
}

func tarball(t *testing.T, files map[string]string, typ byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: typ}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = zw.Close()
	return buf.Bytes()
}

func TestTheBinaryIsReadFromItsDirectoryOfTheArchiveAndNowhereElse(t *testing.T) {
	dir := ArchiveDir("v1.2.3", "linux", "amd64")
	if dir != "kipp_1.2.3_linux_amd64" || ArchiveName("v1.2.3", "linux", "amd64") != dir+".tar.gz" {
		t.Fatalf("%s", dir)
	}
	ok := tarball(t, map[string]string{dir + "/README.md": "r", dir + "/kipp": "binary"}, tar.TypeReg)
	if b, err := ExtractBinary(ok, dir, 100); err != nil || string(b) != "binary" {
		t.Fatalf("%q, %v", b, err)
	}
	if _, err := ExtractBinary(tarball(t, map[string]string{"other/kipp": "x"}, tar.TypeReg), dir, 100); err == nil {
		t.Fatal("a kipp in another directory was taken")
	}
	if _, err := ExtractBinary(tarball(t, map[string]string{dir + "/kipp": "x"}, tar.TypeSymlink), dir, 100); err == nil {
		t.Fatal("a link was taken for the binary")
	}
	if _, err := ExtractBinary(ok, dir, 3); err == nil || !strings.Contains(err.Error(), "over") {
		t.Fatalf("over the cap: %v", err)
	}
	if _, err := ExtractBinary([]byte("not gzip"), dir, 100); err == nil {
		t.Fatal("garbage was read as an archive")
	}
}
