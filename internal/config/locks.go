package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

func checkLockList(key string, names []string) error {
	for i, n := range names {
		switch {
		case strings.TrimSpace(n) == "":
			return fmt.Errorf("%s: a lock has a name", key)
		case slices.Contains(names[:i], n):
			return fmt.Errorf("%s: lock %s twice", key, n)
		}
	}
	return nil
}

func (j *Job) checkLocks() error {
	if j.StageLocks == nil {
		return nil
	}
	if j.Multibranch == nil && j.Owner == nil && j.Source == nil {
		return errors.New("stage_locks: it names the locks the stages file of a repo may take, and this job reads none (it has no source, multibranch or owner); a stage of the job file takes any lock")
	}
	return checkLockList("stage_locks", j.StageLocks)
}

// CheckStageLocks refuses a stage of a stages file in a repo that takes a
// lock the job does not grant in stage_locks: a pushed branch could
// otherwise hold back whatever else takes that lock.
func (j *Job) CheckStageLocks(stages []*Stage) error {
	for _, s := range stages {
		for _, l := range s.Locks {
			if !slices.Contains(j.StageLocks, l) {
				return fmt.Errorf("stage %s: lock %s is not one the job grants a stages file (stage_locks)", s.Name, l)
			}
		}
	}
	return nil
}
