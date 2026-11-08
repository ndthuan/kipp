package update

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAPI answers the latest-release lookup with what its fields hold and
// counts the requests.
type fakeAPI struct {
	status     int
	tag        string
	prerelease bool
	assets     []string
	delay      time.Duration
	hits       atomic.Int32
	agent      atomic.Value
	srv        *httptest.Server
}

func newFake(t *testing.T, tag string, assets ...string) *fakeAPI {
	t.Helper()
	f := &fakeAPI{status: http.StatusOK, tag: tag, assets: assets}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.agent.Store(r.Header.Get("User-Agent"))
		if r.URL.Path != releasePath {
			http.NotFound(w, r)
			return
		}
		time.Sleep(f.delay)
		if f.status != http.StatusOK {
			http.Error(w, "no", f.status)
			return
		}
		type asset struct {
			Name string `json:"name"`
		}
		var as []asset
		for _, n := range f.assets {
			as = append(as, asset{n})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": f.tag, "html_url": "https://github.com/ndthuan/kipp/releases/tag/" + f.tag,
			"prerelease": f.prerelease, "assets": as,
		})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) checker(current, dir string) *Checker {
	u, _ := url.Parse(f.srv.URL)
	return New(Options{Current: current, Dir: dir, Base: f.srv.URL, Hosts: []string{u.Hostname()}, Insecure: true, Timeout: time.Second})
}

func TestANewerReleaseIsReportedWithItsNotesAndWhetherItIsUpgradable(t *testing.T) {
	f := newFake(t, "v1.2.0", "SHA256SUMS", SigFile)
	c := f.checker("v1.1.0", "")
	if err := c.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := c.Status()
	if st.Latest != "v1.2.0" || !st.Upgradable || st.NotesURL != "https://github.com/ndthuan/kipp/releases/tag/v1.2.0" || st.CheckedAt.IsZero() {
		t.Fatalf("%+v", st)
	}
	if got := f.agent.Load(); got != "kipp/v1.1.0" {
		t.Fatalf("the User-Agent is %v", got)
	}
}

func TestANewerReleaseWithoutTheSignatureFileIsReportedButNotUpgradable(t *testing.T) {
	f := newFake(t, "v1.2.0", "SHA256SUMS")
	c := f.checker("v1.1.0", "")
	if err := c.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := c.Status(); st.Latest != "v1.2.0" || st.Upgradable {
		t.Fatalf("%+v", st)
	}
}

func TestAnEqualOrOlderReleaseIsNotReported(t *testing.T) {
	for _, tag := range []string{"v1.1.0", "v1.0.9", "v0.9.0"} {
		f := newFake(t, tag, SigFile)
		c := f.checker("v1.1.0", "")
		if err := c.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
		st := c.Status()
		if st.Latest != "" || st.Upgradable || st.CheckedAt.IsZero() {
			t.Fatalf("%s: %+v", tag, st)
		}
	}
}

func TestAPreReleaseIsIgnored(t *testing.T) {
	f := newFake(t, "v2.0.0", SigFile)
	f.prerelease = true
	c := f.checker("v1.1.0", "")
	if c.Check(context.Background()) == nil || c.Status().Latest != "" {
		t.Fatal("a pre-release was taken")
	}
	g := newFake(t, "v2.0.0-rc1", SigFile)
	d := g.checker("v1.1.0", "")
	if d.Check(context.Background()) == nil || d.Status().Latest != "" {
		t.Fatal("a pre-release tag was taken")
	}
}

func TestA404AndA403AndA429AndGarbageAndATimeoutLeaveNothingReported(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusTooManyRequests} {
		f := newFake(t, "v2.0.0", SigFile)
		f.status = code
		c := f.checker("v1.1.0", "")
		if c.Check(context.Background()) == nil {
			t.Fatalf("%d was not an error", code)
		}
		if st := c.Status(); st.Latest != "" || !st.CheckedAt.IsZero() {
			t.Fatalf("%d: %+v", code, st)
		}
	}
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) }))
	defer garbage.Close()
	u, _ := url.Parse(garbage.URL)
	c := New(Options{Current: "v1.1.0", Base: garbage.URL, Hosts: []string{u.Hostname()}, Insecure: true})
	if c.Check(context.Background()) == nil || c.Status().Latest != "" {
		t.Fatal("garbage was taken")
	}
	slow := newFake(t, "v2.0.0", SigFile)
	slow.delay = 300 * time.Millisecond
	sc := slow.checker("v1.1.0", "")
	sc.client.Timeout = 50 * time.Millisecond
	if sc.Check(context.Background()) == nil || sc.Status().Latest != "" {
		t.Fatal("a timeout was taken")
	}
}

