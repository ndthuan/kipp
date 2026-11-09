package cache

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

	"github.com/klauspost/compress/zstd"
)

// target is a path a stage named: name is how its members are called in
// the archive, abs where it is on this machine.
type target struct{ name, abs string }

// resolve places a path a stage named. It is relative to dir, absolute, or
// starts with ~/ for home. A relative path that climbs out of dir is
// refused: a cache holds what the stage's directory holds, and what lies
// elsewhere is named absolutely or from home.
func resolve(p, home, dir string) (target, error) {
	switch {
	case p == "":
		return target{}, errors.New("an empty path")
	case p == "~" || strings.HasPrefix(p, "~/"):
		if home == "" {
			return target{}, fmt.Errorf("%s: the home directory is not known", p)
		}
		return target{name: path.Clean(p), abs: filepath.Join(home, filepath.FromSlash(strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/")))}, nil
	case filepath.IsAbs(p):
		return target{name: path.Clean(filepath.ToSlash(p)), abs: filepath.Clean(p)}, nil
	}
	name := path.Clean(filepath.ToSlash(p))
	if name == ".." || strings.HasPrefix(name, "../") {
		return target{}, fmt.Errorf("%s: leads out of the stage's directory", p)
	}
	return target{name: name, abs: filepath.Join(dir, filepath.FromSlash(name))}, nil
}

func resolveAll(paths []string, home, dir string) ([]target, error) {
	if len(paths) == 0 {
		return nil, errors.New("no path to cache")
	}
	ts := make([]target, len(paths))
	for i, p := range paths {
		t, err := resolve(p, home, dir)
		if err != nil {
			return nil, err
		}
		ts[i] = t
	}
	return ts, nil
}

// Pack writes the files and directories the paths name as a zstd-compressed
// tar, with their modes and the links among them as links. Every path must
// exist.
func Pack(w io.Writer, paths []string, home, dir string) error {
	ts, err := resolveAll(paths, home, dir)
	if err != nil {
		return err
	}
	zw, err := zstd.NewWriter(w)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(zw)
	for _, t := range ts {
		if _, err := os.Lstat(t.abs); err != nil {
			return fmt.Errorf("%s: %w", t.name, errors.Unwrap(err))
		}
		if err := packTree(tw, t); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return zw.Close()
}

func packTree(tw *tar.Writer, t target) error {
	return filepath.WalkDir(t.abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		h, err := header(t, p, d)
		if err != nil || h == nil {
			return err
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg {
			return nil
		}
		return copyFile(tw, p)
	})
}

// header is the tar header of the file at p, named below t; nil for what a
// cache does not keep: a socket, a pipe or a device.
func header(t target, p string, d fs.DirEntry) (*tar.Header, error) {
	info, err := d.Info()
	if err != nil {
		return nil, err
	}
	mode := info.Mode()
	if !mode.IsRegular() && !mode.IsDir() && mode&fs.ModeSymlink == 0 {
		return nil, nil
	}
	link := ""
	if mode&fs.ModeSymlink != 0 {
		if link, err = os.Readlink(p); err != nil {
			return nil, err
		}
	}
	h, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(t.abs, p)
	if err != nil {
		return nil, err
	}
	h.Name = path.Join(t.name, filepath.ToSlash(rel))
	if mode.IsDir() {
		h.Name += "/"
	}
	h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
	return h, nil
}

func copyFile(w io.Writer, name string) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(w, f)
	return err
}

// Unpack writes the files of a Pack's archive back, over what is there. A
// member that is not under one of the paths named, that climbs with "..",
// that lies below a link, or that is neither a file, a directory nor a
// symbolic link, is an error and is not written: an entry is data a build
// saved, and a stage that restores it must not be led out of the paths it
// asked for.
func Unpack(r io.Reader, paths []string, home, dir string) error {
	ts, err := resolveAll(paths, home, dir)
	if err != nil {
		return err
	}
	zr, err := zstd.NewReader(r)
	if err != nil {
		return err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := unpackMember(tr, h, ts); err != nil {
			return fmt.Errorf("%s: %w", h.Name, err)
		}
	}
}

func unpackMember(r io.Reader, h *tar.Header, ts []target) error {
	dest, root, err := locate(h.Name, ts)
	if err != nil {
		return err
	}
	if err := belowNoLink(root, dest); err != nil {
		return err
	}
	perm := fs.FileMode(uint32(h.Mode & 0o777))
	switch h.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(dest, 0o750); err != nil {
			return err
		}
		// Owner-writable, so that the files below it can be written next.
		return os.Chmod(dest, perm|0o700)
	case tar.TypeReg:
		return writeFileMember(dest, perm, r)
	case tar.TypeSymlink:
		if err := linkStaysInside(root, dest, h.Linkname); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
			return err
		}
		if err := removeNonDir(dest); err != nil {
			return err
		}
		return os.Symlink(h.Linkname, dest)
	}
	return fmt.Errorf("a member of type %q is not kept", h.Typeflag)
}

// linkStaysInside refuses a link whose target is absolute or, read lexically
// from the link's own directory, leaves root: a restored link that points out
// of the cached paths would let a later step, or a save of them, follow it.
// A relative link to somewhere inside root is kept; a chain of such links
// cannot leave either, as each one is held to root.
func linkStaysInside(root, dest, linkname string) error {
	if filepath.IsAbs(linkname) || strings.HasPrefix(linkname, "/") {
		return errLinkOutside
	}
	target := filepath.Join(filepath.Dir(dest), filepath.FromSlash(linkname))
	rel, err := filepath.Rel(root, target)
	if err != nil || escapes(rel) {
		return errLinkOutside
	}
	// Then again with the links already on disk followed, so a target that
	// runs through one cannot lead out either.
	realRoot, err := resolveExisting(root)
	if err != nil {
		return err
	}
	realTarget, err := resolveExisting(target)
	if err != nil {
		return err
	}
	if rel, err = filepath.Rel(realRoot, realTarget); err != nil || escapes(rel) {
		return errLinkOutside
	}
	return nil
}

func escapes(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveExisting follows every link in p up to the deepest part that exists
// and appends the rest as written: a link may point at what is not yet there.
func resolveExisting(p string) (string, error) {
	rest := ""
	for {
		r, err := filepath.EvalSymlinks(p)
		if err == nil {
			return filepath.Join(r, rest), nil
		}
		parent := filepath.Dir(p)
		if !errors.Is(err, fs.ErrNotExist) || parent == p {
			return "", err
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

var errLinkOutside = errors.New("is a link that points outside the paths asked for")

// locate is where a member goes: under the path that names it, and that
// path's directory, which the member must not leave.
func locate(name string, ts []target) (dest, root string, err error) {
	clean := path.Clean(name)
	if slices.Contains(strings.Split(clean, "/"), "..") {
		return "", "", errOutside
	}
	for _, t := range ts {
		if rel, ok := under(t.name, clean); ok {
			return filepath.Join(t.abs, filepath.FromSlash(rel)), t.abs, nil
		}
	}
	return "", "", errOutside
}

var errOutside = errors.New("is outside the paths asked for")

// under is where the member clean lies below the path called name, in the
// archive's names: itself, a path inside it, or no path of it.
func under(name, clean string) (string, bool) {
	switch {
	case clean == name:
		return ".", true
	case name == ".":
		// The stage's directory holds what is neither absolute nor from home.
		if path.IsAbs(clean) || clean == "~" || strings.HasPrefix(clean, "~/") {
			return "", false
		}
		return clean, true
	}
	return strings.CutPrefix(clean, strings.TrimSuffix(name, "/")+"/")
}

// belowNoLink refuses a destination reached through a symbolic link below
// root: a link the archive itself restored could lead a later member out.
// The path named, root itself, may be a link, as ~/.cache is on some hosts.
func belowNoLink(root, dest string) error {
	rel, err := filepath.Rel(root, dest)
	if err != nil || rel == "." {
		return err
	}
	cur := root
	parts := strings.Split(rel, string(filepath.Separator))
	for _, el := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, el)
		fi, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return errors.New("lies below a symbolic link")
		}
	}
	return nil
}

// removeNonDir clears the way for a file or link, which replaces whatever
// is there: a link is replaced, never followed.
func removeNonDir(p string) error {
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return fmt.Errorf("%s is a directory", p)
	}
	return os.Remove(p)
}

func writeFileMember(dest string, perm fs.FileMode, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return err
	}
	if err := removeNonDir(dest); err != nil {
		return err
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// The mode as the archive says, not as the umask left it.
	return os.Chmod(dest, perm)
}
