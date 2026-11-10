package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ndthuan/kipp/internal/api"
	"github.com/ndthuan/kipp/internal/auth"
	"github.com/ndthuan/kipp/internal/cache"
	"github.com/ndthuan/kipp/internal/cachesrv"
	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/diskfree"
	"github.com/ndthuan/kipp/internal/dispatch"
	"github.com/ndthuan/kipp/internal/enginepool"
	"github.com/ndthuan/kipp/internal/github"
	"github.com/ndthuan/kipp/internal/index"
	"github.com/ndthuan/kipp/internal/mcp"
	"github.com/ndthuan/kipp/internal/oauth"
	"github.com/ndthuan/kipp/internal/overview"
	"github.com/ndthuan/kipp/internal/problem"
	"github.com/ndthuan/kipp/internal/retain"
	"github.com/ndthuan/kipp/internal/runner"
	"github.com/ndthuan/kipp/internal/runnerbin"
	"github.com/ndthuan/kipp/internal/runnerhub"
	"github.com/ndthuan/kipp/internal/runnerproto"
	"github.com/ndthuan/kipp/internal/secretbox"
	"github.com/ndthuan/kipp/internal/storage"
	"github.com/ndthuan/kipp/internal/store"
	"github.com/ndthuan/kipp/internal/update"
	"github.com/ndthuan/kipp/internal/upgrade/engine"
	"github.com/ndthuan/kipp/internal/upgrade/launch"
	"github.com/ndthuan/kipp/internal/upgrade/state"
	"github.com/ndthuan/kipp/internal/version"
	"github.com/ndthuan/kipp/internal/web"
	"github.com/ndthuan/kipp/internal/webhook"
)

const serveUsage = `serve --data <dir> --jobs <dir> [flags]

kipp speaks plain HTTP and leaves TLS to a reverse proxy in front of it.
A browser keeps the session cookie only over HTTPS (or, with no proxy
between, on the server's own machine at http://127.0.0.1 or localhost), and
the login form is refused as cross-origin unless the proxy passes the
request's Host header through unchanged.

SIGHUP re-reads the jobs directory, as kipp reload does. SIGINT and SIGTERM
stop the server; a build running then ends as lost. A second one kills a
server that is still stopping, and leaves what its stages started running.
So does a crash. The next serve on the data directory kills what it finds
of those stages before it runs anything.

A stage is given of the server's environment only what --pass-env names,
then the job's env, its params, its secrets and the KIPP_ variables. A
stage on a runner is given of it only what --runner-env names, by name.

A stage with runs_on runs on a standing runner (kipp runner add), or with an
image in a container on a Docker engine (kipp engine add), that carries its
labels; runners dial the server under /runner/, on --listen or on
--runner-listen. --executors 0 is a server that runs no stage itself: one
that names no runs_on goes where --default-runs-on says.

A stage with cache_scopes restores and saves keyed caches through kipp cache,
kept under the data directory; --cache-max bounds them. A stage on a runner
reaches them at --runner-url, which it must be given, on --listen or on
--runner-listen.

A cron is read in its job's timezone, else in --timezone, else in the
host's zone (TZ, or /etc/localtime). The pages show times in --timezone,
else the host's zone, and the retention sweep runs at 03:30 there.

The server asks GitHub's releases API for the latest release at start and
about every 12 hours, one unauthenticated GET that sends nothing about you
or this install but a User-Agent of kipp/<version>, and says on the Server
page when a newer one exists. A build that is not a release (dev) never
asks. --no-update-check, or KIPP_NO_UPDATE_CHECK=1 in the environment,
turns it off.

A release build looks at <data>/bin/ first: when the version in
<data>/bin/current is a higher release than the executable's own and its
binary kipp-<version> is there and answers "version", the process becomes
that binary (same arguments, environment and process ID). KIPP_USE_BUNDLED_BINARY=1
keeps the executable's own. A dev build ignores the directory.

One server at a time uses a data directory: it holds a lock on it for as
long as it runs. Only serve makes the directory and the database in it;
the user, token and secret commands work on one a server has made.

--mcp serves an MCP endpoint at /mcp, on --listen only, for Claude and other
MCP clients. It takes the Authorization: Bearer header of an API token and
lets the token's user do what their grants allow; see docs/mcp.md.
--mcp-oauth also lets an MCP client sign in by OAuth, which kipp serves itself
(a Claude custom connector); it needs --mcp and --base-url, an https:// URL
(http:// only for localhost).
`

const (
	headerTimeout = 10 * time.Second
	idleTimeout   = 2 * time.Minute
	// drainGrace is how long requests still open may take to end once the
	// builds have.
	drainGrace = 5 * time.Second
	// rerunStopGrace is how long a stop waits for the builds it stops to
	// run again after the restart (their hooks included); past it the
	// rest ends with the other builds, and still runs again.
	rerunStopGrace = time.Minute
)

func cmdServe(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// The first signal hands both back to their default before the shutdown
	// starts, so that a second one kills a shutdown that does not end, and
	// one that comes as soon as the shutdown is logged is not swallowed.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { <-sigCtx.Done(); stop(); cancel() }()
	// The site and the webhook log through the default logger; in this
	// process that is the server's own log.
	slog.SetDefault(slog.New(slog.NewTextHandler(stderr, nil)))
	return serve(ctx, args, stdout, stderr)
}

