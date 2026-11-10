package storage

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/ndthuan/kipp/internal/store"
)

// The kinds of a HealStep, in the order Heal takes them.
const (
	HealStageTemp = "stage_temp" // KIPP_TMP of a stage of a build that has ended
	HealWorkspace = "workspace"  // the workspaces of a job whose file is gone
	HealCache     = "cache"      // cache entries past their age
)

// Heal is a clean of the data directory's disk, which runs when its free
// space is under the floor.
type Heal struct {
	At                    time.Time
	Floor                 int64
	FreeBefore, FreeAfter int64
	Steps                 []HealStep
	FloorMet              bool
}

// HealStep is one thing a clean removed.
type HealStep struct {
	Kind string
	// Path is what was removed, under the data directory.
	Path  string
	Bytes int64
	Error string
}

// Freed is the bytes the clean removed.
func (h *Heal) Freed() int64 {
	var n int64
	for _, s := range h.Steps {
		n += s.Bytes
	}
	return n
}

// LastHeal is the last clean, nil before any.
func (s *Service) LastHeal() *Heal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.heal
}

// Heal cleans, when the disk of the data directory is under its floor,
// what is safe to remove, in this order and until the floor is met: the
// stage temp left by builds that have ended, the workspaces of jobs whose
// file is gone, and the cache entries past their age. It never touches a
// build's logs or artifacts, nor the database. It returns nil when the
// disk is over the floor or its space cannot be read.
func (s *Service) Heal(ctx context.Context) (*Heal, error) {
	free, total, ok := s.space(s.o.Data)
	if !ok || free >= floor(total) {
		return nil, nil
	}
	running, err := s.o.Store.BuildsWithStatus(ctx, store.Running)
	if err != nil {
		return nil, err
	}
	h := &Heal{At: s.o.Now(), Floor: floor(total), FreeBefore: free}
	s.o.Log.Warn("the data directory's disk is under its floor; cleaning what is safe to clean", "free", free, "floor", h.Floor)
	met := func() bool {
		free, _, _ = s.space(s.o.Data)
		return free >= h.Floor
	}
	for _, step := range []func(context.Context, []*store.Build) []HealStep{s.healStageTemp, s.healWorkspaces, s.healCache} {
		for _, st := range step(ctx, running) {
			h.Steps = append(h.Steps, st)
			s.o.Log.Warn("disk floor: removed", "kind", st.Kind, "path", st.Path, "bytes", st.Bytes, "err", st.Error)
		}
		if h.FloorMet = met(); h.FloorMet {
			break
		}
	}
	h.FreeAfter = free
	s.o.Log.Warn("disk floor: clean done", "freed", h.Freed(), "free", h.FreeAfter, "floor", h.Floor, "floor_met", h.FloorMet)
	s.mu.Lock()
	s.heal = h
	s.mu.Unlock()
	return h, nil
}

// removeStep removes path under the data directory as one step of kind.
func (s *Service) removeStep(kind, path string) HealStep {
	full := filepath.Join(s.o.Data, path)
	st := HealStep{Kind: kind, Path: path, Bytes: treeSize(full)}
	if err := os.RemoveAll(full); err != nil {
		st.Error = err.Error()
	}
	return st
}

// healStageTemp removes the folders in stage-tmp/ older than the oldest
// running build: a stage makes its folder when it starts, and a write in
// it only makes the folder newer, so an older one belongs to a build that
// has ended.
func (s *Service) healStageTemp(_ context.Context, running []*store.Build) []HealStep {
	var oldest time.Time
	for _, b := range running {
		at := b.QueuedAt
		if b.StartedAt != nil {
			at = *b.StartedAt
		}
		if oldest.IsZero() || at.Before(oldest) {
			oldest = at
		}
	}
	entries, _ := os.ReadDir(filepath.Join(s.o.Data, "stage-tmp"))
	var out []HealStep
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil || (!oldest.IsZero() && !fi.ModTime().Before(oldest)) {
			continue
		}
		out = append(out, s.removeStep(HealStageTemp, filepath.Join("stage-tmp", e.Name())))
	}
	return out
}

// healWorkspaces removes workspace/<job> of each job with no job file and
// no running build.
func (s *Service) healWorkspaces(_ context.Context, running []*store.Build) []HealStep {
	busy := map[string]bool{}
	for _, b := range running {
		busy[b.Job] = true
	}
	snap := s.snapshot()
	entries, _ := os.ReadDir(filepath.Join(s.o.Data, "workspace"))
	var out []HealStep
	for _, e := range entries {
		if _, known := snap.Jobs[e.Name()]; known || busy[e.Name()] || !e.IsDir() {
			continue
		}
		out = append(out, s.removeStep(HealWorkspace, filepath.Join("workspace", e.Name())))
	}
	return out
}

// healCache removes the cache entries past their age.
func (s *Service) healCache(ctx context.Context, _ []*store.Build) []HealStep {
	if s.o.TrimCache == nil {
		return nil
	}
	dir := filepath.Join(s.o.Data, "cache")
	before := treeSize(dir)
	n, err := s.o.TrimCache(ctx)
	if n == 0 && err == nil {
		return nil
	}
	st := HealStep{Kind: HealCache, Path: "cache", Bytes: max(0, before-treeSize(dir))}
	if err != nil {
		st.Error = err.Error()
	}
	return []HealStep{st}
}
