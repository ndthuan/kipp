package storage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/retain"
	"github.com/ndthuan/kipp/internal/store"
)

// ErrHasFile refuses to remove the data of a job that still has a job file:
// its keep rule is the way to hold its size.
var ErrHasFile = errors.New("the job has a job file")

// ErrNothingLeft is a job with no build left to remove.
var ErrNothingLeft = errors.New("nothing of the job is left to remove")

// RemoveJob is "remove now": it removes the ended builds of a job whose
// file is gone, with their logs and artifacts, as the retention would 90
// days after the newest, and is how many it removed. A build kept by hand
// stays. One row of the audit says who did it.
func (s *Service) RemoveJob(ctx context.Context, actor, name string) (int, error) {
	if _, ok := s.snapshot().Jobs[name]; ok {
		return 0, fmt.Errorf("%w: %s; set its keep instead", ErrHasFile, name)
	}
	ids, err := s.o.Store.EndedBuilds(ctx, name)
	if err != nil {
		return 0, err
	}
	logs := filepath.Join(s.o.Data, "logs")
	if len(ids) == 0 && treeSize(filepath.Join(logs, name)) == 0 && treeSize(filepath.Join(retain.ArtifactsDir(logs), name)) == 0 {
		return 0, fmt.Errorf("%w: %s", ErrNothingLeft, name)
	}
	n, err := retain.Remove(ctx, s.o.Store, name, logs)
	detail := fmt.Sprintf("%d builds removed by hand, its job file gone", n)
	if err != nil {
		detail += "; " + err.Error()
	}
	if aerr := s.o.Store.AppendAudit(ctx, store.Audit{At: s.o.Now(), Actor: actor, Action: "storage.remove-job", Target: name, Detail: detail}); aerr != nil {
		err = errors.Join(err, aerr)
	}
	return n, err
}

// snapshot is the job set, an empty one before the server has one.
func (s *Service) snapshot() *config.Snapshot {
	if s.o.Snapshot != nil {
		if snap := s.o.Snapshot(); snap != nil {
			return snap
		}
	}
	return &config.Snapshot{Jobs: map[string]*config.Job{}}
}
