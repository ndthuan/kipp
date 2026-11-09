// Package enginepool is the server's Docker engines: which can be reached,
// how many containers each runs, and the containers of the builds. A build
// gets one container per engine and image, started for its first stage
// there with the runner put into it, and removed when the build ends.
package enginepool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/engine/docker"
	"github.com/ndthuan/kipp/internal/placement"
	"github.com/ndthuan/kipp/internal/runnerd"
	"github.com/ndthuan/kipp/internal/runnerhub"
	"github.com/ndthuan/kipp/internal/store"
)

// The labels of a container kipp started, which a server that starts
// again lists its engines by.
const (
	LabelServer = "kipp.server"
	LabelBuild  = "kipp.build"
	LabelJob    = "kipp.job"
)

// Where the runner and the build's workspace are in a container.
const (
	RunnerPath = "/kipp/runner"
	Workspace  = "/kipp/workspace"
)

// Unreachable is how many probes in a row may fail before the stages of
// an engine's containers are given up.
const Unreachable = 3

// Options configure a Pool. Zero values take the defaults noted.
type Options struct {
	Hub *runnerhub.Hub
	// RunnerURL is what a container's runner dials; ServerID is this server
	// in its containers' labels, its base URL.
	RunnerURL, ServerID string
	// Binary is the runner for an engine's system and architecture.
	Binary func(goos, goarch string) ([]byte, error)
	// Dial reaches an engine; default docker.Dial with the engine's own
	// TLS files and ssh key, else SSHKey.
	Dial   func(ctx context.Context, e *store.Engine) (*docker.Client, error)
	SSHKey string
	Log    *slog.Logger
	Now    func() time.Time
	// Probe is how often an engine is asked whether it is there; default
	// a minute. ConnectWait is how long a new container's runner has to
	// connect; default 2 minutes.
	Probe, ConnectWait time.Duration
}

// Pool is the engines.
type Pool struct {
	o    Options
	wake chan struct{}

	mu      sync.Mutex
	engines map[string]*engine
	pending map[int64]int // builds with a container being made, so a probe leaves it alone
}

// engine is one engine as the pool knows it.
type engine struct {
	e          *store.Engine
	client     *docker.Client
	reachable  bool
	probed     bool
	since      time.Time
	lastErr    string
	failures   int
	os, arch   string
	version    string
	containers map[key]*container
	starting   int                   // containers being made, counted against Max
	making     map[key]chan struct{} // closed when the container of the key is made, or not
	ordinals   map[int64]int         // the last container number of each build here
	probing    bool
}

type key struct {
	build int64
	image string
}

type container struct {
	id, name string
	build    int64
}

// who is the container's runner, as the hub knows it.
func (c *container) who() runnerhub.Who { return runnerhub.Who{Build: c.build, Name: c.name} }

