package cache

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// craft is a zstd tar made by hand, for members Pack would never write.
func craft(t *testing.T, members ...tar.Header) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	zw, err := zstd.NewWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	for _, h := range members {
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		h.Size = int64(len("data"))
		if h.Typeflag != tar.TypeReg {
			h.Size = 0
		}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			_, _ = tw.Write([]byte("data"))
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return &out
}

func TestPackedFilesComeBackWithTheirModesAndLinks(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(src, "mods/a/one.txt"), "one")
	writeFile(t, filepath.Join(src, "mods/two.sh"), "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(src, "mods/two.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("two.sh", filepath.Join(src, "mods/link")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Pack(&buf, []string{"mods"}, "", src); err != nil {
		t.Fatal(err)
	}
	if err := Unpack(&buf, []string{"mods"}, "", dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "mods/a/one.txt")); string(b) != "one" {
		t.Fatalf("one.txt: %q", b)
	}
	if fi, _ := os.Stat(filepath.Join(dst, "mods/two.sh")); fi == nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("two.sh mode: %v", fi)
	}
	if target, err := os.Readlink(filepath.Join(dst, "mods/link")); err != nil || target != "two.sh" {
		t.Fatalf("link: %q %v", target, err)
	}
}

func TestTildePathsAreTheStageUsersHome(t *testing.T) {
	home, home2, dir := t.TempDir(), t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(home, ".cache/tool/x"), "x")
	var buf bytes.Buffer
	if err := Pack(&buf, []string{"~/.cache/tool"}, home, dir); err != nil {
		t.Fatal(err)
	}
	if err := Unpack(&buf, []string{"~/.cache/tool"}, home2, dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(home2, ".cache/tool/x")); string(b) != "x" {
		t.Fatalf("restored into the other home: %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, ".cache")); err == nil {
		t.Fatal("a ~/ path was restored under the stage's directory")
	}
}

func TestAbsolutePathsRoundTrip(t *testing.T) {
	root, dir := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(root, "store/y"), "y")
	var buf bytes.Buffer
	if err := Pack(&buf, []string{filepath.Join(root, "store")}, "", dir); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "store")); err != nil {
		t.Fatal(err)
	}
	if err := Unpack(&buf, []string{filepath.Join(root, "store")}, "", dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "store/y")); string(b) != "y" {
		t.Fatalf("restored: %q", b)
	}
}

func TestAMemberOutsideThePathsIsRefused(t *testing.T) {
	cases := map[string][]tar.Header{
		"a name that climbs":      {{Name: "mods/../../evil"}},
		"a sibling of the path":   {{Name: "other/file"}},
		"an absolute name":        {{Name: "/etc/evil"}},
		"a path's name as prefix": {{Name: "modsX/file"}},
		"a dot dot at the start":  {{Name: "../evil"}},
		"a hard link":             {{Name: "mods/h", Typeflag: tar.TypeLink, Linkname: "/etc/passwd"}},
		"a device":                {{Name: "mods/d", Typeflag: tar.TypeChar}},
		"a file below a link":     {{Name: "mods/l", Typeflag: tar.TypeSymlink, Linkname: "../../outside"}, {Name: "mods/l/pwn"}},
	}
	for name, members := range cases {
		dir := t.TempDir()
		err := Unpack(craft(t, members...), []string{"mods"}, "", dir)
		if err == nil {
			t.Errorf("%s: unpacked", name)
		}
		if _, statErr := os.Lstat(filepath.Join(dir, "..", "evil")); statErr == nil {
			t.Errorf("%s: wrote outside the directory", name)
		}
		if _, statErr := os.Lstat(filepath.Join(dir, "mods/l/pwn")); statErr == nil {
			t.Errorf("%s: wrote through a link", name)
		}
	}
}