type serveFlags struct {
	data, jobs, listen, baseURL, shell string
	executors, builds, flakyFlips      int
	// The runners': the labels of a stage that names none on a server
	// that runs no stage, the variables they are given of the server's,
	// the URL a container's runner dials, the listener of /runner/ apart
	// from --listen, and the key of an ssh:// engine.
	defaultRunsOn                   []string
	runnerEnv                       []string
	runnerURL, runnerListen, sshKey string
	// githubAPI is where the multibranch jobs' repos are read.
	githubAPI  string
	sessionTTL time.Duration
	clientIP   api.ClientIP
	// passEnv names the server's variables a stage is given.
	passEnv []string
	// zone is --timezone, else the host's by name, else nil for time.Local.
	zone *time.Location
	// artifactsMax is the most every build's artifacts may come to.
	artifactsMax config.ByteSize
	// cacheMax is the most every cache entry comes to.
	cacheMax config.ByteSize
	// noUpdateCheck is --no-update-check, or KIPP_NO_UPDATE_CHECK.
	noUpdateCheck bool
	// mcp is --mcp: the MCP endpoint at /mcp is served.
	mcp bool
	// mcpOAuth is --mcp-oauth: /mcp also takes the access tokens of the
	// OAuth sign-in, which kipp serves itself.
	mcpOAuth bool
	// oauthBase is --base-url as the OAuth issuer is written.
	oauthBase string
}

func parseServeFlags(args []string, stderr io.Writer) (*serveFlags, error) {
	f := &serveFlags{artifactsMax: 50 * config.GB, cacheMax: 20 * config.GB}
	fs := newFlags(serveUsage, stderr)
	fs.StringVar(&f.data, "data", "", "`directory` for the database, the secrets' key, logs and workspaces (required)")
	fs.StringVar(&f.jobs, "jobs", "", "`directory` of job files (required)")
	fs.StringVar(&f.listen, "listen", "127.0.0.1:8085", "`address` to listen on, in plain HTTP")
	fs.IntVar(&f.flakyFlips, "flaky-flips", overview.DefaultFlips, "changes of colour among a job's last 20 builds that make it flaky on the front page, at least 5 builds being needed; 0 hides the section")
	fs.StringVar(&f.baseURL, "base-url", "", "`URL` stages see as KIPP_URL (default http://<listen>); commit statuses link to builds under it only when it is given")
	fs.StringVar(&f.shell, "shell", "sh", "shell that runs stages, as <shell> -c <command>")
	fs.IntVar(&f.executors, "executors", 4, "stages the server runs itself at once; 0 runs none, and every stage goes to a runner")
	fs.IntVar(&f.builds, "builds", 0, "builds with something running at once: preparing, a stage on the server, or a stage started on a runner or engine; one waiting to be placed counts for none (default --executors, or 4 when that is 0)")
	defaultRunsOn := fs.String("default-runs-on", "", "`labels`, comma-separated, where a stage that names no runs_on goes on a server with --executors 0")
	runnerEnv := fs.String("runner-env", "", "`names`, comma-separated, of the server's environment variables a stage on a runner is also given")
	fs.StringVar(&f.runnerURL, "runner-url", "", "`URL` a container's runner dials the server at (default --base-url)")
	fs.StringVar(&f.runnerListen, "runner-listen", "", "`address` of a second listener that serves /api/v1/runner/ and /api/v1/cache/ and nothing else; /api/v1/runner/ is then not on --listen")
	fs.StringVar(&f.sshKey, "ssh-key", "", "private key `file` for ssh:// engines that name none (default the user's own under ~/.ssh)")
	fs.StringVar(&f.githubAPI, "github-api", "https://api.github.com", "`URL` of GitHub's API, where a multibranch job's repo is scanned and its commit statuses are posted (GitHub Enterprise has its own)")
	fs.DurationVar(&f.sessionTTL, "session-ttl", 30*24*time.Hour, "how long a browser login lasts")
	fs.StringVar(&f.clientIP.Header, "client-ip-header", "", "`header` a proxy in front puts the client's address in, like X-Forwarded-For; failed logins are then counted by client too (default none)")
	trusted := fs.String("trusted-proxy", "127.0.0.0/8,::1/128", "`CIDRs`, comma-separated, of the proxies whose --client-ip-header is believed")
	passEnv := fs.String("pass-env", strings.Join(dispatch.DefaultPassEnv, ","), "`names`, comma-separated, of the server's environment variables a stage is given; NAME_* for a prefix, * for all")
	fs.Var(&f.artifactsMax, "artifacts-max", "the most every build's artifacts come to, a `size` like 50GB; past it the oldest builds' are removed")
	fs.Var(&f.cacheMax, "cache-max", "the most every cache entry comes to, a `size` like 20GB; past it the least recently restored are removed")
	noUpdate, err := envBool("KIPP_NO_UPDATE_CHECK")
	if err != nil {
		return nil, refused(err.Error())
	}
	fs.BoolVar(&f.noUpdateCheck, "no-update-check", noUpdate, "do not ask GitHub for the latest release (also KIPP_NO_UPDATE_CHECK=1)")
	fs.BoolVar(&f.mcp, "mcp", false, "serve an MCP endpoint at /mcp for Claude and other MCP clients, authenticated by API tokens and limited to their users' grants (docs/mcp.md)")
	fs.BoolVar(&f.mcpOAuth, "mcp-oauth", false, "let /mcp clients such as Claude sign in with OAuth, served by kipp itself; needs --mcp and an https --base-url (docs/mcp.md)")
	var zone string
	fs.StringVar(&zone, "timezone", "", "IANA `zone` a cron is read in when its job names none, like Europe/Paris (default the host's)")
	positional, err := parseArgs(fs, args)
	switch {
	case err != nil:
		return nil, err
	case len(positional) != 0:
		fs.Usage()
		return nil, refused("it takes flags only")
	case f.data == "" || f.jobs == "":
		return nil, refused("--data and --jobs are required")
	case f.executors < 0:
		return nil, refused("--executors is 0 or more")
	case f.builds < 0:
		return nil, refused("--builds is 0, for the default, or more")
	case f.flakyFlips < 0:
		return nil, refused("--flaky-flips is 0, to hide the section, or more")
	case f.sessionTTL <= 0:
		return nil, refused("--session-ttl is a time to come")
	}
	if f.clientIP.Trusted, err = parsePrefixes(*trusted); err != nil {
		return nil, refused("--trusted-proxy " + err.Error())
	}
	if f.passEnv, err = dispatch.ParsePassEnv(*passEnv); err != nil {
		return nil, refused(err.Error())
	}
	if err := f.parseRunnerFlags(*defaultRunsOn, *runnerEnv); err != nil {
		return nil, err
	}
	if err := f.parseOAuthFlags(); err != nil {
		return nil, err
	}
	switch {
	case !headerName(f.clientIP.Header):
		return nil, refused("--client-ip-header is a header's name, like X-Forwarded-For")
	case zone != "":
		z, err := config.LoadZone(zone)
		if err != nil {
			return nil, refused("--timezone " + err.Error())
		}
		f.zone = z
	default:
		tz, set := os.LookupEnv("TZ")
		f.zone = hostZone(tz, set, "/etc/localtime")
	}
	return f, nil
}