// New makes a Pool; call Run to probe its engines.
func New(o Options) *Pool {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Probe <= 0 {
		o.Probe = time.Minute
	}
	if o.ConnectWait <= 0 {
		o.ConnectWait = 2 * time.Minute
	}
	if o.Dial == nil {
		o.Dial = func(ctx context.Context, e *store.Engine) (*docker.Client, error) {
			return docker.Dial(ctx, e.URL, docker.Options{TLSCA: e.TLSCA, TLSCert: e.TLSCert, TLSKey: e.TLSKey, SSHKey: cmpOr(e.SSHKey, o.SSHKey)})
		}
	}
	return &Pool{o: o, wake: make(chan struct{}, 1), engines: map[string]*engine{}, pending: map[int64]int{}}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// SetEngines is the engines as the server's records now say. One removed
// keeps the containers it runs until their builds end, and takes no new
// one; a changed URL or key is dialled afresh.
func (p *Pool) SetEngines(es []*store.Engine) {
	p.mu.Lock()
	byName := map[string]*store.Engine{}
	for _, e := range es {
		byName[e.Name] = e
	}
	for name, en := range p.engines {
		ne := byName[name]
		if ne == nil {
			en.e = &store.Engine{Name: name, Kind: en.e.Kind, URL: en.e.URL, Labels: en.e.Labels}
			continue
		}
		if ne.URL != en.e.URL || ne.SSHKey != en.e.SSHKey || ne.TLSCA != en.e.TLSCA || ne.TLSCert != en.e.TLSCert || ne.TLSKey != en.e.TLSKey {
			en.closeClient()
			en.probed = false
		}
		en.e = ne
	}
	for name, e := range byName {
		if p.engines[name] == nil {
			p.engines[name] = &engine{e: e, containers: map[key]*container{}, making: map[key]chan struct{}{}, ordinals: map[int64]int{}}
		}
	}
	p.mu.Unlock()
	p.Wake()
}

// Wake asks for a probe of every engine now.
func (p *Pool) Wake() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (en *engine) closeClient() {
	if en.client != nil {
		_ = en.client.Close()
		en.client = nil
	}
}

// running is how many containers the engine runs or is making.
func (en *engine) running() int { return len(en.containers) + en.starting }

// Candidates implements dispatch.EngineSource.
func (p *Pool) Candidates(build int64, image string) []placement.Candidate {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []placement.Candidate
	asleep := false
	for name, en := range p.engines {
		if !en.e.Enabled {
			continue
		}
		k := key{build, image}
		p.dropDead(en, k)
		room := max(en.e.Max-en.running(), 0)
		reuse := en.containers[k] != nil || en.making[k] != nil
		out = append(out, placement.Candidate{
			Name: name, Kind: placement.KindEngine, Labels: slices.Clone(en.e.Labels),
			Free: room > 0, Room: room, Reuse: reuse, Reachable: en.reachable, Why: en.why(),
		})
		asleep = asleep || !en.reachable
	}
	if asleep {
		p.Wake() // a stage waits for it: ask again now rather than in a minute
	}
	return out
}

// why says how an engine that cannot be reached failed, and since when.
func (en *engine) why() string {
	if !en.probed {
		return "not asked yet"
	}
	return fmt.Sprintf("unreachable since %s: %s", en.since.Format("15:04"), en.lastErr)
}

// Acquire implements dispatch.EngineSource. A stage that asks while the
// container of its build and image is being made waits for that one.
func (p *Pool) Acquire(ctx context.Context, b *store.Build, engineName, image string, out io.Writer) (string, error) {
	for {
		sl, err := p.reserve(b.ID, engineName, image)
		switch {
		case err != nil:
			return "", err
		case sl.c != nil:
			return sl.c.name, nil
		case sl.wait != nil:
			select {
			case <-sl.wait:
				continue
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		return p.make(ctx, sl, b, image, out)
	}
}

// slot is what reserve found: the build's container of the image, one
// being made to wait for, or a place for a new one whose runner will be
// called name.
type slot struct {
	en   *engine
	c    *container
	wait <-chan struct{}
	name string
}

// make starts the container of sl and records it as the build's.
func (p *Pool) make(ctx context.Context, sl slot, b *store.Build, image string, out io.Writer) (string, error) {
	k := key{b.ID, image}
	defer p.unreserve(sl.en, k)
	c, err := p.start(ctx, sl.en, b, sl.name, image, out)
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	sl.en.containers[k] = c
	p.mu.Unlock()
	return c.name, nil
}

// reserve is the build's container of image on the engine, or the one
// being made, or a place for a new one counted against the engine's cap,
// and the name its runner will have: the engine's name and the container's
// number in the build.
func (p *Pool) reserve(build int64, name, image string) (slot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	en := p.engines[name]
	if en == nil || !en.e.Enabled {
		return slot{}, fmt.Errorf("%w: engine %s is not enabled", runnerhub.ErrNoRunner, name)
	}
	k := key{build, image}
	p.dropDead(en, k)
	if c := en.containers[k]; c != nil {
		return slot{en: en, c: c}, nil
	}
	if w := en.making[k]; w != nil {
		return slot{en: en, wait: w}, nil
	}
	if en.running() >= en.e.Max || !en.reachable {
		return slot{}, fmt.Errorf("%w: engine %s is full or cannot be reached", runnerhub.ErrNoRunner, name)
	}
	en.starting++
	en.making[k] = make(chan struct{})
	p.pending[build]++
	en.ordinals[build]++
	return slot{en: en, name: fmt.Sprintf("%s/%d", name, en.ordinals[build])}, nil
}

func (p *Pool) unreserve(en *engine, k key) {
	p.mu.Lock()
	en.starting--
	close(en.making[k])
	delete(en.making, k)
	if p.pending[k.build]--; p.pending[k.build] <= 0 {
		delete(p.pending, k.build)
	}
	p.mu.Unlock()
}

// dropDead forgets the container of k when its runner is no longer
// connected (killed, or the container stopped), so that the build's next
// stage there gets a new one; what the dead one ran has failed already.
// It is removed meanwhile. p.mu is held.
func (p *Pool) dropDead(en *engine, k key) {
	c := en.containers[k]
	if c == nil || p.o.Hub.Free(c.who()) {
		return
	}
	delete(en.containers, k)
	p.o.Log.Warn("a container's runner is gone; the build's next stage gets another", "engine", en.e.Name, "runner", c.name)
	go p.discard(en, c)
}

// discard ends the token of c's runner and removes c.
func (p *Pool) discard(en *engine, c *container) {
	p.o.Hub.Revoke(c.who(), runnerhub.ErrOffline)
	client, err := p.client(context.Background(), en)
	if err != nil {
		p.o.Log.Warn("a container whose runner is gone is left until its engine answers", "engine", en.e.Name, "container", c.id, "err", err)
		return
	}
	p.remove(en, client, c)
}

// start makes the container: the image, the runner put into it, a token
// for this build alone, and waits until its runner has connected.
func (p *Pool) start(ctx context.Context, en *engine, b *store.Build, name, image string, out io.Writer) (*container, error) {
	client, err := p.client(ctx, en)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	goos, goarch := en.os, en.arch
	p.mu.Unlock()
	bin, err := p.o.Binary(goos, goarch)
	if err != nil {
		return nil, err
	}
	if err := client.Pull(ctx, image, out); err != nil {
		return nil, fmt.Errorf("pulling %s: %w", image, err)
	}
	token, err := p.o.Hub.Grant(b.ID, name)
	if err != nil {
		return nil, err
	}
	id, err := client.Create(ctx, docker.CreateSpec{
		Image: image, Cmd: []string{RunnerPath, "--url", p.o.RunnerURL, "--workdir", Workspace},
		Env:        []string{runnerd.TokenEnv + "=" + token},
		Labels:     map[string]string{LabelServer: p.o.ServerID, LabelBuild: strconv.FormatInt(b.ID, 10), LabelJob: b.Job},
		WorkingDir: Workspace,
	})
	if err != nil {
		p.o.Hub.Revoke(runnerhub.Who{Build: b.ID, Name: name}, err)
		return nil, fmt.Errorf("creating a container of %s: %w", image, err)
	}
	c := &container{id: id, name: name, build: b.ID}
	if err := p.boot(ctx, client, c, bin, out); err != nil {
		p.o.Hub.Revoke(runnerhub.Who{Build: b.ID, Name: name}, err)
		p.remove(en, client, c)
		return nil, err
	}
	return c, nil
}

// boot puts the runner into the container, starts it, and waits until the
// runner connects, or the container stops, or ConnectWait passes. What the
// runner said goes to out when it never connected.
func (p *Pool) boot(ctx context.Context, client *docker.Client, c *container, bin []byte, out io.Writer) error {
	if err := client.PutFile(ctx, c.id, RunnerPath, 0o755, bin); err != nil {
		return fmt.Errorf("putting the runner into the container: %w", err)
	}
	if err := client.Start(ctx, c.id); err != nil {
		return fmt.Errorf("starting the container: %w", err)
	}
	stopped := make(chan error, 1)
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		code, err := client.Wait(wctx, c.id)
		if err == nil {
			err = fmt.Errorf("the container exited with %d before its runner connected", code)
		}
		stopped <- err
	}()
	deadline := time.NewTimer(p.o.ConnectWait)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for !p.o.Hub.Free(c.who()) {
		var why error
		select {
		case <-tick.C:
			continue
		case why = <-stopped:
		case <-deadline.C:
			why = fmt.Errorf("its runner did not connect within %s", p.o.ConnectWait)
		case <-ctx.Done():
			return ctx.Err()
		}
		_, _ = fmt.Fprintf(out, "kipp: the container's runner said:\n")
		_ = client.Logs(context.WithoutCancel(ctx), c.id, out)
		return why
	}
	return nil
}

// client is the engine's connection, made when there is none.
func (p *Pool) client(ctx context.Context, en *engine) (*docker.Client, error) {
	p.mu.Lock()
	c, e := en.client, en.e
	p.mu.Unlock()
	if c != nil {
		return c, nil
	}
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c, err := p.o.Dial(dctx, e)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if en.client != nil {
		_ = c.Close()
		return en.client, nil
	}
	en.client = c
	return c, nil
}

// EndBuild implements dispatch.EngineSource: the build's containers are
// killed and removed.
func (p *Pool) EndBuild(ctx context.Context, build int64) {
	p.mu.Lock()
	type gone struct {
		en *engine
		c  *container
	}
	var ending []gone
	for _, en := range p.engines {
		for k, c := range en.containers {
			if k.build == build {
				ending = append(ending, gone{en, c})
				delete(en.containers, k)
			}
		}
		delete(en.ordinals, build)
	}
	p.mu.Unlock()
	for _, g := range ending {
		client, err := p.client(ctx, g.en)
		if err != nil {
			p.o.Log.Warn("a container of an ended build is left until its engine answers", "engine", g.en.e.Name, "container", g.c.id, "err", err)
			continue
		}
		p.remove(g.en, client, g.c)
	}
}

func (p *Pool) remove(en *engine, client *docker.Client, c *container) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := errors.Join(client.Kill(ctx, c.id), client.Remove(ctx, c.id)); err != nil {
		p.o.Log.Warn("removing a container", "engine", en.e.Name, "container", c.id, "err", err)
	}
}

// EngineStatus is what may be shown of an engine.
type EngineStatus struct {
	Name, Kind, URL string // the URL with no password, which it never holds anyway
	Labels          []string
	Max             int
	Enabled         bool
	Reachable       bool
	Since           time.Time
	LastError       string
	Version         string
	Running         []string // the containers' runners
}

// Status is every engine, by name.
func (p *Pool) Status() []EngineStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]EngineStatus, 0, len(p.engines))
	for _, name := range slices.Sorted(maps.Keys(p.engines)) {
		en := p.engines[name]
		st := EngineStatus{
			Name: name, Kind: en.e.Kind, URL: Redact(en.e.URL), Labels: slices.Clone(en.e.Labels), Max: en.e.Max,
			Enabled: en.e.Enabled, Reachable: en.reachable, Since: en.since, LastError: en.lastErr, Version: en.version,
		}
		for _, c := range en.containers {
			st.Running = append(st.Running, c.name)
		}
		slices.Sort(st.Running)
		out = append(out, st)
	}
	return out
}

