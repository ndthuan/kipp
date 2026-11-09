package runnerhub

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/artifacts"
	"github.com/ndthuan/kipp/internal/runnerproto"
)

// tarOf is a tar of the headers given, each regular one holding size
// bytes of x.
func tarOf(t *testing.T, headers ...tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range headers {
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(bytes.Repeat([]byte("x"), int(h.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// claimed gives box a stage that keeps artifacts of up to max bytes in
// dir and claims it by hand, as a forged runner would, and is its id.
func (f *fixture) claimed(dir string, max int64) (string, func() (int, error)) {
	f.t.Helper()
	f.call(http.MethodPost, runnerproto.ConnectPath, `{"Version":"v-test"}`)
	meta := StageMeta{BuildID: 1, Job: "j", Number: 1, Stage: "s", Workspace: "j/1",
		Artifacts: &runnerproto.Artifacts{Paths: []string{"*"}, Max: max}, ArtifactsDir: dir}
	p, _ := f.start(context.Background(), "true", meta)
	resp := f.do(http.MethodGet, runnerproto.ClaimPath, nil)
	var c runnerproto.Claim
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil || c.Assignment == nil {
		f.t.Fatalf("%v %+v", err, c)
	}
	_ = resp.Body.Close()
	return c.Assignment.ID, p.Wait
}

func (f *fixture) do(method, path string, body io.Reader) *http.Response {
	f.t.Helper()
	req, err := http.NewRequest(method, f.srv.URL+path, body)
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+boxToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp
}

// A stage in a container runs as root beside its runner and can write the
// tar it sends: the server takes regular files inside the stage's
// directory, up to its max_size, and nothing else, writes nothing outside
// the directory it was given, and ends the stage failed saying why.
func TestTheServerRefusesAForgedArtifactTarFromAnRunner(t *testing.T) {
	for name, tc := range map[string]struct {
		body   []byte
		size   string
		status int
		why    string
	}{
		"a member climbing out": {tarOf(t, tar.Header{Name: "../escape.txt", Typeflag: tar.TypeReg, Size: 2, Mode: 0o600}), "2", http.StatusBadRequest, `refused tar member "../escape.txt"`},
		"an absolute member":    {tarOf(t, tar.Header{Name: "/tmp/escape.txt", Typeflag: tar.TypeReg, Size: 2, Mode: 0o600}), "2", http.StatusBadRequest, `refused tar member "/tmp/escape.txt"`},
		"a symlink":             {tarOf(t, tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}), "0", http.StatusBadRequest, `refused tar member "link": only files and directories`},
		"a hard link":           {tarOf(t, tar.Header{Name: "hard", Typeflag: tar.TypeLink, Linkname: "../../x"}), "0", http.StatusBadRequest, `refused tar member "hard"`},
		"over max_size, said":   {tarOf(t, tar.Header{Name: "a.bin", Typeflag: tar.TypeReg, Size: 5, Mode: 0o600}), "5", http.StatusRequestEntityTooLarge, "artifacts: 5 bytes over the stage's max_size of 4"},
		"over max_size, unsaid": {tarOf(t, tar.Header{Name: "a.bin", Typeflag: tar.TypeReg, Size: 3, Mode: 0o600}, tar.Header{Name: "b.bin", Typeflag: tar.TypeReg, Size: 3, Mode: 0o600}), "1", http.StatusRequestEntityTooLarge, "artifacts: 6 bytes over the stage's max_size of 4"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			base := t.TempDir()
			dir := filepath.Join(base, "staging", "s")
			id, wait := f.claimed(dir, 4)
			resp := f.do(http.MethodPut, runnerproto.StagePath+id+runnerproto.ArtifactsSuffix+"?size="+tc.size, bytes.NewReader(tc.body))
			_ = resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("answered %d, want %d", resp.StatusCode, tc.status)
			}
			_ = f.do(http.MethodPost, runnerproto.StagePath+id+runnerproto.DoneSuffix+"?exit=0", nil).Body.Close()
			if _, err := wait(); err == nil || !strings.Contains(err.Error(), tc.why) {
				t.Fatalf("the stage ended with %v, want %q", err, tc.why)
			}
			if found := filesUnder(base); len(found) != 0 {
				t.Fatalf("written: %v", found)
			}
		})
	}
}

// What a stage keeps travels on an upload of its own, so a stage keeps
// more than the done body takes. The fixture's runner sends no heartbeat
// while it uploads, as a real one does, so the heartbeat is long enough
// for the upload on a slow machine under the race detector.
func TestArtifactsOverTheDoneBodysCapReachTheServer(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.Heartbeat = 10 * time.Second })
	dir := filepath.Join(t.TempDir(), "s")
	id, wait := f.claimed(dir, 1<<30)
	size := int64(maxJunitBody + 1)
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		_ = tw.WriteHeader(&tar.Header{Name: "big.bin", Typeflag: tar.TypeReg, Size: size, Mode: 0o600})
		_, _ = io.CopyN(tw, zeroReader{}, size)
		_ = pw.CloseWithError(tw.Close())
	}()
	resp := f.do(http.MethodPut, runnerproto.StagePath+id+runnerproto.ArtifactsSuffix, pr)
	_ = resp.Body.Close()
	_ = f.do(http.MethodPost, runnerproto.StagePath+id+runnerproto.DoneSuffix+"?exit=0", nil).Body.Close()
	if code, err := wait(); resp.StatusCode != http.StatusNoContent || code != 0 || err != nil {
		t.Fatalf("%d: %d %v", resp.StatusCode, code, err)
	}
	if info, err := os.Stat(filepath.Join(dir, "big.bin")); err != nil || info.Size() != size {
		t.Fatalf("%v %v", info, err)
	}
}

// filesUnder is every file under dir, which a refused tar left none of.
func filesUnder(dir string) []string {
	var found []string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			found = append(found, p)
		}
		return err
	})
	return found
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// A runner is sent the files of the stages its stage is given, and of no
// other stage of the build, whatever name it asks for.
func TestAnRunnerIsSentOnlyTheInputsItsStageIsGiven(t *testing.T) {
	f := newFixture(t)
	kept := t.TempDir()
	if err := os.WriteFile(filepath.Join(kept, "app.js"), []byte("app"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.call(http.MethodPost, runnerproto.ConnectPath, `{"Version":"v-test"}`)
	meta := StageMeta{BuildID: 1, Job: "j", Number: 1, Stage: "deploy", Workspace: "j/1",
		Inputs: []runnerproto.Input{{Stage: "build", Size: 3}}, InputDirs: map[string]string{"build": kept}}
	_, _ = f.start(context.Background(), "true", meta)
	resp := f.do(http.MethodGet, runnerproto.ClaimPath, nil)
	var c runnerproto.Claim
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil || c.Assignment == nil || len(c.Assignment.Inputs) != 1 {
		t.Fatalf("%v %+v", err, c)
	}
	_ = resp.Body.Close()
	base := runnerproto.StagePath + c.Assignment.ID + runnerproto.InputsSuffix
	resp = f.do(http.MethodGet, base+"build", nil)
	dir := filepath.Join(t.TempDir(), "in")
	if _, err := artifacts.Untar(resp.Body, dir, 3, -1); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("%d %v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()
	if data, err := os.ReadFile(filepath.Join(dir, "app.js")); err != nil || string(data) != "app" {
		t.Fatalf("%q %v", data, err)
	}
	for _, name := range []string{"deploy", "test", "..", "%2e%2e"} {
		resp := f.do(http.MethodGet, base+name, nil)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: %d, want 404", name, resp.StatusCode)
		}
	}
}
