package runner

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
)

// WriteSecretFiles writes each of files, by the name of the secret, as a
// file only its owner reads, in a directory made under root for this
// command alone. It returns the variables that name them, NAME=path, and
// the func that removes the directory, which the caller runs however the
// command ends. root is outside every workspace.
func WriteSecretFiles(root string, files map[string][]byte) (env []string, remove func(), err error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, nil, err
	}
	dir, err := os.MkdirTemp(root, "s-")
	if err != nil {
		return nil, nil, err
	}
	remove = func() { _ = os.RemoveAll(dir) }
	for _, name := range slices.Sorted(maps.Keys(files)) {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, files[name], 0o600); err != nil {
			remove()
			return nil, nil, fmt.Errorf("could not write the file of the secret %s: %w", name, err)
		}
		env = append(env, name+"="+path)
	}
	return env, remove, nil
}
