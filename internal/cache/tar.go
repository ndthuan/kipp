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

type target struct{ name, abs string }

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
	base := filepath.Dir(t.abs)
	root, err := os.OpenRoot(base)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
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
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return err
		}
		return copyFile(tw, root, rel)
	})
}

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

func copyFile(w io.Writer, root *os.Root, name string) error {
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(w, f)
	return err
}

// Unpack writes the files of a Pack's archive back, over what is there. A
// member that is not under one of the paths named, that climbs with "..",
// that lies below a link, that is a directory where a link is, that is a
// link whose target climbs with ".." after a name (the name may be, or
// become, a link itself), or that is neither a file, a directory nor a
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
	rel, root, err := locate(h.Name, ts)
	if err != nil {
		return err
	}
	if !filepath.IsLocal(rel) {
		return errOutside
	}
	dest := filepath.Join(root, rel)
	if err := belowNoLink(root, dest); err != nil {
		return err
	}
	perm := fs.FileMode(uint32(h.Mode & 0o777))
	switch h.Typeflag {
	case tar.TypeDir:
		return makeDirMember(root, dest, perm)
	case tar.TypeReg:
		return writeFileMember(root, dest, perm, r)
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

func makeDirMember(root, dest string, perm fs.FileMode) error {
	if dest != root {
		if fi, err := os.Lstat(dest); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
			return errors.New("is a directory where a symbolic link is")
		}
	}
	if err := os.MkdirAll(dest, 0o750); err != nil {
		return err
	}
	return os.Chmod(dest, perm|0o700)
}

func climbsAfterAName(linkname string) bool {
	named := false
	for el := range strings.SplitSeq(filepath.ToSlash(linkname), "/") {
		switch el {
		case "", ".":
		case "..":
			if named {
				return true
			}
		default:
			named = true
		}
	}
	return false
}

func linkStaysInside(root, dest, linkname string) error {
	if filepath.IsAbs(linkname) || strings.HasPrefix(linkname, "/") || climbsAfterAName(linkname) {
		return errLinkOutside
	}
	target := filepath.Join(filepath.Dir(dest), filepath.FromSlash(linkname))
	rel, err := filepath.Rel(root, target)
	if err != nil || escapes(rel) {
		return errLinkOutside
	}
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

func locate(name string, ts []target) (rel, root string, err error) {
	clean := path.Clean(name)
	if slices.Contains(strings.Split(clean, "/"), "..") {
		return "", "", errOutside
	}
	for _, t := range ts {
		if rel, ok := under(t.name, clean); ok {
			return filepath.FromSlash(rel), t.abs, nil
		}
	}
	return "", "", errOutside
}

var errOutside = errors.New("is outside the paths asked for")

func under(name, clean string) (string, bool) {
	switch {
	case clean == name:
		return ".", true
	case name == ".":
		if path.IsAbs(clean) || clean == "~" || strings.HasPrefix(clean, "~/") {
			return "", false
		}
		return clean, true
	}
	return strings.CutPrefix(clean, strings.TrimSuffix(name, "/")+"/")
}

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

func removeNonDirIn(root *os.Root, name string) error {
	fi, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return fmt.Errorf("%s is a directory", name)
	}
	return root.Remove(name)
}

func writeFileMember(rootDir, dest string, perm fs.FileMode, r io.Reader) error {
	rel, err := filepath.Rel(rootDir, dest)
	if err != nil {
		return err
	}
	if rel == "." {
		rootDir, rel = filepath.Dir(dest), filepath.Base(dest)
	}
	if err := os.MkdirAll(rootDir, 0o750); err != nil {
		return err
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.MkdirAll(filepath.Dir(rel), 0o750); err != nil {
		return err
	}
	if err := removeNonDirIn(root, rel); err != nil {
		return err
	}
	f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
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
	return os.Chmod(dest, perm)
}
