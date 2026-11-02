package artifacts

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Copy copies the files from src into dst, both stage directories, dst
// emptied first: a database restored from a backup gives out a build
// number again, and what the earlier build of it kept is not this one's.
func Copy(src, dst string, files []File) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if len(files) == 0 {
		// An empty directory says the stage kept nothing.
		return os.MkdirAll(dst, 0o750)
	}
	from, err := os.OpenRoot(src)
	if err != nil {
		return err
	}
	defer func() { _ = from.Close() }()
	if err := os.MkdirAll(dst, 0o750); err != nil {
		return err
	}
	to, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer func() { _ = to.Close() }()
	for _, f := range files {
		if err := copyOne(from, to, f); err != nil {
			return fmt.Errorf("artifacts: %s: %w", f.Path, err)
		}
	}
	return nil
}

func copyOne(from, to *os.Root, f File) error {
	in, err := from.Open(filepath.FromSlash(f.Path))
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	// What the file holds now, up to the size it was counted at: a stage
	// still writing to it cannot make it larger than its max_size let it.
	return write(to, f.Path, io.LimitReader(in, f.Size))
}

// write creates name inside root from r, its directories with it.
func write(root *os.Root, name string, r io.Reader) error {
	name = filepath.FromSlash(name)
	if err := root.MkdirAll(filepath.Dir(name), 0o750); err != nil {
		return err
	}
	out, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, r)
	return errors.Join(err, out.Close())
}

// Open opens one kept file of a stage's directory, nothing outside it.
func Open(dir, name string) (*os.File, error) {
	if !ValidPath(name) {
		return nil, fs.ErrNotExist
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return root.Open(filepath.FromSlash(name))
}

// TarFiles writes the files of dir as a tar, each no larger than it was
// counted at, names their paths.
func TarFiles(w io.Writer, dir string, files []File) error {
	tw := tar.NewWriter(w)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	for _, f := range files {
		if err := tarOne(tw, root, f.Path, f.Size); err != nil {
			return err
		}
	}
	return tw.Close()
}

// Tar writes every regular file under dir as a tar, names relative to
// it; a dir that is not there is an empty tar.
func Tar(w io.Writer, dir string) error {
	tw := tar.NewWriter(w)
	root, err := os.OpenRoot(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return tw.Close()
	case err != nil:
		return err
	}
	defer func() { _ = root.Close() }()
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return tarOne(tw, root, p, info.Size())
	})
	return errors.Join(err, tw.Close())
}

// tarOne writes name as a member of size bytes: a file that has grown
// since it was counted is cut there, and one that has shrunk is padded
// with zeros, so that the tar is always what its headers say.
func tarOne(tw *tar.Writer, root *os.Root, name string, size int64) error {
	f, err := root.Open(filepath.FromSlash(name))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: size, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	n, err := io.Copy(tw, io.LimitReader(f, size))
	if err == nil && n < size {
		_, err = io.CopyN(tw, zeros{}, size-n)
	}
	return err
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// ForgedError is a tar member no stage's files could have made: a link,
// or a name that is absolute or climbs out with "..". A stage in a
// container runs as root beside its agent and can write the tar the
// agent sends, so the receiving end checks it.
type ForgedError struct {
	Name string
	Why  string
}

func (e *ForgedError) Error() string {
	return fmt.Sprintf("artifacts: refused tar member %q: %s", e.Name, e.Why)
}

// Untar writes the regular files of a tar under dir, which it makes, and
// is the files it wrote. A link, a name that is not a path inside dir,
// and more than max bytes in all are refused, and then dir is removed: a
// tar that is refused keeps nothing. A directory member is passed over.
// declared is the size the sender said the files come to, -1 for none:
// one over max is refused before anything is read.
func Untar(r io.Reader, dir string, max, declared int64) ([]File, error) {
	if declared > max {
		return nil, &TooLargeError{Size: declared, Max: max}
	}
	files, err := untar(r, dir, max)
	if err != nil {
		return nil, errors.Join(err, os.RemoveAll(dir))
	}
	return files, nil
}

func untar(r io.Reader, dir string, max int64) ([]File, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	tr := tar.NewReader(r)
	var total int64
	var files []File
	for {
		hd, err := tr.Next()
		switch {
		case errors.Is(err, io.EOF):
			slices.SortFunc(files, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
			return files, nil
		case err != nil:
			return nil, err
		}
		if err := member(hd); err != nil || hd.Typeflag == tar.TypeDir {
			if err != nil {
				return nil, err
			}
			continue
		}
		if total += hd.Size; total > max {
			return nil, &TooLargeError{Size: total, Max: max}
		}
		name := path.Clean(hd.Name)
		if err := write(root, name, io.LimitReader(tr, hd.Size)); err != nil {
			return nil, err
		}
		files = append(files, File{Path: name, Size: hd.Size})
	}
}

// member refuses a tar member that is not a plain file or directory
// inside the directory it is unpacked in.
func member(hd *tar.Header) error {
	switch {
	case hd.Typeflag != tar.TypeReg && hd.Typeflag != tar.TypeDir:
		return &ForgedError{Name: hd.Name, Why: "only files and directories are taken, not links"}
	case !filepath.IsLocal(filepath.FromSlash(hd.Name)) || path.IsAbs(hd.Name):
		return &ForgedError{Name: hd.Name, Why: "a name inside the stage's directory, not absolute and not climbing out with .."}
	case hd.Size < 0:
		return &ForgedError{Name: hd.Name, Why: "a size below zero"}
	}
	return nil
}
