package dispatch

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"

	"github.com/ndthuan/kipp/internal/config"
	"github.com/ndthuan/kipp/internal/runner"
	"github.com/ndthuan/kipp/internal/runnerproto"
	"github.com/ndthuan/kipp/internal/store"
)

// Secret is a secret as a build is given it, opened. Kind is one of
// config.SecretText, SecretFile and SecretUserPass.
type Secret struct {
	Kind   string
	Value  string
	User   string
	Data   []byte
	Scopes []string
}

func checkScopes(job *config.Job, secrets map[string]Secret) error {
	for _, name := range slices.Sorted(maps.Keys(secrets)) {
		if !config.ScopesReach(secrets[name].Scopes, job) {
			return fmt.Errorf("kipp: secret %s is not in scope for job %s", name, job.Name)
		}
	}
	return nil
}

func secretVars(secrets map[string]Secret) []string {
	var env []string
	for _, name := range slices.Sorted(maps.Keys(secrets)) {
		switch sec := secrets[name]; sec.Kind {
		case config.SecretFile:
		case config.SecretUserPass:
			env = append(env, name+"="+sec.User+":"+sec.Value, name+"_USR="+sec.User, name+"_PSW="+sec.Value)
		default:
			env = append(env, name+"="+sec.Value)
		}
	}
	return env
}

func secretMasks(secrets map[string]Secret) []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(secrets)) {
		switch sec := secrets[name]; sec.Kind {
		case config.SecretFile:
			out = append(out, config.SecretMasks(sec.Data)...)
		case config.SecretUserPass:
			out = append(out, sec.Value, sec.User+":"+sec.Value)
		default:
			out = append(out, sec.Value)
		}
	}
	return out
}

func secretFileData(secrets map[string]Secret) map[string][]byte {
	var files map[string][]byte
	for name, sec := range secrets {
		if sec.Kind == config.SecretFile {
			if files == nil {
				files = map[string][]byte{}
			}
			files[name] = sec.Data
		}
	}
	return files
}

func checkDerivedNames(b *store.Build, job *config.Job, secrets map[string]Secret) error {
	for _, name := range slices.Sorted(maps.Keys(secrets)) {
		if secrets[name].Kind != config.SecretUserPass {
			continue
		}
		for _, derived := range config.DerivedSecretNames(name) {
			switch {
			case has(job.Env, derived):
				return fmt.Errorf("secrets: %s is the user and password secret %s's, and env: gives it too", derived, name)
			case has(b.Params, derived):
				return fmt.Errorf("secrets: %s is the user and password secret %s's, and a param is called so too", derived, name)
			case has(secrets, derived):
				return fmt.Errorf("secrets: %s is the user and password secret %s's, and a secret is called so too", derived, name)
			}
		}
	}
	return nil
}

func has[V any](m map[string]V, k string) bool {
	_, ok := m[k]
	return ok
}

func (d *Dispatcher) secretFilesDir() string {
	return filepath.Join(d.opts.DataDir, "secret-files")
}

func (d *Dispatcher) secretFilesFor(e *buildEnv) ([]string, func(), error) {
	if len(e.files) == 0 {
		return nil, func() {}, nil
	}
	return runner.WriteSecretFiles(d.secretFilesDir(), e.files)
}

func (d *Dispatcher) stageTmpDir() string {
	return filepath.Join(d.opts.DataDir, "stage-tmp")
}

func (d *Dispatcher) commandFiles(e *buildEnv) ([]string, func(), error) {
	files, removeFiles, err := d.secretFilesFor(e)
	if err != nil {
		return nil, nil, err
	}
	tmp, removeTmp, err := runner.StageTemp(d.stageTmpDir())
	if err != nil {
		removeFiles()
		return nil, nil, fmt.Errorf("could not make the stage's KIPP_TMP: %w", err)
	}
	return append(files, "KIPP_TMP="+tmp), func() { removeTmp(); removeFiles() }, nil
}

func remoteSecretFiles(e *buildEnv) []runnerproto.SecretFile {
	var out []runnerproto.SecretFile
	for _, name := range slices.Sorted(maps.Keys(e.files)) {
		out = append(out, runnerproto.SecretFile{Name: name, Data: e.files[name]})
	}
	return out
}
