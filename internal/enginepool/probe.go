package enginepool

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/ndthuan/kipp/internal/engine/docker"
)

// errUnreachable ends the stages of an engine that stopped answering.
var errUnreachable = errors.New("the engine could not be reached")

// probeTimeout bounds one probe: an engine that does not answer in it is
// unreachable, and the probe never holds anything else up.
const probeTimeout = 15 * time.Second

// Run probes every enabled engine once each Probe, and at once when Wake
// asks, until ctx ends. A probe of one engine never waits for another's.
func (p *Pool) Run(ctx context.Context) {
	t := time.NewTicker(p.o.Probe)
	defer t.Stop()
	for {
		p.probeAll(ctx)
		select {
		case <-ctx.Done():
			p.closeAll()
			return
		case <-t.C:
		case <-p.wake:
		}
	}
}

func (p *Pool) probeAll(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, en := range p.engines {
		if !en.e.Enabled || en.probing {
			continue
		}
		en.probing = true
		go p.probe(ctx, en)
	}
}

// probe asks one engine what it is, and on an answer removes what a
// server of this name left there that no build of it now runs.
func (p *Pool) probe(ctx context.Context, en *engine) {
	defer func() { p.mu.Lock(); en.probing = false; p.mu.Unlock() }()
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	client, err := p.client(pctx, en)
	var v docker.Version
	if err == nil {
		v, err = client.Version(pctx)
	}
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		p.unreachable(en, err)
		return
	}
	p.mu.Lock()
	if !en.reachable {
		en.since = p.o.Now()
		p.o.Log.Info("engine reachable", "engine", en.e.Name, "version", v.Version, "arch", v.Arch)
	}
	en.reachable, en.probed, en.failures, en.lastErr = true, true, 0, ""
	en.os, en.arch, en.version = v.Os, v.Arch, v.Version
	p.mu.Unlock()
	p.reap(pctx, en, client)
}

func (p *Pool) unreachable(en *engine, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	en.closeClient()
	if en.reachable || !en.probed {
		en.since = p.o.Now()
		p.o.Log.Warn("engine unreachable", "engine", en.e.Name, "err", err)
	}
	en.reachable, en.probed, en.lastErr = false, true, err.Error()
	if en.failures++; en.failures != Unreachable {
		return
	}
	// Its containers are given up: their stages fail, and the containers
	// are removed by the first probe that reaches the engine again.
	for k, c := range en.containers {
		delete(en.containers, k)
		go p.o.Hub.Revoke(c.who(), errUnreachable)
	}
}

// reap removes the containers labelled with this server whose build the
// pool does not run: those a server of this name left when it stopped,
// and those of an engine that came back after its stages were given up.
func (p *Pool) reap(ctx context.Context, en *engine, client *docker.Client) {
	list, err := client.List(ctx, LabelServer+"="+p.o.ServerID)
	if err != nil {
		p.o.Log.Warn("listing an engine's containers", "engine", en.e.Name, "err", err)
		return
	}
	for _, c := range list {
		if p.owned(en, c.Labels[LabelBuild]) {
			continue
		}
		p.o.Log.Info("removing a container no build runs", "engine", en.e.Name, "container", c.ID, "build", c.Labels[LabelBuild])
		p.remove(en, client, &container{id: c.ID})
	}
}

// owned reports whether the build of that id has a container on the
// engine, or one being made anywhere.
func (p *Pool) owned(en *engine, build string) bool {
	id, err := strconv.ParseInt(build, 10, 64)
	if err != nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending[id] > 0 {
		return true
	}
	for k := range en.containers {
		if k.build == id {
			return true
		}
	}
	return false
}

func (p *Pool) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, en := range p.engines {
		en.closeClient()
	}
}
