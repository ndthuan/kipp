// Package artifacts finds the files a stage keeps, copies them where
// their build keeps them, and hands them out again: one by one, as a zip
// of a stage's, or as a tar to a stage on an agent. Every read and write
// goes through an os.Root, so that a link cannot lead one out of the
// directory it is about.
package artifacts

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
	"syscall"
)

// File is one kept file: its path inside the stage's directory, with
// slashes, its size and the SHA-256 of its contents.
type File struct {
	Path   string
	Size   int64
	SHA256 string
}

// TooLargeError is a stage whose matches come to more than its max_size.
type TooLargeError struct {
	Size, Max int64
}

func (e *TooLargeError) Error() string {
	return fmt.Sprintf("artifacts: %d bytes over the stage's max_size of %d", e.Size, e.Max)
}

// OutsideError is a match that leads out of the stage's directory.
type OutsideError struct {
	Path string
}

func (e *OutsideError) Error() string {
	return fmt.Sprintf("artifacts: %s leads outside the stage's directory", e.Path)
}

// Collect is the files the globs match in dir, with their sizes and
// sums, in path order. A match that is a directory stands for every file
// under it; a link is followed to a file inside dir, and one that leads
// out of it is an OutsideError. A link to a directory is not followed,
// so that a loop cannot hold the stage. Matches that come to more than
// max bytes are a TooLargeError, and nothing is read of them.
func Collect(dir string, globs []string, max int64) ([]File, error) {
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	files, err := match(root, globs)
	if err != nil {
		return nil, err
	}
	var total int64
	for _, f := range files {
		total += f.Size
	}
	if total > max {
		return nil, &TooLargeError{Size: total, Max: max}
	}
	for i := range files {
		if files[i].SHA256, err = sum(root, files[i].Path); err != nil {
			return nil, err
		}
	}
	return files, nil
}

// match is the regular files the globs stand for, each once, sized.
func match(root *os.Root, globs []string) ([]File, error) {
	found := map[string]int64{}
	for _, g := range globs {
		names, err := candidates(root, g)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			if err := add(root, name, found); err != nil {
				return nil, err
			}
		}
	}
	files := make([]File, 0, len(found))
	for _, p := range slices.Sorted(maps.Keys(found)) {
		files = append(files, File{Path: p, Size: found[p]})
	}
	return files, nil
}

// candidates is what one glob names: itself when it has no pattern in it
// and is there, else what fs.Glob finds.
func candidates(root *os.Root, glob string) ([]string, error) {
	if !strings.ContainsAny(glob, `*?[\`) {
		_, err := root.Lstat(glob)
		switch {
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
			return nil, nil
		case err != nil:
			// The path is there and leads through a link out of the root.
			return nil, &OutsideError{Path: glob}
		}
		return []string{glob}, nil
	}
	return fs.Glob(root.FS(), glob)
}

// add puts name, or every file under it, into found.
func add(root *os.Root, name string, found map[string]int64) error {
	info, err := resolve(root, name)
	switch {
	case err != nil:
		return err
	case info == nil:
		return nil
	case info.Mode().IsRegular():
		found[name] = info.Size()
		return nil
	}
	return fs.WalkDir(root.FS(), name, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == name || d.IsDir() {
			return err
		}
		info, err := resolve(root, p)
		if err == nil && info != nil && info.Mode().IsRegular() {
			found[p] = info.Size()
		}
		return err
	})
}

// resolve is what name is once a link is followed: a regular file or a
// directory, or nil for anything else (a socket, a link to a directory).
func resolve(root *os.Root, name string) (fs.FileInfo, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	link := info.Mode()&fs.ModeSymlink != 0
	if link {
		if info, err = root.Stat(name); errors.Is(err, fs.ErrNotExist) {
			return nil, nil // a link to nothing keeps nothing
		} else if err != nil {
			// What os.Root refuses is a link that leads out of it.
			return nil, &OutsideError{Path: name}
		}
	}
	switch {
	case info.Mode().IsRegular(), info.IsDir() && !link:
		return info, nil
	}
	return nil, nil
}

func sum(root *os.Root, name string) (string, error) {
	f, err := root.Open(name)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ValidPath reports whether p may be a kept file's path: inside a
// directory, with slashes, no part of it . or .., and not empty.
func ValidPath(p string) bool {
	return fs.ValidPath(p) && p != "." && path.Clean(p) == p
}