func TestADevBuildMakesNoRequest(t *testing.T) {
	for _, v := range []string{"dev", "v1.1.0-3-gabc123", "v1.1.0-rc1", "", "1.1.0"} {
		f := newFake(t, "v9.0.0", SigFile)
		c := f.checker(v, "")
		if err := c.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		c.Run(ctx)
		cancel()
		if n := f.hits.Load(); n != 0 || c.Status() != (Status{}) {
			t.Fatalf("%q: %d requests, %+v", v, n, c.Status())
		}
	}
}

func TestACheckerThatIsNeverRunMakesNoRequest(t *testing.T) {
	f := newFake(t, "v9.0.0", SigFile)
	_ = f.checker("v1.0.0", "")
	if f.hits.Load() != 0 {
		t.Fatal("making a checker asked")
	}
}

func TestTheKeptAnswerStopsASecondRequestInsideTheIntervalAfterARestart(t *testing.T) {
	dir := t.TempDir()
	f := newFake(t, "v1.2.0", SigFile)
	first := f.checker("v1.1.0", dir)
	if err := first.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, CacheFile)); err != nil {
		t.Fatal(err)
	}
	second := f.checker("v1.1.0", dir)
	if st := second.Status(); st.Latest != "v1.2.0" || !st.Upgradable {
		t.Fatalf("the kept answer was not read: %+v", st)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	second.Run(ctx)
	cancel()
	if n := f.hits.Load(); n != 1 {
		t.Fatalf("%d requests, want the first only", n)
	}
}

func TestAnAnswerOlderThanTheIntervalIsAskedAgainAtOnce(t *testing.T) {
	dir := t.TempDir()
	f := newFake(t, "v1.2.0", SigFile)
	old := found{Version: "v1.1.5", Signed: true, CheckedAt: time.Now().Add(-Interval - time.Minute)}
	b, _ := json.Marshal(old)
	if err := os.WriteFile(filepath.Join(dir, CacheFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	c := f.checker("v1.1.0", dir)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for f.hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if f.hits.Load() != 1 {
		t.Fatalf("%d requests", f.hits.Load())
	}
}

func TestARunAsksAgainEveryInterval(t *testing.T) {
	f := newFake(t, "v1.2.0", SigFile)
	u, _ := url.Parse(f.srv.URL)
	c := New(Options{Current: "v1.1.0", Base: f.srv.URL, Hosts: []string{u.Hostname()}, Insecure: true, Interval: 30 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	c.Run(ctx)
	if f.hits.Load() < 3 {
		t.Fatalf("%d requests", f.hits.Load())
	}
}

func TestAKeptAnswerThatIsDatedInTheFutureOrUnreadableIsNone(t *testing.T) {
	for _, body := range []string{
		`{"version":"v1.2.0","signed":true,"checked_at":"2999-01-01T00:00:00Z"}`,
		`not json`,
		`{"version":"nightly","checked_at":"2020-01-01T00:00:00Z"}`,
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, CacheFile), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if st := New(Options{Current: "v1.1.0", Dir: dir}).Status(); st != (Status{}) {
			t.Fatalf("%s: %+v", body, st)
		}
	}
}

func TestARedirectOutsideTheAllowlistOrToPlainHTTPIsRefused(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer elsewhere.Close()
	bounce := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://localhost:"+mustPort(t, elsewhere.URL)+"/x", http.StatusFound)
	}))
	defer bounce.Close()
	u, _ := url.Parse(bounce.URL)
	c := New(Options{Current: "v1.1.0", Base: bounce.URL, Hosts: []string{u.Hostname()}, Insecure: true})
	if err := c.Check(context.Background()); err == nil {
		t.Fatal("followed a redirect to a host off the list")
	}
	strict := New(Options{Current: "v1.1.0", Base: bounce.URL, Hosts: []string{u.Hostname()}})
	if err := strict.Check(context.Background()); err == nil {
		t.Fatal("plain HTTP was let through")
	}
}