// envBool reads a true or false from the environment; unset is false.
func envBool(name string) (bool, error) {
	v := os.Getenv(name)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s is true or false, not %q", name, v)
	}
	return b, nil
}

// parseOAuthFlags checks --mcp-oauth: it is the MCP endpoint's sign-in, and
// its issuer is --base-url, which must be an https origin (or one on this
// machine).
func (f *serveFlags) parseOAuthFlags() error {
	if !f.mcpOAuth {
		return nil
	}
	if !f.mcp {
		return refused("--mcp-oauth needs --mcp")
	}
	if f.baseURL == "" {
		return refused("--mcp-oauth needs --base-url: it is the address the sign-in is served at")
	}
	base, err := oauth.ParseBaseURL(f.baseURL)
	if err != nil {
		return refused("--base-url with --mcp-oauth " + err.Error())
	}
	f.oauthBase = base
	return nil
}

// parseRunnerFlags reads the flags of where the server's stages run.
func (f *serveFlags) parseRunnerFlags(defaultRunsOn, runnerEnv string) error {
	if defaultRunsOn != "" {
		labels, err := config.ParseLabels(defaultRunsOn)
		if err != nil {
			return refused("--default-runs-on " + err.Error())
		}
		f.defaultRunsOn = labels
	}
	var err error
	if f.runnerEnv, err = dispatch.ParseRunnerEnv(runnerEnv); err != nil {
		return refused(err.Error())
	}
	if f.runnerURL != "" {
		if u, err := url.Parse(f.runnerURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return refused("--runner-url is an http:// or https:// URL")
		}
	}
	return nil
}

