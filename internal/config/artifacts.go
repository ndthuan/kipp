package config

import (
	"fmt"
	"io/fs"
	"path"
	"slices"
)

// The limits of what one stage keeps.
const (
	DefaultArtifactsMax = 1 * GiB
	MostArtifactsMax    = 10 * GiB
)

// StageArtifacts are the files a stage keeps with its build.
type StageArtifacts struct {
	Paths   []string `yaml:"paths"`
	MaxSize ByteSize `yaml:"max_size"`
}

// Max is the most the stage's matches may come to, in bytes.
func (a *StageArtifacts) Max() int64 {
	if a.MaxSize == 0 {
		return int64(DefaultArtifactsMax)
	}
	return int64(a.MaxSize)
}

func (a *StageArtifacts) check(stage string) error {
	if a == nil {
		return nil
	}
	if len(a.Paths) == 0 {
		return fmt.Errorf("stage %s: artifacts.paths: one or more globs of the files to keep, like dist/* or report.html", stage)
	}
	for _, p := range a.Paths {
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("stage %s: artifacts.paths: %q is not a pattern path.Match reads", stage, p)
		}
		if !fs.ValidPath(p) || p == "." {
			return fmt.Errorf("stage %s: artifacts.paths: %q leaves the stage's directory: a path inside it, like dist/*", stage, p)
		}
	}
	if a.MaxSize > MostArtifactsMax {
		return fmt.Errorf("stage %s: artifacts.max_size %s is over the most a stage keeps, %s", stage, a.MaxSize, MostArtifactsMax)
	}
	return nil
}

func (s *Stage) checkArtifactsFrom(needs map[string][]string) error {
	before := upstream(s.Name, needs)
	for i, from := range s.ArtifactsFrom {
		switch {
		case slices.Contains(s.ArtifactsFrom[:i], from):
			return fmt.Errorf("stage %s: artifacts_from names %s twice", s.Name, from)
		case !before[from]:
			return fmt.Errorf("stage %s: artifacts_from: %q is not a stage it needs, directly or through others", s.Name, from)
		}
	}
	return nil
}

func upstream(name string, needs map[string][]string) map[string]bool {
	seen := map[string]bool{}
	next := slices.Clone(needs[name])
	for len(next) > 0 {
		n := next[len(next)-1]
		next = next[:len(next)-1]
		if !seen[n] {
			seen[n] = true
			next = append(next, needs[n]...)
		}
	}
	return seen
}