// Redact is an engine URL with any password taken out: one is refused
// when an engine is added, and this keeps a record made by hand from
// showing one.
func Redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if _, ok := u.User.Password(); ok {
		u.User = url.User(u.User.Username())
	}
	return u.String()
}

// Check refuses an engine kipp could not dial or choose by: a name that
// is not one, no label, a cap below one, a URL docker.CheckURL refuses.
func Check(e *store.Engine) error {
	switch {
	case !config.ValidName(e.Name):
		return fmt.Errorf("engine %q: lower-case letters, digits, dot, dash and underscore only", e.Name)
	case e.Kind != "docker":
		return fmt.Errorf("engine %s: kind %q; docker is the one kind", e.Name, e.Kind)
	case len(e.Labels) == 0:
		return fmt.Errorf("engine %s: one or more labels, which a stage's runs_on chooses it by", e.Name)
	case e.Max < 1:
		return fmt.Errorf("engine %s: max is at least 1", e.Name)
	}
	for _, l := range e.Labels {
		if !config.ValidLabel(l) {
			return fmt.Errorf("engine %s: label %q: lower-case letters, digits, dot, dash and underscore, starting with a letter or digit", e.Name, l)
		}
	}
	_, err := docker.CheckURL(e.URL, docker.Options{TLSCA: e.TLSCA, TLSCert: e.TLSCert, TLSKey: e.TLSKey})
	return err
}