func TestARestoreOverwritesAFileButNeverFollowsALinkThere(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	victim := filepath.Join(outside, "victim")
	writeFile(t, victim, "keep")
	if err := os.MkdirAll(filepath.Join(dir, "mods"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, "mods/f")); err != nil {
		t.Fatal(err)
	}
	if err := Unpack(craft(t, tar.Header{Name: "mods/f"}), []string{"mods"}, "", dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatalf("a restore wrote through a link: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "mods/f")); string(b) != "data" {
		t.Fatalf("the file was not restored: %q", b)
	}
}

func TestPackNamesAPathThatIsNotThere(t *testing.T) {
	err := Pack(&bytes.Buffer{}, []string{"nope"}, "", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("error %v, want one that names the path", err)
	}
}

func TestAPathThatClimbsOutOfTheDirectoryIsRefused(t *testing.T) {
	if err := Pack(&bytes.Buffer{}, []string{"../x"}, "", t.TempDir()); err == nil {
		t.Fatal("packed a path that climbs out of the stage's directory")
	}
	if err := Unpack(craft(t), []string{"../x"}, "", t.TempDir()); err == nil {
		t.Fatal("unpacked into a path that climbs out of the stage's directory")
	}
}

func TestALinkThatPointsOutOfThePathsIsRefusedAndNotWritten(t *testing.T) {
	for _, target := range []string{"/etc", "../../x", "a/../../x", "..", "/"} {
		dir := t.TempDir()
		err := Unpack(craft(t, tar.Header{Name: "mods/l", Typeflag: tar.TypeSymlink, Linkname: target}),
			[]string{"mods"}, "", dir)
		if err == nil {
			t.Errorf("link to %q: unpacked", target)
		}
		if _, statErr := os.Lstat(filepath.Join(dir, "mods/l")); statErr == nil {
			t.Errorf("link to %q: written", target)
		}
	}
}

func TestALinkToASiblingInsideThePathsIsKept(t *testing.T) {
	dir := t.TempDir()
	members := []tar.Header{
		{Name: "mods/sub/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "mods/sib", Typeflag: tar.TypeSymlink, Linkname: "sub"},
		{Name: "mods/sub/up", Typeflag: tar.TypeSymlink, Linkname: "../sib"},
		{Name: "mods/self", Typeflag: tar.TypeSymlink, Linkname: "."},
	}
	if err := Unpack(craft(t, members...), []string{"mods"}, "", dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.Readlink(filepath.Join(dir, "mods/sub/up")); got != "../sib" {
		t.Fatalf("link kept as %q", got)
	}
}

func TestAChainOfLinksCannotLeadOutOfThePaths(t *testing.T) {
	dir := t.TempDir()
	members := []tar.Header{
		{Name: "mods/a", Typeflag: tar.TypeSymlink, Linkname: "b"},
		{Name: "mods/b", Typeflag: tar.TypeSymlink, Linkname: "../../out"},
	}
	if err := Unpack(craft(t, members...), []string{"mods"}, "", dir); err == nil {
		t.Fatal("a chain of links led out of the paths")
	}
	if _, err := os.Lstat(filepath.Join(dir, "mods/b")); err == nil {
		t.Fatal("the link that leaves the paths was written")
	}
}

func TestMembersThatEscapeAreRefusedAndNothingIsWritten(t *testing.T) {
	for _, name := range []string{"../x", "/etc/x", "a/../../x", "mods/../../x"} {
		dir := t.TempDir()
		if err := Unpack(craft(t, tar.Header{Name: name}), []string{"mods"}, "", dir); err == nil {
			t.Errorf("%q: unpacked", name)
		}
		if _, err := os.Lstat(filepath.Join(dir, "..", "x")); err == nil {
			t.Errorf("%q: wrote outside the directory", name)
		}
	}
}

func TestAFileBelowALinkToTheOutsideIsRefused(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "mods"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "mods/lnk")); err != nil {
		t.Fatal(err)
	}
	if err := Unpack(craft(t, tar.Header{Name: "mods/lnk/f"}), []string{"mods"}, "", dir); err == nil {
		t.Fatal("unpacked a file below a link")
	}
	if _, err := os.Lstat(filepath.Join(outside, "f")); err == nil {
		t.Fatal("wrote through the link")
	}
}
