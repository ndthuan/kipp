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
	Kind string
	// Value is a text secret's value, or a userpass secret's password.
	Value string
	// User is a userpass secret's user.
	User string
	// Data is a file secret's bytes.
	Data []byte
	// Scopes are the jobs that may list the secret; a build of any other
	// job is refused before a stage starts.
	Scopes []string
}

// checkScopes refuses a job that lists a secret whose scopes do not reach
// it. The message names the secret and the job, never a value.
func checkScopes(job *config.Job, secrets map[string]Secret) error {
	for _, name := range slices.Sorted(maps.Keys(secrets)) {
		if !config.ScopesReach(secrets[name].Scopes, job) {
			return fmt.Errorf("kipp: secret %s is not in scope for job %s", name, job.Name)
		}
	}
	return nil
}

// secretVars are the variables the secrets give a stage: a text secret
// its value, a userpass secret NAME as user:password and NAME_USR and
// NAME_PSW. A file secret gives none here: its path is only known once the
// file is written for a stage.
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

// secretMasks are the strings the logs must not show of the secrets. A
// userpass user is a plain word more often than not and is shown; a file
// is masked line by line when it is text.
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

// secretFileData are the bytes of the file secrets, by name; nil for none.
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

// checkDerivedNames refuses a job whose env, params or secrets give a
// variable a userpass secret gives too.
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

// secretFilesDir is where the files of the secrets in use are written,
// outside every workspace.
func (d *Dispatcher) secretFilesDir() string {
	return filepath.Join(d.opts.DataDir, "secret-files")
}

// secretFilesFor writes the build's file secrets for one command of the
// server's own, and returns the variables naming them and the func that
// removes them; the func is safe to run when there are none.
func (d *Dispatcher) secretFilesFor(e *buildEnv) ([]string, func(), error) {
	if len(e.files) == 0 {
		return nil, func() {}, nil
	}
	return runner.WriteSecretFiles(d.secretFilesDir(), e.files)
}

// stageTmpDir is where the directories of the commands' KIPP_TMP are made,
// outside every workspace.
func (d *Dispatcher) stageTmpDir() string {
	return filepath.Join(d.opts.DataDir, "stage-tmp")
}

// commandFiles are what a command of the server's own is given that is
// removed when the command ends: the files of the build's secrets, and
// KIPP_TMP, a directory of its own. The func removes both and is safe to
// run when there are none.
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

// remoteSecretFiles are the file secrets a stage on a runner is sent.
func remoteSecretFiles(e *buildEnv) []runnerproto.SecretFile {
	var out []runnerproto.SecretFile
	for _, name := range slices.Sorted(maps.Keys(e.files)) {
		out = append(out, runnerproto.SecretFile{Name: name, Data: e.files[name]})
	}
	return out
}
