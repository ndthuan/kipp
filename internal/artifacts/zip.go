package artifacts

import (
	"archive/zip"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// Zip writes the files of the stage's directory root as a zip, one at a
// time as it goes, so that a stage's gigabytes are never held at once.
func Zip(w io.Writer, root string, files []File) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	zw := zip.NewWriter(w)
	for _, f := range files {
		if err := zipOne(zw, r, f); err != nil {
			return errors.Join(err, zw.Close())
		}
	}
	return zw.Close()
}

func zipOne(zw *zip.Writer, root *os.Root, f File) error {
	in, err := root.Open(filepath.FromSlash(f.Path))
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := zw.CreateHeader(&zip.FileHeader{Name: f.Path, Method: zip.Deflate})
	if err != nil {
		return err
	}
	_, err = io.Copy(out, io.LimitReader(in, f.Size))
	return err
}
