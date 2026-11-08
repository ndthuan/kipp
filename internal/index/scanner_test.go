package index

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/store"
	"github.com/ndthuan/kipp/internal/waitfor"
)

const scanned = "name: site\nmultibranch: {repo: acme/site, token: GH_TOKEN, scan: 10m}\n"

// scannerFixture is a Scanner over a fixture, on a clock the test moves,
// that queues its builds into a list instead of a dispatcher.
type scannerFixture struct {
	*fixture
	s      *Scanner
	gh     *fakeGitHub
	srv    *httptest.Server
	mu     sync.Mutex
	queued []dispatch.TriggerRequest
	secret func(name string) (string, error)
}

func newScannerFixture(t *testing.T, files map[string]string) *scannerFixture {
	t.Helper()
	f := &scannerFixture{fixture: newFixture(t, files), gh: newFakeGitHub()}
	f.srv = httptest.NewServer(http.HandlerFunc(f.gh.serve))
	t.Cleanup(f.srv.Close)
	f.secret = func(name string) (string, error) { return "scan-token", nil }
	f.s = NewScanner(context.Background(), f.x, ScannerOptions{
		API: f.srv.URL,
		Token: func(_ context.Context, name string) (string, error) {
			return f.secret(name)
		},
		Trigger: func(_ context.Context, req dispatch.TriggerRequest) (*store.Build, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.queued = append(f.queued, req)
			return &store.Build{Job: req.Job}, nil
		},
	})
	f.s.now = func() time.Time { return f.clock }
	t.Cleanup(f.s.Wait)
	return f
}

func (f *scannerFixture) queuedJobs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, q := range f.queued {
		out = append(out, q.Job+":"+q.Kind)
	}
	return out
}

func TestAScanRunsAtLoadAndEveryScanInterval(t *testing.T) {
	f := newScannerFixture(t, map[string]string{"site": scanned})
	f.s.Reload()
	f.s.Wait()
	if n := f.gh.called("/repos/acme/site"); n == 0 {
		t.Fatal("no scan at load")
	}
	runs := func() int { f.s.Wait(); return f.gh.called("/repos/acme/site/branches/main") }
	if runs() != 1 {
		t.Fatalf("%d scans at load", runs())
	}
	f.clock = f.clock.Add(9 * time.Minute)
	f.s.tick(f.clock)
	if runs() != 1 {
		t.Fatal("scanned before the interval was out")
	}
	f.clock = f.clock.Add(time.Minute)
	f.s.tick(f.clock)
	if runs() != 2 {
		t.Fatalf("%d scans after the interval", runs())
	}
	// A reload scans again at once, whatever the interval says.
	f.s.Reload()
	if runs() != 3 {
		t.Fatalf("%d scans after a reload", runs())
	}
}

func TestAScanQueuesTheBuildsItPlansWithTriggerScan(t *testing.T) {
	f := newScannerFixture(t, map[string]string{"site": scanned})
	f.gh.pr(head2)
	f.s.Reload()
	f.s.Wait()
	if got := strings.Join(f.queuedJobs(), " "); got != "site.pr-12:scan site.main:scan" && got != "site.main:scan site.pr-12:scan" {
		t.Fatalf("queued %q", got)
	}
	st := f.s.Status("site")
	if st.Err != "" || st.At != f.clock || !strings.Contains(st.Result, "2 queued") {
		t.Fatalf("status %+v", st)
	}
}

func TestAJobWithoutATokenOrWithScanOffIsNotScanned(t *testing.T) {
	f := newScannerFixture(t, map[string]string{
		"site":  "name: site\nmultibranch: {repo: acme/site}\n",
		"quiet": "name: quiet\nmultibranch: {repo: acme/site, token: GH_TOKEN, scan: 0}\n",
	})
	f.s.Reload()
	f.s.Wait()
	if len(f.gh.calls) != 0 {
		t.Fatalf("calls = %v", f.gh.calls)
	}
	st := f.s.Status("site")
	if st.Token || st.Every != 0 && st.Every != 30*time.Minute {
		t.Fatalf("status %+v", st)
	}
	if err := f.s.Scan(context.Background(), "site"); !errors.Is(err, ErrNoToken) {
		t.Fatalf("Scan now without a token = %v", err)
	}
	// With a token, "Scan now" works even where the interval is off.
	if err := f.s.Scan(context.Background(), "quiet"); err != nil {
		t.Fatalf("Scan now = %v", err)
	}
	if f.gh.called("/repos/acme/site") == 0 {
		t.Fatal("Scan now did not scan")
	}
}

