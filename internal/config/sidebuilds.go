package config

import "errors"

type sideBuildsFile struct {
	Params map[string][]string `yaml:"params"`
}

func (j *Job) setSideBuilds(f *sideBuildsFile) error {
	if f == nil {
		return nil
	}
	if len(f.Params) == 0 {
		return errors.New("side_builds: params names at least one param")
	}
	if err := checkParamValues("side_builds.params", f.Params, j.Params); err != nil {
		return err
	}
	rule := &When{Params: f.Params}
	defaults := make(map[string]string, len(j.Params))
	for _, p := range j.Params {
		defaults[p.Name] = p.Default
	}
	if rule.Holds(Push{Params: defaults}) {
		return errors.New("side_builds: a build with every param at its default is one, so every ordinary build would be")
	}
	j.SideBuilds = rule
	return nil
}

// IsSideBuild reports whether a build of the job with these params, defaults
// filled in, is a side build: one that says nothing to GitHub and starts no
// job that runs after this one.
func (j *Job) IsSideBuild(params map[string]string) bool {
	return j.SideBuilds != nil && j.SideBuilds.Holds(Push{Params: params})
}
