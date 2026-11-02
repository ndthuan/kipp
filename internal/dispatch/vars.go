package dispatch

import (
	"fmt"
	"maps"
	"slices"

	"github.com/ndthuan/kipp/internal/config"
)

// stageEnv is a stage's own env:, as KEY=value, after every other variable
// of the build but for the params, which a stage's env: does not override.
func stageEnv(s *config.Stage, params map[string]string) []string {
	var env []string
	for _, k := range slices.Sorted(maps.Keys(s.Env)) {
		if !has(params, k) {
			env = append(env, k+"="+s.Env[k])
		}
	}
	return env
}

// declared are the names the build is given from outside the job file's
// own env: the _vars.yml entries that reach it and the stages file's.
func (e *buildEnv) declared() []string {
	names := slices.Sorted(maps.Keys(e.fileEnv))
	for _, v := range e.vars {
		names = append(names, v.Name)
	}
	return names
}

// checkVars refuses a build whose _vars.yml entries cannot be made (two
// patterns setting one name for it) or give a name a secret of the job
// has. The reload leaves both to the build where only a child, a repo job
// or the shared global and group entries show them.
func checkVars(job *config.Job, e *buildEnv, secrets map[string]Secret) error {
	if e.varsErr != nil {
		return e.varsErr
	}
	var names []string
	for _, v := range e.vars {
		names = append(names, v.Name)
	}
	if err := job.CheckVarNames(config.VarsFile, names); err != nil {
		return err
	}
	return checkDerivedDeclared(secrets, config.VarsFile, names)
}

// checkFile refuses a stages file whose env: names what a job file's may
// not: a secret of the job, a name of a user and password secret.
func checkFile(job *config.Job, e *buildEnv, secrets map[string]Secret, path string) error {
	if err := job.CheckEnv(path, e.fileEnv); err != nil {
		return err
	}
	if err := job.CheckStagesEnv(path, e.stages); err != nil {
		return err
	}
	names := slices.Sorted(maps.Keys(e.fileEnv))
	for _, s := range e.stages {
		names = append(names, slices.Sorted(maps.Keys(s.Env))...)
	}
	return checkDerivedDeclared(secrets, path, names)
}

// checkDerivedDeclared refuses names a file declares that a user and
// password secret gives too, as NAME_USR and NAME_PSW.
func checkDerivedDeclared(secrets map[string]Secret, where string, names []string) error {
	for _, name := range slices.Sorted(maps.Keys(secrets)) {
		if secrets[name].Kind != config.SecretUserPass {
			continue
		}
		for _, derived := range config.DerivedSecretNames(name) {
			if slices.Contains(names, derived) {
				return fmt.Errorf("secrets: %s is the user and password secret %s's, and %s gives it too", derived, name, where)
			}
		}
	}
	return nil
}
