package storage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// CheckFile is where the last self-check is kept in the data directory, so
// that a restart still shows a failure.
const CheckFile = "self-check.json"

// Check is a self-check of the database: PRAGMA quick_check.
type Check struct {
	At time.Time `json:"at"`
	OK bool      `json:"ok"`
	// Problems are what quick_check found wrong; Error is why it could not
	// run, which is a failure too.
	Problems []string `json:"problems"`
	Error    string   `json:"error"`
}

// maxProblems bounds what a check keeps of what it found: the first lines
// say enough to act on, and a broken file can yield thousands.
const maxProblems = 20

// Check runs the self-check now, keeps its result and says a failure in
// the log at ERROR. It runs on the read-only connection and does not hold
// quiet. kipp never restores a backup by itself: that would lose every
// build newer than it without a word.
func (s *Service) Check(ctx context.Context) Check {
	problems, err := s.o.Store.QuickCheck(ctx)
	c := Check{At: s.o.Now(), OK: err == nil && len(problems) == 0, Problems: problems}
	if len(c.Problems) > maxProblems {
		c.Problems = c.Problems[:maxProblems]
	}
	if err != nil {
		c.Error = err.Error()
	}
	if c.OK {
		s.o.Log.Info("self-check of the database passed")
	} else {
		s.o.Log.Error("self-check of the database FAILED: restore it by hand from a backup (docs/operations.md)", "problems", c.Problems, "err", c.Error)
	}
	s.mu.Lock()
	s.check = &c
	s.mu.Unlock()
	if err := writeCheck(s.o.Data, c); err != nil {
		s.o.Log.Error("keeping the result of the self-check", "err", err)
	}
	return c
}

// LastCheck is the last self-check, nil before any.
func (s *Service) LastCheck() *Check {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.check
}

func readCheck(data string) *Check {
	root, err := os.OpenRoot(data)
	if err != nil {
		return nil
	}
	defer func() { _ = root.Close() }()
	b, err := root.ReadFile(CheckFile)
	if err != nil {
		return nil
	}
	var c Check
	if json.Unmarshal(b, &c) != nil || c.At.IsZero() {
		return nil
	}
	return &c
}

// writeCheck writes the file through a rename, so a reader sees the old
// result or the new.
func writeCheck(data string, c Check) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(data, "."+CheckFile+"-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(b)
	if err := tmp.Close(); werr == nil {
		werr = err
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), filepath.Join(data, CheckFile))
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
	}
	return werr
}