// headerName reports whether h may name a header: letters, digits and
// hyphens, or nothing for no header at all.
func headerName(h string) bool {
	for _, r := range h {
		if r != '-' && (r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}

// parsePrefixes reads a comma-separated list of CIDRs; an address alone
// is the prefix of that one address.
func parsePrefixes(list string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for item := range strings.SplitSeq(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if addr, err := netip.ParseAddr(item); err == nil {
			out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(item)
		if err != nil {
			return nil, fmt.Errorf("%q is not a CIDR such as 10.0.0.0/8", item)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// hostZone is the host's zone by its IANA name, so that a page and the API
// can say which zone it is: Go calls the host's zone "Local", whatever it
// is. It is read from TZ when that is set and from the link /etc/localtime
// is otherwise. nil, for time.Local, is when neither names a zone.
func hostZone(tz string, set bool, localtime string) *time.Location {
	name := strings.TrimPrefix(tz, ":")
	if !set {
		target, err := os.Readlink(localtime)
		if err != nil {
			return nil
		}
		_, after, found := strings.Cut(target, "zoneinfo/")
		if !found {
			return nil
		}
		name = after
	}
	z, err := config.LoadZone(name)
	if err != nil {
		return nil
	}
	return z
}

// updateOptions is update.Production, a variable so that a test can have
// the checker ask a fake instead of GitHub.
var updateOptions = update.Production

// releaseKey and upgradeWaitPoll are the release public key and how often an
// upgrade looks at the builds, variables so that a test can sign a release
// of its own and not wait seconds.
var (
	releaseKey      = update.ReleaseKey
	upgradeWaitPoll time.Duration
)

// listen is net.Listen, a variable so that a test can have the listener
// say it took an address it did not.
var listen = net.Listen

// server is one running kipp.
type server struct {
	f   *serveFlags
	log *slog.Logger
	key []byte
	// unset is a value sealed with key, opened in place of a secret that
	// is not set.
	unset []byte
	// deviceKey signs the device cookies logins hand out.
	deviceKey []byte
	// lock is held on the data directory for as long as the process lives.
	lock *os.File
	st   *store.Store
	d    *dispatch.Dispatcher
	// ix is the children of the multibranch jobs, which it adds to the
	// job set it hands the dispatcher.
	ix *index.Index
	// scanner reads the multibranch jobs' repos, now and then; it is made
	// when the server runs, and ends with its work.
	scanner *index.Scanner
	// hooks are the webhook routes, made with the handler; the deliveries
	// kept while the last server was quiet are handled at start.
	hooks *webhook.Hooks
	// apps signs in as the GitHub Apps the owner jobs name.
	apps *github.Apps
	// hub is the runners, and pool the engines, a stage may be placed on;
	// runnerLn is the --runner-listen listener, nil without one.
	hub      *runnerhub.Hub
	pool     *enginepool.Pool
	runnerLn net.Listener
	// cache is the keyed caches stages restore and save; cacheURL is where
	// a stage on this machine reaches them, and runnerURLSet whether
	// --runner-url was given, which a stage on a runner needs to reach them.
	cache        *cachesrv.Service
	buildTokens  *auth.BuildTokens
	cacheURL     string
	runnerURLSet bool
	// publicURL is --base-url as it was given, empty when it was not.
	publicURL string
	// listenAddr is the address taken, the port the system picked included.
	listenAddr string

	// updates asks for newer releases; nil with --no-update-check.
	updates *update.Checker
	// storage is the report of the data directory, the compact, the
	// self-check and the clean of a disk under its floor.
	storage *storage.Service
	// upgrades carries out an upgrade or a rollback. It asks the server to
	// stop through switchOver, and serve then starts again as the launcher.
	upgrades *engine.Engine
	// args are serve's own arguments, which the restart is given again.
	args      []string
	switching atomic.Bool
	// restarting is a switch with no upgrade: the restart an admin asked for.
	restarting atomic.Bool
	stopRun    context.CancelFunc

	// startedAt is when the server began to open its data directory.
	startedAt time.Time

	// mu is held through a reload, so that of two the later one stays.
	mu        sync.Mutex
	reloadErr string
	// reloadAt is when the job files were last read: at the start, then
	// at each reload, taken or refused.
	reloadAt time.Time
}

// serve is the server; it returns once ctx has ended and everything it
// started has stopped.
func serve(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	f, err := parseServeFlags(args, stderr)
	var bad refused
	switch {
	case errors.As(err, &bad):
		return fail(stderr, "serve", err)
	case err != nil:
		return exitFor(err)
	}
	s := &server{f: f, args: args, log: slog.New(slog.NewTextHandler(stderr, nil))}
	// Before anything is read or opened: the process may become another.
	launch.Run(s.log, f.data, version.Version, args)
	ln, err := s.open(ctx)
	if err != nil {
		return fail(stderr, "serve", err)
	}
	defer s.close()
	code := s.run(ctx, ln, stdout)
	if s.switching.Load() {
		// The store and the lock first: the next process opens them.
		s.close()
		return restart(s.log, args)
	}
	return code
}

// execve is syscall.Exec, a variable so that a test can see where the
// process would have gone.
var execve = syscall.Exec

// restart is what a server does once an upgrade has written its pending
// record and the server has stopped. Under the launcher (which ran this
// process as its child) it ends with the code that has the launcher read the
// records again. Alone it becomes `kipp serve` again with the same
// arguments and the same process ID, and that start is the launcher's.
func restart(log *slog.Logger, args []string) int {
	if os.Getenv(launch.EnvSupervised) == "1" {
		return launch.ExitRestart
	}
	exe, err := os.Executable()
	if err == nil {
		err = execve(exe, append([]string{exe, "serve"}, args...), os.Environ())
	}
	log.Error("not restarting by itself; the pending record is carried out at the next start", "err", err)
	return 1
}

// switchOver stops the server for an upgrade that has written its record.
func (s *server) switchOver() {
	s.switching.Store(true)
	s.stopRun()
}

// restartNow stops the server and starts it again, the way an upgrade's
// switch does but with no record to carry out.
func (s *server) restartNow() {
	s.restarting.Store(true)
	s.switchOver()
}

func (s *server) quietCounts(q dispatch.QuietState, err error) (engine.Counts, error) {
	return engine.Counts{Quiet: q.On, Running: q.Running, Queued: q.Queued, Rerun: q.Rerun}, err
}

// newUpgrades makes the engine for this server's data directory.
func (s *server) newUpgrades() *engine.Engine {
	return engine.New(engine.Options{
		Data: s.f.data, Jobs: s.f.jobs, Version: version.Version, Checker: s.updates, Key: releaseKey(), Poll: upgradeWaitPoll,
		SetQuiet: func(ctx context.Context, on bool) (engine.Counts, error) {
			return s.quietCounts(s.d.SetQuiet(ctx, on))
		},
		Counts: func(ctx context.Context) (engine.Counts, error) { return s.quietCounts(s.d.QuietState(ctx)) },
		Switch: s.switchOver,
		Log:    s.log,
	})
}

// newStorage makes the storage of this server's data directory.
func (s *server) newStorage() *storage.Service {
	counts := func(q dispatch.QuietState, err error) (storage.Counts, error) {
		// A compact restarts nothing, so it waits for the builds a restart
		// would run again as for any other.
		return storage.Counts{Quiet: q.On, Running: q.Running + q.Rerun, Queued: q.Queued}, err
	}
	return storage.New(storage.Options{
		Store: s.st, Data: s.f.data, Snapshot: s.d.Snapshot,
		SetQuiet:  func(ctx context.Context, on bool) (storage.Counts, error) { return counts(s.d.SetQuiet(ctx, on)) },
		Counts:    func(ctx context.Context) (storage.Counts, error) { return counts(s.d.QuietState(ctx)) },
		TrimCache: func(ctx context.Context) (int, error) { return s.cache.Trim(ctx, 0, cachesrv.IdleDays*24*time.Hour) },
		Log:       s.log,
	})
}

func (s *server) close() {
	if s.st != nil {
		_ = s.st.Close()
	}
	if s.lock != nil {
		_ = s.lock.Close()
	}
}

// open reads the jobs, opens the data directory and takes the address. On
// an error nothing is left open. The jobs come first: a bad set starts
// nothing and creates nothing.
func (s *server) open(ctx context.Context) (net.Listener, error) {
	s.startedAt = time.Now()
	s.reloadAt = s.startedAt
	snap, err := config.Load(s.f.jobs)
	if err != nil {
		return nil, fmt.Errorf("the jobs directory has errors; not starting:\n%w", err)
	}
	ln, err := s.openData(ctx, snap)
	if err != nil {
		s.close()
	}
	return ln, err
}

// openData makes the data directory if it is not there, locks it, and
// opens what it holds. The lock comes before anything is read or written:
// a second server on the same directory would call the first one's running
// builds lost and remove the workspaces they run in.
func (s *server) openData(ctx context.Context, snap *config.Snapshot) (net.Listener, error) {
	var err error
	// Absolute, because a stage reads KIPP_WORKSPACE from inside it.
	if s.f.data, err = filepath.Abs(s.f.data); err != nil {
		return nil, err
	}
	if err := makeDataDir(s.f.data, s.log); err != nil {
		return nil, err
	}
	if s.lock, err = lockData(s.f.data); err != nil {
		return nil, err
	}
	if s.key, err = secretbox.LoadOrCreateKey(filepath.Join(s.f.data, keyFile)); err != nil {
		return nil, err
	}
	if s.unset, err = secretbox.Seal(s.key, unsetName, make([]byte, unsetSize)); err != nil {
		return nil, err
	}
	if s.deviceKey, err = secretbox.LoadOrCreateKey(filepath.Join(s.f.data, deviceKeyFile)); err != nil {
		return nil, err
	}
	// A launcher waiting for this process reads the file: a migration may
	// outlast its deadline for a listener.
	migrated, merr := state.MarkMigrating(s.f.data)
	if merr != nil {
		s.log.Warn("not marking the migration for the launcher", "err", merr)
	}
	s.st, err = openStore(s.f.data, true)
	migrated()
	if err != nil {
		return nil, err
	}
	logUpgrade(s.log, s.st.Upgrade())
	return s.listen(ctx, snap)
}

// logUpgrade says when the database was brought up from an older schema,
// and where the copy of it as it was is: going back to the kipp before is
// that copy, put back while no server runs.
func logUpgrade(log *slog.Logger, up store.Upgrade) {
	if up.From > 0 && up.To != up.From {
		log.Info("database upgraded", "from_schema", up.From, "to_schema", up.To, "copy", up.Backup)
	}
}

// listen takes the address, then makes the dispatcher and has it clear
// what the last process left. The address comes first, so that a server
// that cannot be reached changes nothing.
func (s *server) listen(ctx context.Context, snap *config.Snapshot) (net.Listener, error) {
	ln, err := listen("tcp", s.f.listen)
	if err != nil {
		return nil, err
	}
	// The default below is an address for stages on this machine; a status
	// links to the build's page only when the operator said where that is.
	s.publicURL = s.f.baseURL
	if s.f.baseURL == "" {
		s.f.baseURL = baseURLFor(ln.Addr())
	}
	s.listenAddr = ln.Addr().String()
	if s.f.runnerListen != "" {
		if s.runnerLn, err = listen("tcp", s.f.runnerListen); err != nil {
			_ = ln.Close()
			return nil, fmt.Errorf("--runner-listen: %w", err)
		}
	}
	s.runnerURLSet = s.f.runnerURL != ""
	s.places()
	s.cacheURL = baseURLFor(ln.Addr())
	s.cache = cachesrv.New(cachesrv.Options{
		Dir: filepath.Join(s.f.data, "cache"), Store: s.st, Max: int64(s.f.cacheMax), Log: s.log,
	})
	s.buildTokens = auth.NewBuildTokens()
	var runnerURL string // only what the operator gave: the default is no address runners are known to reach
	if s.runnerURLSet {
		runnerURL = s.f.runnerURL
	}
	s.apps = github.NewApps(s.f.githubAPI)
	if !s.f.noUpdateCheck {
		s.updates = update.New(updateOptions(version.Version, s.f.data, s.log))
	}
	s.d = dispatch.New(s.st, runner.Local{Shell: s.f.shell}, dispatch.Options{
		DataDir: s.f.data, Executors: s.f.executors, RunsNoStages: s.f.executors == 0, Builds: s.f.builds,
		BaseURL: s.f.baseURL, TimeZone: s.f.zone,
		Secrets: s.secrets, Retain: retain.Job, Logger: s.log, PassEnv: s.f.passEnv,
		Hub: s.hub, Engines: s.pool, DefaultRunsOn: s.f.defaultRunsOn, RunnerEnv: s.f.runnerEnv,
		ArtifactsMax: int64(s.f.artifactsMax),
		Cache:        s.cache, CacheURL: s.cacheURL, RunnerURL: runnerURL, BuildTokens: s.buildTokens,
		Status: index.PostStatusFor(s.f.githubAPI, s.secret, s.appClient), PublicURL: s.publicURL,
	})
	s.upgrades = s.newUpgrades()
	s.storage = s.newStorage()
	// Before anything can ask: the dispatcher has no jobs until it is given some.
	s.ix = index.New(s.st, s.d.SetSnapshot)
	if err := s.ix.SetBase(ctx, snap); err != nil {
		s.log.Error("reading the children of the multibranch jobs; they build again once a push makes them", "err", err)
	}
	s.loadPlaces(ctx)
	if err := s.d.Recover(ctx); err != nil {
		_ = ln.Close()
		if s.runnerLn != nil {
			_ = s.runnerLn.Close()
		}
		return nil, err
	}
	// Before the kept webhooks are handled (run), so that the builds the
	// last restart stopped are queued ahead of them.
	s.d.Rerun(ctx)
	return ln, nil
}

// places makes the hub of the runners and the pool of the engines.
func (s *server) places() {
	if s.f.runnerURL == "" {
		s.f.runnerURL = s.f.baseURL
	}
	s.hub = runnerhub.New(runnerhub.Options{
		Version: version.Version, Log: s.log,
		Standing: func(ctx context.Context, hash string) (*runnerhub.Identity, error) {
			a, err := s.st.RunnerByTokenHash(ctx, hash)
			if err != nil {
				return nil, err
			}
			id := identityOf(a)
			return &id, nil
		},
		Seen: func(ctx context.Context, name, os, arch, version string, at time.Time) {
			if err := s.st.SetRunnerSeen(ctx, name, at, os, arch, version); err != nil && !errors.Is(err, store.ErrNotFound) {
				s.log.Warn("recording a runner's connect", "runner", name, "err", err)
			}
		},
	})
	s.pool = enginepool.New(enginepool.Options{
		Hub: s.hub, RunnerURL: s.f.runnerURL, ServerID: s.f.baseURL, Binary: runnerbin.For, SSHKey: s.f.sshKey, Log: s.log,
	})
}

func identityOf(a *store.Runner) runnerhub.Identity {
	return runnerhub.Identity{Name: a.Name, Labels: a.Labels, Executors: a.Executors, Enabled: a.Enabled}
}

// loadPlaces gives the pool the engines and the hub the standing runners
// the database holds. What cannot be read is logged, and what the server
// had stays.
func (s *server) loadPlaces(ctx context.Context) {
	es, err := s.st.Engines(ctx)
	if err != nil {
		s.log.Error("reading the engines; the ones before stay", "err", err)
		return
	}
	as, err := s.st.Runners(ctx)
	if err != nil {
		s.log.Error("reading the runners; the ones before stay", "err", err)
		return
	}
	ids := make([]runnerhub.Identity, len(as))
	for i, a := range as {
		ids[i] = identityOf(a)
	}
	s.pool.SetEngines(es)
	s.hub.SetStanding(ids)
	s.log.Info("engines reloaded", "count", len(es), "runners", len(as))
}

// baseURLFor is where a stage on this machine reaches a server listening
// on addr. An address that means every interface (":8085", "0.0.0.0:8085")
// is none a client can call, so that one is the loopback.
func baseURLFor(addr net.Addr) string {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "http://" + addr.String()
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// secrets opens the named secrets for a build.
func (s *server) secrets(ctx context.Context, names []string) (map[string]dispatch.Secret, error) {
	out := make(map[string]dispatch.Secret, len(names))
	for _, name := range names {
		sec, err := s.openSecretRecord(ctx, name)
		if err != nil {
			return nil, err
		}
		out[name] = sec
	}
	return out, nil
}

// openSecret is secretbox.Open, which a test counts.
var openSecret = secretbox.Open

// What is opened for a secret that is not set: a value of a webhook
// secret's usual length, under a name no secret has.
const (
	unsetName = "kipp: no such secret"
	unsetSize = 40
)

// secret opens one secret. Its errors name it and never hold its value;
// one that is not set is still a store.ErrNotFound, which the webhook asks
// for. That one opens a value of the same kind first: the webhook asks for
// the secret of every name it is sent, and an answer that came sooner for
// a name without one would tell which jobs have a webhook.
func (s *server) secret(ctx context.Context, name string) (string, error) {
	sec, err := s.openSecretRecord(ctx, name)
	if sec.Kind == config.SecretFile {
		return string(sec.Data), err
	}
	return sec.Value, err
}

// openSecretRecord is secret for any kind: a file's bytes are Data, and
// a userpass secret's password is Value.
func (s *server) openSecretRecord(ctx context.Context, name string) (dispatch.Secret, error) {
	rec, err := s.st.SecretRecord(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		_, _ = openSecret(s.key, unsetName, s.unset)
		return dispatch.Secret{}, fmt.Errorf("secret %s is not set (kipp secret set %s): %w", name, name, err)
	}
	if err != nil {
		return dispatch.Secret{}, fmt.Errorf("secret %s: %w", name, err)
	}
	value, err := openSecret(s.key, name, rec.Sealed)
	if err != nil {
		return dispatch.Secret{}, fmt.Errorf("secret %s does not open with this data directory's key, or was sealed under another name: %w", name, err)
	}
	if rec.Kind == config.SecretFile {
		return dispatch.Secret{Kind: rec.Kind, Data: value, Scopes: rec.Scopes}, nil
	}
	return dispatch.Secret{Kind: rec.Kind, User: rec.User, Value: string(value), Scopes: rec.Scopes}, nil
}

// reload re-reads the jobs directory. A set with an error in it changes
// nothing: the jobs stay as they were and the error is kept for the banner.
//
// The two messages below, "jobs reloaded" and "reload refused", are read
// from the log by deploy scripts: their wording is part of what kipp
// promises. TestTheTwoReloadLinesInTheLogKeepTheirWording holds them.
func (s *server) reload(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadAt = time.Now()
	s.loadPlaces(ctx)
	snap, err := config.Load(s.f.jobs)
	if err != nil {
		s.reloadErr = err.Error()
		s.log.Error("reload refused; the previous job set stays", "err", err)
		return &api.JobsRefused{Err: err}
	}
	s.reloadErr = ""
	if err := s.ix.SetBase(ctx, snap); err != nil {
		s.log.Error("reading the children of the multibranch jobs; the ones read before stay", "err", err)
	}
	if s.scanner != nil {
		s.scanner.Reload() // a new file, or a new token, is read at once
	}
	s.log.Info("jobs reloaded", "count", len(snap.Names))
	return nil
}

func (s *server) reloadError() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reloadErr == "" {
		return ""
	}
	return "The last reload was refused, so the jobs are as they were before it. " + s.reloadErr
}

func (s *server) handler() http.Handler {
	au := auth.New(s.st, s.f.sessionTTL, s.deviceKey)
	au.UseBuildTokens(s.buildTokens)
	a := api.New(s.deps(au))
	mux := http.NewServeMux()
	mux.Handle("/api/", a.Handler())
	s.hooks = webhook.New(s.d, webhook.Options{
		Secret:  s.secret,
		Record:  func(ctx context.Context, id, job string) error { return s.st.RecordDelivery(ctx, id, job, time.Now()) },
		Keep:    s.st.KeepDelivery,
		Pending: s.st.PendingDeliveries,
		Done:    s.st.DeletePendingDelivery,
		Index:   s.ix,
		Rescan: func(job string) error {
			if s.scanner == nil {
				return nil
			}
			return s.scanner.Rescan(job)
		},
	})
	mux.Handle(webhook.Prefix, s.hooks.Handler())
	var oauthServer *oauth.Server
	if s.f.mcp {
		cfg := mcp.Config{Service: a.Service(), Auth: au, Version: version.Version, BaseURL: s.f.baseURL, Logger: s.log}
		if s.f.mcpOAuth {
			oauthServer = s.oauthServer(a)
			oauthServer.Register(mux)
			cfg.Verify = mcp.OAuthTokens(oauthServer, mcp.APITokens(au))
			cfg.ResourceMetadataURL = oauthServer.ResourceMetadataURL()
		}
		mux.Handle(mcp.Path, mcp.New(cfg).Handler())
	}
	// With a listener of their own, the runners' calls are not on this one.
	runners := s.hub.Handler()
	if s.runnerLn != nil {
		runners = problem.NotFoundHandler()
	}
	mux.Handle(runnerproto.Root, runners)
	mux.Handle(cache.Prefix, s.cache.Handler())
	data, err := filepath.Abs(s.f.data)
	if err != nil {
		data = s.f.data
	}
	mux.Handle("/", web.New(a, au, s.st, version.Version,
		web.WithApps(web.AppFlow{DataDir: data, BaseURL: s.f.baseURL, API: s.f.githubAPI}), web.WithOAuth(oauthServer))) // the pages and /static/
	return mux
}

// oauthServer is the sign-in for MCP clients. Its issuer is --base-url,
// which the flags check is an origin of the right kind.
func (s *server) oauthServer(a *api.API) *oauth.Server {
	return oauth.New(oauth.Config{
		Store: s.st, BaseURL: s.f.oauthBase, ResourcePath: mcp.Path, Logger: s.log,
		Client: func(r *http.Request) string {
			at := a.Attempt(r)
			if at.Client != "" {
				return at.Client
			}
			if host, _, err := net.SplitHostPort(at.Peer); err == nil {
				return host
			}
			return at.Peer
		},
	})
}

// deps are what the API is given of this server.
func (s *server) deps(au *auth.Service) api.Deps {
	return api.Deps{
		Auth: au, Dispatch: s.d, Store: s.st, Reload: s.reload, ReloadError: s.reloadError,
		Version: version.Version, Logger: s.log, ClientIP: s.f.clientIP,
		Seal:       func(name string, value []byte) ([]byte, error) { return secretbox.Seal(s.key, name, value) },
		ServerInfo: s.info, Listen: s.listenAddr,
		Engines: s.pool.Status, Runners: s.hub.Runners, PlacesChanged: s.loadPlaces,
		Cache: s.cache, Scanner: s.scanner, Upgrade: s.upgrades, FlakyFlips: s.f.flakyFlips,
		Storage: s.storage, Restart: s.restartNow,
	}
}

// info is what the server runs with, as of now.
func (s *server) info() api.ServerInfo {
	s.mu.Lock()
	reloadAt, reloadErr := s.reloadAt, s.reloadErr
	s.mu.Unlock()
	free, known := diskfree.Free(s.f.data)
	dbSize := int64(-1)
	if fi, err := os.Stat(filepath.Join(s.f.data, dbFile)); err == nil {
		dbSize = fi.Size()
	}
	up := update.Status{}
	if s.updates != nil {
		up = s.updates.Status()
	}
	return api.ServerInfo{
		Origin: update.ServerOrigin(), Latest: up.Latest, NotesURL: up.NotesURL, Upgradable: up.Upgradable, CheckedAt: up.CheckedAt,
		UpdateCheck: s.updates != nil,
		Version:     version.Version, GoVersion: runtime.Version(), Schema: s.st.Upgrade().To, StartedAt: s.startedAt,
		Listen: s.listenAddr, BaseURL: s.f.baseURL, Executors: s.f.executors, Builds: s.d.Builds(), Shell: s.f.shell, SessionTTL: s.f.sessionTTL,
		DefaultRunsOn: s.f.defaultRunsOn, RunnerEnv: s.f.runnerEnv, RunnerURL: s.f.runnerURL, RunnerListen: s.runnerListenAddr(),
		TimeZone: s.d.Zone(nil).String(), DataDir: s.f.data, JobsDir: s.f.jobs,
		FreeDisk: free, FreeDiskKnown: known, DBSize: dbSize, LastReloadAt: reloadAt, LastReloadError: reloadErr,
	}
}

// runnerListenAddr is the address --runner-listen took, empty without one.
func (s *server) runnerListenAddr() string {
	if s.runnerLn == nil {
		return ""
	}
	return s.runnerLn.Addr().String()
}

// hookPlan is the webhook kept on the repos of an owner job: one URL and
// one secret for the job. Without --base-url GitHub has no address to
// deliver to, and without the secret set a hook could not be verified, so
// the repos are left to the scan and the reason is shown.
func (s *server) hookPlan(ctx context.Context, owner string) index.HookPlan {
	name := webhook.SecretName(owner)
	if s.publicURL == "" {
		return index.HookPlan{Off: "serve has no --base-url, so GitHub has no address to deliver to"}
	}
	secret, err := s.secret(ctx, name)
	switch {
	case errors.Is(err, store.ErrNotFound) || err == nil && secret == "":
		return index.HookPlan{Off: "the secret " + name + " is not set (kipp secret set " + name + ")"}
	case err != nil:
		return index.HookPlan{Off: err.Error()}
	}
	return index.HookPlan{
		URL: webhook.JobURL(s.publicURL, owner), Secret: secret,
	}
}

// appClient is a client signed in as the App, for the account login.
func (s *server) appClient(ctx context.Context, app *config.App, login string) (*github.Client, error) {
	return s.apps.Get(app.ID, app.KeyFile).Client(ctx, login)
}

// run serves until ctx ends or the listener fails, then stops everything.
func (s *server) run(ctx context.Context, ln net.Listener, stdout io.Writer) int {
	// An upgrade ends the run the way a signal does.
	ctx, s.stopRun = context.WithCancel(ctx)
	defer s.stopRun()
	// Not ctx's child: stop decides when the builds end.
	work, stopWork := context.WithCancel(context.WithoutCancel(ctx))
	defer stopWork()
	s.upgrades.Resume()
	s.scanner = index.NewScanner(work, s.ix, index.ScannerOptions{API: s.f.githubAPI, Token: s.secret, Trigger: s.d.Trigger, Quiet: s.d.Quiet, Hook: s.hookPlan, Log: s.log, App: s.appClient})
	srv := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: headerTimeout,
		IdleTimeout:       idleTimeout,
		// No WriteTimeout: the long-poll answers after up to a minute and
		// a log stream stays open for as long as its build runs. No
		// ReadTimeout either: its deadline stays on the connection while
		// the handler runs, and the read that watches for a client leaving
		// would then end every stream after it. The webhook, the one route
		// that reads a large body from anyone, gives its body a deadline of
		// its own; every other body is MaxBody at most.
		ErrorLog: slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	// SIGHUP stays a reload until run returns: handed back any sooner, one
	// that arrives while the builds are ending would kill the server.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	var wg sync.WaitGroup
	s.startWork(work, hup, &wg)
	failed := make(chan error, 2)
	go func() { failed <- srv.Serve(ln) }()
	wg.Go(func() { s.hooks.Replay(work) })
	runnerSrv := s.serveRunners(failed)
	say(stdout, "kipp %s listening on http://%s, %s from %s, data in %s",
		version.Version, ln.Addr(), jobCount(len(s.d.Snapshot().Names)), s.f.jobs, s.f.data)

	code := 0
	select {
	case err := <-failed:
		s.log.Error("the HTTP server stopped", "err", err)
		code = 1
	case <-ctx.Done():
		switch {
		case s.restarting.Load():
			s.log.Info("shutting down to start again")
		case s.switching.Load():
			s.log.Info("shutting down to start the other version")
		default:
			s.log.Info("shutting down")
		}
	}
	s.stop(stopWork, &wg, srv, runnerSrv)
	return code
}

// serveRunners serves /api/v1/runner/ on --runner-listen, when there is one; its
// failure is the server's, as the main listener's is.
func (s *server) serveRunners(failed chan<- error) *http.Server {
	if s.runnerLn == nil {
		return nil
	}
	srv := &http.Server{
		Handler: s.runnerMux(), ReadHeaderTimeout: headerTimeout, IdleTimeout: idleTimeout,
		ErrorLog: slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	go func() { failed <- srv.Serve(s.runnerLn) }()
	return srv
}

// runnerMux is what --runner-listen serves: the runners' calls and the
// stages' cache, both reached with a token and nothing else.
func (s *server) runnerMux() http.Handler {
	mux := http.NewServeMux()
	mux.Handle(runnerproto.Root, s.hub.Handler())
	mux.Handle(cache.Prefix, s.cache.Handler())
	mux.Handle("/", problem.NotFoundHandler()) // nothing else is served here
	return mux
}

// startWork starts what runs beside the HTTP server until work ends: the
// queue, the cron and sweep clocks, and the reload on SIGHUP.
func (s *server) startWork(work context.Context, hup <-chan os.Signal, wg *sync.WaitGroup) {
	wg.Go(func() { s.d.Run(work) })
	wg.Go(func() { s.hub.Run(work) })
	wg.Go(func() { s.pool.Run(work) })
	wg.Go(func() { every(work, time.Now, sleep, cronAt, s.d.Tick) })
	// The sweep's half past three is in the server's zone.
	zone := s.d.Zone(nil)
	wg.Go(func() { every(work, func() time.Time { return time.Now().In(zone) }, sleep, sweepAt, s.sweep) })
	wg.Go(func() { every(work, time.Now, sleep, floorAt, s.floor) })
	// The tables are measured once at the start, so the Storage page has
	// them by the time anyone reads it.
	wg.Go(func() { _ = s.storage.Measure(work) })
	wg.Go(func() { s.reloadOn(work, hup) })
	if s.updates != nil {
		wg.Go(func() { s.updates.Run(work) })
	}
	// The first scan is at start; a job whose scan is 0 never scans on its own.
	wg.Go(func() { s.scanner.Reload(); s.scanner.Run(work) })
}

// stop ends the server's work: it takes no new connection, ends the
// builds, and lets the requests still open finish. The builds end while
// those requests are open, not after, so that whoever waits on a build is
// told it was lost instead of being cut off.
func (s *server) stop(stopWork context.CancelFunc, wg *sync.WaitGroup, srvs ...*http.Server) {
	// While the runners can still be told: a stage on one is stopped
	// through its connection.
	stopping, stopped := context.WithTimeout(context.Background(), rerunStopGrace)
	s.d.StopForRestart(stopping)
	stopped()
	ctx, giveUp := context.WithCancel(context.Background())
	defer giveUp()
	var drained sync.WaitGroup
	for _, srv := range srvs {
		if srv != nil {
			drained.Go(func() { _ = srv.Shutdown(ctx) })
		}
	}
	done := make(chan struct{})
	go func() { drained.Wait(); close(done) }()
	stopWork()
	wg.Wait() // Run returns once its builds have ended as lost
	select {
	case <-done:
	case <-time.After(drainGrace):
		giveUp()
		<-done
		for _, srv := range srvs {
			if srv != nil {
				_ = srv.Close()
			}
		}
	}
}

func (s *server) reloadOn(ctx context.Context, hup <-chan os.Signal) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			_ = s.reload(ctx) // it has logged why
		}
	}
}

// sweep is the daily retention pass. What it could not remove is logged,
// and tomorrow's pass tries again.
func (s *server) sweep(ctx context.Context, now time.Time) {
	// The gone children past their days leave the job set first, so that
	// their builds are kept as a removed job's.
	if err := s.ix.Sweep(ctx); err != nil {
		s.log.Error("sweeping the children of the multibranch jobs", "err", err)
	}
	if n, err := s.cache.Sweep(ctx); err != nil {
		s.log.Error("cache sweep", "removed", n, "err", err)
	} else if n > 0 {
		s.log.Info("cache sweep", "removed", n)
	}
	n, err := retain.Sweep(ctx, s.st, s.d.Snapshot(), s.d.LogsDir(), now)
	if err != nil {
		s.log.Error("retention sweep", "deleted", n, "err", err)
	} else {
		s.log.Info("retention sweep", "deleted", n)
	}
	s.storage.Check(ctx)
	s.floor(ctx, now)
	s.storage.MeasureSoon()
}

// floor cleans what is safe to clean when the data directory's disk is
// under its floor; it logs what it did itself.
func (s *server) floor(ctx context.Context, _ time.Time) {
	if _, err := s.storage.Heal(ctx); err != nil {
		s.log.Error("cleaning the data directory's disk under its floor", "err", err)
	}
}
