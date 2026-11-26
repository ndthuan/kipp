package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
)

// The files of a release that an upgrade reads.
const (
	// SumsFile lists the checksum of every archive, and SigFile signs it.
	SumsFile = "SHA256SUMS"
	// MaxArchive is the most an archive may be, packed or unpacked.
	MaxArchive = 200 << 20
	// MaxSmall is the most SHA256SUMS or its signature may be.
	MaxSmall = 1 << 20
)

// ArchiveName is the release archive of a platform: the file to download,
// and (without the suffix) the directory its binary is in.
func ArchiveName(tag, goos, goarch string) string {
	return ArchiveDir(tag, goos, goarch) + ".tar.gz"
}

// ArchiveDir is the directory a release archive holds its files in.
func ArchiveDir(tag, goos, goarch string) string {
	return "kipp_" + strings.TrimPrefix(tag, "v") + "_" + goos + "_" + goarch
}

// Fetch downloads one file of the release tagged tag, at most max bytes.
// The address is built here from the repository and the tag; nothing a
// lookup answered is used in it. A redirect to a host that releases are not
// served from is refused.
func (c *Checker) Fetch(ctx context.Context, tag, file string, max int64) ([]byte, error) {
	if !Release(tag) || file == "" || file != path.Base(file) || strings.ContainsAny(file, `\?#%`) || file == ".." {
		return nil, fmt.Errorf("%q of %q is not a release file", file, tag)
	}
	u, err := c.allowed(c.o.DownloadBase + repoPath + "/releases/download/" + tag + "/" + file)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "kipp/"+c.o.Current)
	resp, err := c.dl.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s answered %d", file, resp.StatusCode)
	}
	if resp.ContentLength > max {
		return nil, fmt.Errorf("%s is %d bytes, over the %d allowed", file, resp.ContentLength, max)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", file, err)
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is over the %d bytes allowed", file, max)
	}
	return b, nil
}

// ExtractBinary reads a release archive and returns the file kipp in the
// directory dir, at most max bytes unpacked.
func ExtractBinary(archive []byte, dir string, max int64) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("the archive: %w", err)
	}
	defer func() { _ = zr.Close() }()
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("the archive has no %s/kipp", dir)
		}
		if err != nil {
			return nil, fmt.Errorf("the archive: %w", err)
		}
		if h.Typeflag != tar.TypeReg || path.Clean(h.Name) != dir+"/kipp" {
			continue
		}
		if h.Size > max {
			return nil, fmt.Errorf("the binary is %d bytes, over the %d allowed", h.Size, max)
		}
		b, err := io.ReadAll(io.LimitReader(tr, max+1))
		if err != nil {
			return nil, fmt.Errorf("the archive: %w", err)
		}
		if int64(len(b)) > max {
			return nil, fmt.Errorf("the binary is over the %d bytes allowed", max)
		}
		return b, nil
	}
}
