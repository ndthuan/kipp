package jobstore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// CheckWritable is files mode's check at start: that the server can
// create, rename and remove a file in dir. When it cannot, the error says
// which directory, which user, and the ways out.
func CheckWritable(dir string) error {
	err := tryWrite(dir)
	if err == nil {
		return nil
	}
	return fmt.Errorf("--job-edits files cannot write job files in %s as uid %d: %w\n"+
		"Make the directory writable by that user, drop :ro from a container's mount of it, or start with --job-edits store",
		dir, os.Getuid(), err)
}

func tryWrite(dir string) error {
	f, err := os.CreateTemp(dir, ".kipp-check-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if err := f.Close(); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	moved := tmp + ".renamed"
	if err := os.Rename(tmp, moved); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	return os.Remove(moved)
}

// writeFile writes the job file of name in dir atomically: a temporary
// file in the same directory, synced, then renamed over the old one, whose
// mode it keeps.
func writeFile(dir, name string, text []byte) error {
	path := filepath.Join(dir, name)
	mode := fs.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := atomicWrite(dir, path, text, mode); err != nil {
		return &WriteError{Path: path, Err: err}
	}
	return nil
}

func atomicWrite(dir, path string, text []byte, mode fs.FileMode) error {
	f, err := os.CreateTemp(dir, ".kipp-job-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(text)
	if err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	syncDir(dir)
	return nil
}

// syncDir makes a rename or a removal in dir last a crash, where the
// system can; a directory it cannot sync loses nothing but that.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// removeFile removes the job file of name in dir.
func removeFile(dir, name string) error {
	path := filepath.Join(dir, name)
	if err := os.Remove(path); err != nil {
		return &WriteError{Path: path, Err: err}
	}
	syncDir(dir)
	return nil
}