func TestScanErrorIsShownAndChangesNothing(t *testing.T) {
	f := newScannerFixture(t, map[string]string{"site": scanned})
	f.s.Reload()
	f.s.Wait()
	before := f.x.Children("site")
	f.gh.pr(head2)
	f.gh.fail = map[string]int{"/repos/acme/site/pulls": http.StatusForbidden}
	f.clock = f.clock.Add(time.Hour)
	f.queued = nil
	err := f.s.Scan(context.Background(), "site")
	if err == nil {
		t.Fatal("a failed scan returned no error")
	}
	st := f.s.Status("site")
	if !strings.Contains(st.Err, "/repos/acme/site/pulls") || strings.Contains(st.Err, "scan-token") || st.At != f.clock {
		t.Fatalf("status %+v", st)
	}
	if len(f.queued) != 0 || len(f.x.Children("site")) != len(before) {
		t.Fatal("a failed scan changed something")
	}
	// The next one that works clears the error.
	f.gh.fail = nil
	if err := f.s.Scan(context.Background(), "site"); err != nil {
		t.Fatal(err)
	}
	if st := f.s.Status("site"); st.Err != "" {
		t.Fatalf("status %+v", st)
	}
}

func TestAMissingSecretIsAScanErrorThatNamesTheSecret(t *testing.T) {
	f := newScannerFixture(t, map[string]string{"site": scanned})
	f.secret = func(name string) (string, error) { return "", errors.New("secret " + name + " is not set") }
	err := f.s.Scan(context.Background(), "site")
	if err == nil || !strings.Contains(err.Error(), "GH_TOKEN") {
		t.Fatalf("err = %v", err)
	}
	if len(f.gh.calls) != 0 {
		t.Fatal("GitHub was called without a token")
	}
}

func TestAtMostTwoScansRunAtOnce(t *testing.T) {
	files := map[string]string{}
	for _, n := range []string{"a", "b", "c", "d"} {
		files[n] = "name: " + n + "\nmultibranch: {repo: acme/" + n + ", token: GH_TOKEN, scan: 10m}\n"
	}
	f := newScannerFixture(t, files)
	var now, peak atomic.Int32
	release := make(chan struct{})
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The highest count seen, raised by compare-and-swap: a load then a
		// store lets a slower request write 1 over another's 2.
		for n, p := now.Add(1), peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		<-release
		now.Add(-1)
		http.Error(w, "stop here", http.StatusInternalServerError)
	}))
	t.Cleanup(blocked.Close)
	f.s.api = blocked.URL
	f.s.Reload()
	waitfor.Until(t, "two scans are at the server", func() bool { return now.Load() >= 2 })
	// What a limit that failed would show is a third scan at the server, and
	// the absence of one cannot be announced: it has this long to start.
	time.Sleep(50 * time.Millisecond)
	if got := now.Load(); got != 2 {
		t.Fatalf("%d scans at once, want 2", got)
	}
	close(release)
	f.s.Wait()
	if peak.Load() != 2 {
		t.Fatalf("peak %d", peak.Load())
	}
	for _, n := range []string{"a", "b", "c", "d"} {
		if f.s.Status(n).At.IsZero() {
			t.Fatalf("%s was never scanned", n)
		}
	}
}

func TestAScanOfAParentAlreadyBeingScannedIsRefused(t *testing.T) {
	f := newScannerFixture(t, map[string]string{"site": scanned})
	release := make(chan struct{})
	inside := make(chan struct{}, 1)
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case inside <- struct{}{}:
		default:
		}
		<-release
		http.Error(w, "stop here", http.StatusInternalServerError)
	}))
	t.Cleanup(blocked.Close)
	f.s.api = blocked.URL
	if err := f.s.Start("site"); err != nil {
		t.Fatal(err)
	}
	<-inside
	if err := f.s.Start("site"); !errors.Is(err, ErrScanning) {
		t.Fatalf("a second scan of the parent = %v", err)
	}
	if !f.s.Status("site").Running {
		t.Fatal("status does not say it is running")
	}
	close(release)
	f.s.Wait()
}

func TestScanningAJobThatIsNotMultibranchIsRefused(t *testing.T) {
	f := newScannerFixture(t, map[string]string{"site": scanned, "plain": "name: plain\nstages:\n  s: {run: 'true'}\n"})
	for _, name := range []string{"plain", "nobody"} {
		if err := f.s.Start(name); !errors.Is(err, ErrNotMultibranch) {
			t.Fatalf("Start(%s) = %v", name, err)
		}
	}
}

func TestAScanDuringQuietReadsNothingAndTheNextOneFindsTheCommits(t *testing.T) {
	f := newScannerFixture(t, map[string]string{"site": scanned})
	var quiet atomic.Bool
	f.s.opts.Quiet = quiet.Load
	quiet.Store(true)
	f.s.Reload()
	f.s.Wait()
	if f.gh.called("/repos/acme/site/branches/main") != 0 || len(f.queuedJobs()) != 0 {
		t.Fatalf("a quiet scan read %d times and queued %v", f.gh.called("/repos/acme/site/branches/main"), f.queuedJobs())
	}
	if st := f.s.Status("site"); st.Err != "" || !strings.Contains(st.Result, "quiet") {
		t.Fatalf("status %+v", st)
	}
	quiet.Store(false)
	f.clock = f.clock.Add(10 * time.Minute)
	f.s.tick(f.clock)
	f.s.Wait()
	if got := strings.Join(f.queuedJobs(), " "); got != "site.main:scan" {
		t.Fatalf("queued %q after quiet", got)
	}
}
