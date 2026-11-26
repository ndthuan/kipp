package dispatch

import (
	"fmt"
	"maps"
	"slices"

	"github.com/ndthuan/kipp/internal/config"
)

func stageEnv(s *config.Stage, params map[string]string) []string {
	var env []string
	for _, k := range slices.Sorted(maps.Keys(s.Env)) {
		if !has(params, k) {
			env = append(env, k+"="+s.Env[k])
		}
	}
	return env
}

func (e *buildEnv) declared() []string {
	names := slices.Sorted(maps.Keys(e.fileEnv))
	for _, v := range e.vars {
		names = append(names, v.Name)
	}
	return names
}

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