func mustPort(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Port()
}

func TestTheProductionOptionsPointAtGitHubAndNothingElse(t *testing.T) {
	t.Setenv("KIPP_TEST_UPDATE_API", "http://127.0.0.1:1") // honoured only under the kipptest tag
	o := Production("v1.0.0", "", nil)
	if testBuild {
		return
	}
	if o.Base != "https://api.github.com" || o.Insecure {
		t.Fatalf("%+v", o)
	}
	want := []string{"api.github.com", "github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com"}
	if len(o.Hosts) != len(want) {
		t.Fatalf("%v", o.Hosts)
	}
	for i := range want {
		if o.Hosts[i] != want[i] {
			t.Fatalf("%v", o.Hosts)
		}
	}
}

func TestReleaseTagsAreComparedByNumber(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v1.10.0", "v1.9.0", true}, {"v1.0.1", "v1.0.0", true}, {"v2.0.0", "v1.99.99", true},
		{"v1.0.0", "v1.0.0", false}, {"v1.0.0", "v1.0.1", false},
		{"v1.0.0-rc1", "v0.9.0", false}, {"v2.0.0", "dev", false}, {"v01.0.0", "v0.1.0", false},
	} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func TestTheInstallTypeComesFromTheMarkerThenThePathThenTheVersion(t *testing.T) {
	for _, c := range []struct {
		ver, marker, exe, want string
	}{
		{"v1.0.0", "docker", "/usr/local/bin/kipp", Docker},
		{"v1.0.0", "", "/opt/homebrew/Cellar/kipp/1.0.0/bin/kipp", Homebrew},
		{"v1.0.0", "", "/usr/local/Cellar/kipp/1.0.0/bin/kipp", Homebrew},
		{"v1.0.0", "", "/usr/local/bin/kipp", Archive},
		{"v1.0.0", "", "/home/me/Cellarette/kipp", Archive},
		{"dev", "docker", "/usr/local/bin/kipp", Dev},
		{"v1.0.0-3-gabc", "", "/opt/homebrew/Cellar/kipp/1.0.0/bin/kipp", Dev},
	} {
		if got := installType(c.ver, c.marker, c.exe); got != c.want {
			t.Errorf("%+v: %s", c, got)
		}
	}
}

func TestTheBundledExecutableIsWhatTheInstallTypeJudges(t *testing.T) {
	root := t.TempDir()
	cellar := filepath.Join(root, "Cellar", "kipp", "1.0.0", "bin")
	if err := os.MkdirAll(cellar, 0o750); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(cellar, "kipp")
	if err := os.WriteFile(exe, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "kipp")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIPP_BUNDLED_EXE", link)
	t.Setenv("KIPP_INSTALL_SOURCE", "")
	if got := originType("", executable()); got != Homebrew {
		t.Fatalf("a link into the Cellar is %s", got)
	}
	if Origin() != "Homebrew" {
		t.Fatal(Origin())
	}
	t.Setenv("KIPP_INSTALL_SOURCE", "docker")
	if Origin() != "Docker image" {
		t.Fatal(Origin())
	}
}

func TestServerOriginNamesTheDataDirectoryOnlyWhenTheLauncherSwitched(t *testing.T) {
	t.Setenv("KIPP_INSTALL_SOURCE", "docker")
	t.Setenv("KIPP_BUNDLED_EXE", "")
	t.Setenv("KIPP_BUNDLED_VERSION", "v1.0.0")
	t.Setenv("KIPP_LAUNCHED_FROM_DATADIR", "")
	if got := ServerOrigin(); got != "Docker image" {
		t.Fatalf("without the switch: %q", got)
	}
	t.Setenv("KIPP_LAUNCHED_FROM_DATADIR", "1")
	if got := ServerOrigin(); got != "data directory (the executable's own is v1.0.0)" {
		t.Fatalf("after the switch: %q", got)
	}
	if got := Origin(); got != "Docker image" {
		t.Fatalf("kipp version is not changed: %q", got)
	}
	t.Setenv("KIPP_BUNDLED_VERSION", "not a version")
	if got := ServerOrigin(); got != "Docker image" {
		t.Fatalf("a bundled version that is no release is not shown: %q", got)
	}
}
