package cli

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/ndthuan/kipp/internal/update"
	"github.com/ndthuan/kipp/internal/version"
)

// fakeReleases answers the latest-release lookup with v9.9.9, signed, and
// counts what it is asked; the server under test is pointed at it.
func fakeReleases(t *testing.T) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9","html_url":"https://github.com/ndthuan/kipp/releases/tag/v9.9.9","assets":[{"name":"SHA256SUMS.sig"}]}`))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	realOptions, realVersion := updateOptions, version.Version
	t.Cleanup(func() { updateOptions, version.Version = realOptions, realVersion })
	version.Version = "v1.0.0"
	updateOptions = func(current, dir string, log *slog.Logger) update.Options {
		return update.Options{Current: current, Dir: dir, Base: srv.URL, Hosts: []string{u.Hostname()}, Insecure: true, Log: log}
	}
	return &hits
}

func serverJSON(t *testing.T, s *served) map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.url+"/api/v1/admin/server", nil)
	must(t, err)
	req.Header.Set("Authorization", "Bearer "+s.token)
	res, err := http.DefaultClient.Do(req)
	must(t, err)
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	must(t, err)
	var got map[string]any
	must(t, json.Unmarshal(body, &got))
	return got
}

func TestAServerOfAReleaseAsksForTheLatestReleaseAtStartAndSaysSo(t *testing.T) {
	hits := fakeReleases(t)
	s := start(t, map[string]string{"hello.yml": helloJob})
	eventually(t, "the server knows the release", func() bool { return serverJSON(t, s)["latest"] == "v9.9.9" })
	got := serverJSON(t, s)
	if got["upgradable"] != true || got["update_check"] != true || got["checked_at"] == nil ||
		got["notes_url"] != "https://github.com/ndthuan/kipp/releases/tag/v9.9.9" || got["origin"] == "" {
		t.Fatalf("%v", got)
	}
	eq(t, "requests", hits.Load(), int32(1))
}

func TestNoUpdateCheckMakesNoRequestAndSaysTheCheckIsOff(t *testing.T) {
	hits := fakeReleases(t)
	s := &served{t: t, stderr: &syncBuffer{}, args: []string{"--no-update-check"}}
	s.data, s.jobs, s.token = dataAndJobs(t, map[string]string{"hello.yml": helloJob})
	s.serve()
	got := serverJSON(t, s)
	if got["update_check"] != false || got["latest"] != "" || got["checked_at"] != nil {
		t.Fatalf("%v", got)
	}
	// Nothing can ask once the server has ended.
	s.stop()
	eq(t, "requests", hits.Load(), int32(0))
}

func TestTheEnvironmentTurnsTheUpdateCheckOffAsTheFlagDoes(t *testing.T) {
	hits := fakeReleases(t)
	t.Setenv("KIPP_NO_UPDATE_CHECK", "1")
	s := start(t, map[string]string{"hello.yml": helloJob})
	eq(t, "check", serverJSON(t, s)["update_check"], false)
	s.stop()
	eq(t, "requests", hits.Load(), int32(0))

	t.Setenv("KIPP_NO_UPDATE_CHECK", "maybe")
	if _, err := parseServeFlags([]string{"--data", "d", "--jobs", "j"}, io.Discard); err == nil {
		t.Fatal("a value that is no boolean was taken")
	}
}

func TestADevBuildMakesNoUpdateRequest(t *testing.T) {
	hits := fakeReleases(t)
	version.Version = "dev"
	s := start(t, map[string]string{"hello.yml": helloJob})
	got := serverJSON(t, s)
	if got["latest"] != "" || got["checked_at"] != nil {
		t.Fatalf("%v", got)
	}
	s.stop()
	eq(t, "requests", hits.Load(), int32(0))
}

func TestVersionPrintsWhereTheBinaryCameFromAndAsksNothing(t *testing.T) {
	t.Setenv("KIPP_INSTALL_SOURCE", "docker")
	code, out, _ := kipp("", "version")
	eq(t, "exit", code, 0)
	contains(t, out, "kipp "+version.Version+"\n", "installed from: Docker image\n")
}
