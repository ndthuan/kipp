package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// call is one API request as the admin, its status and body.
func (s *served) call(method, path, body string, hdr ...string) (int, string, http.Header) {
	s.t.Helper()
	req, err := http.NewRequest(method, s.url+path, strings.NewReader(body))
	must(s.t, err)
	req.Header.Set("Authorization", "Bearer "+s.token)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	must(s.t, err)
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(res.Body)
	must(s.t, err)
	return res.StatusCode, string(data), res.Header
}

const storedJob = "name: stored\nstages:\n  s: {run: 'echo stored'}\n"

func createBody(t *testing.T, name, yaml string) string {
	t.Helper()
	data, err := json.Marshal(map[string]string{"name": name, "yaml": yaml})
	must(t, err)
	return string(data)
}

func TestAJobCreatedThroughKippIsServedAgainAfterARestartWithoutAJobsDirectory(t *testing.T) {
	s := start(t, map[string]string{"hello.yml": helloJob})
	code, body, _ := s.call("POST", "/api/v1/jobs", createBody(t, "stored", storedJob))
	eq(t, "create: "+body, code, 201)
	contains(t, s.stderr.String(), `msg="job created through kipp" job=stored`)
	eq(t, "exit", s.stop(), 0)

	again := &served{t: t, stderr: &syncBuffer{}, data: s.data, token: s.token}
	again.serve()
	code, body, _ = again.call("GET", "/api/v1/jobs/stored/definition", "")
	eq(t, "definition: "+body, code, 200)
	contains(t, body, `"source":"store"`)
	// hello came from a directory this server was not given.
	code, _, _ = again.call("GET", "/api/v1/jobs/hello", "")
	eq(t, "hello", code, 404)
}

func TestFilesModeWritesTheJobFile(t *testing.T) {
	s := &served{t: t, stderr: &syncBuffer{}, args: []string{"--job-edits", "files"}}
	s.data, s.jobs, s.token = dataAndJobs(t, map[string]string{"hello.yml": helloJob})
	s.serve()
	code, body, _ := s.call("POST", "/api/v1/jobs", createBody(t, "stored", storedJob))
	eq(t, "create: "+body, code, 201)
	data, err := os.ReadFile(filepath.Join(s.jobs, "stored.yml"))
	must(t, err)
	eq(t, "file", string(data), storedJob)
	contains(t, body, `"source":"file"`)

	// A file changed on disk since kipp read it refuses a change until a reload.
	write(t, filepath.Join(s.jobs, "stored.yml"), strings.Replace(storedJob, "echo stored", "echo by hand", 1), 0o600)
	_, _, h := s.call("GET", "/api/v1/jobs/stored/definition", "")
	code, body, _ = s.call("PUT", "/api/v1/jobs/stored/definition", `{"yaml":"name: stored\nstages:\n  s: {run: 'true'}\n"}`, "If-Match", h.Get("ETag"))
	eq(t, "stale file: "+body, code, 409)
	contains(t, body, "files_changed")
}

func TestFilesModeRefusesToStartOnADirectoryItCannotWrite(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes a read-only directory")
	}
	data, jobs, _ := dataAndJobs(t, map[string]string{"hello.yml": helloJob})
	must(t, os.Chmod(jobs, 0o500))
	t.Cleanup(func() { _ = os.Chmod(jobs, 0o700) })
	var out, errOut syncBuffer
	code := serve(soon(t), []string{"--data", data, "--jobs", jobs, "--job-edits", "files", "--listen", "127.0.0.1:0"}, &out, &errOut)
	eq(t, "exit", code, 1)
	contains(t, errOut.String(), jobs, "uid", "drop :ro", "--job-edits store")
	lacks(t, out.String(), "listening")
}
