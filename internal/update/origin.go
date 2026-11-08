package update

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ndthuan/kipp/internal/version"
)

// The install types InstallType answers.
const (
	Homebrew = "homebrew"
	Docker   = "docker"
	Archive  = "archive"
	Dev      = "dev"
)

// InstallType is how the running kipp was installed: the image sets
// KIPP_INSTALL_SOURCE=docker, an executable under a Cellar directory is
// Homebrew's, a version that is not a release's is a dev build, and what
// is left is a release archive. A process the launcher started from the
// data directory names the executable it replaced in KIPP_BUNDLED_EXE, and
// that one is judged by its path.
func InstallType() string {
	return installType(version.Version, os.Getenv("KIPP_INSTALL_SOURCE"), executable())
}

// Origin is InstallType for people to read. It goes by the marker and the
// executable's path alone, so that a dev build still says where it is from.
func Origin() string {
	return originName(originType(os.Getenv("KIPP_INSTALL_SOURCE"), executable()))
}

func originName(t string) string {
	switch t {
	case Docker:
		return "Docker image"
	case Homebrew:
		return "Homebrew"
	}
	return "release archive or build"
}

func installType(ver, marker, exe string) string {
	if !Release(ver) {
		return Dev
	}
	return originType(marker, exe)
}

func originType(marker, exe string) string {
	switch {
	case marker == "docker":
		return Docker
	case slices.Contains(strings.Split(filepath.ToSlash(exe), "/"), "Cellar"):
		return Homebrew
	}
	return Archive
}

// executable is the real path of the executable the process began as, or
// of the one the launcher replaced.
func executable() string {
	exe := os.Getenv("KIPP_BUNDLED_EXE")
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return ""
		}
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return exe
}
