package runner

import "os"

// StageTemp makes a directory under root for one command alone, empty,
// only its owner's, and returns it with the func that removes it however
// the command ends. root is outside every workspace.
func StageTemp(root string) (dir string, remove func(), err error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", nil, err
	}
	dir, err = os.MkdirTemp(root, "t-")
	if err != nil {
		return "", nil, err
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}
