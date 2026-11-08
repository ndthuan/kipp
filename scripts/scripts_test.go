// Package scripts holds the tests of the release scripts beside it.
package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func checkVersion(args ...string) error {
	return exec.Command("sh", append([]string{"./check-version.sh"}, args...)...).Run()
}

func TestABuildVersionIsAReleaseTagOrWhatGitDescribeSaysAndNothingElse(t *testing.T) {
	for _, v := range []string{"v0.1.0", "0.1.0", "v1.2.3-rc.1", "v0.1.0-3-gabc1234", "v0.1.0-3-gabc1234-dirty", "abc1234", "abc1234-dirty"} {
		if err := checkVersion(v); err != nil {
			t.Errorf("%q refused: %v", v, err)
		}
	}
	for _, v := range []string{"", "v1", "v1.2", "v1.2.3;touch x", "$(id)", "v1.2.3 ", "v1.2.3\nx", "abc12", "ABC1234", "-v1.2.3", "v1.2.3-", "a/b"} {
		if checkVersion(v) == nil {
			t.Errorf("%q taken", v)
		}
	}
}

func TestAReleaseTagIsVMajorMinorPatchWithAnOptionalPreRelease(t *testing.T) {
	for _, v := range []string{"v0.1.0", "v1.2.3-rc.1", "v10.20.30-beta"} {
		if err := checkVersion("--release", v); err != nil {
			t.Errorf("%q refused: %v", v, err)
		}
	}
	for _, v := range []string{"0.1.0", "v0.1.0-3-gabc1234", "abc1234", "v1.2.3-rc-1", "v1.2.3;x", "v1.2"} {
		if checkVersion("--release", v) == nil {
			t.Errorf("%q taken", v)
		}
	}
}

// A tag's name may hold what a shell runs (git allows ; and ` in one):
// make dist must hand it to dist.sh as a word, and dist.sh refuse it
// before it builds or removes anything.
func TestMakeDistRunsNothingAVersionHolds(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("no make")
	}
	pwned := filepath.Join(t.TempDir(), "pwned")
	cmd := exec.Command("make", "-s", "dist", "VERSION=v1.0.0;touch "+pwned, "GO=false")
	cmd.Dir = ".."
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("make dist took the version:\n%s", out)
	}
	if _, err := os.Stat(pwned); err == nil {
		t.Fatal("make dist ran what the version held")
	}
}

// Every kipp of a release carries the Linux agents of the same version,
// so dist.sh builds them before it builds a kipp, with the same flags.
func TestDistBuildsTheAgentsBeforeEveryKippAndWithItsVersion(t *testing.T) {
	data, err := os.ReadFile("dist.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	agents, kipp := strings.Index(script, "./cmd/kipp-agent"), strings.Index(script, "./cmd/kipp\n")
	if agents < 0 || kipp < 0 || agents > kipp {
		t.Fatal("dist.sh does not build the agents before kipp")
	}
	if strings.Count(script, `-ldflags "$ldflags"`) != 2 {
		t.Fatal("the agents and kipp are not built with the same flags")
	}
}

func brewFormula(t *testing.T, args ...string) (string, error) {
	t.Helper()
	out, err := exec.Command("sh", append([]string{"./brew-formula.sh"}, args...)...).Output()
	return string(out), err
}

func TestTheFormulaOfAReleaseInstallsEachPlatformsArchiveByItsChecksum(t *testing.T) {
	sums := filepath.Join(t.TempDir(), "SHA256SUMS")
	var lines []string
	for i, p := range []string{"darwin_amd64", "darwin_arm64", "linux_amd64", "linux_arm64"} {
		lines = append(lines, strings.Repeat(string(rune('a'+i)), 64)+"  kipp_1.2.3_"+p+".tar.gz")
	}
	if err := os.WriteFile(sums, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := brewFormula(t, "v1.2.3", sums)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range []string{"darwin_amd64", "darwin_arm64", "linux_amd64", "linux_arm64"} {
		url := `url "https://github.com/ndthuan/kipp/releases/download/v1.2.3/kipp_1.2.3_` + p + `.tar.gz"`
		sum := `sha256 "` + strings.Repeat(string(rune('a'+i)), 64) + `"`
		if !strings.Contains(f, url+"\n      "+sum) {
			t.Errorf("%s: want %s then %s in\n%s", p, url, sum, f)
		}
	}
	if !strings.Contains(f, `version "1.2.3"`) || !strings.Contains(f, "head do") {
		t.Errorf("no version or no head in\n%s", f)
	}

	if err := os.WriteFile(sums, []byte(strings.Join(lines[:3], "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := brewFormula(t, "v1.2.3", sums); err == nil {
		t.Error("a release missing one platform's checksum was written")
	}
	if _, err := brewFormula(t, "v1.2.3;x", sums); err == nil {
		t.Error("a tag that is not a release was taken")
	}
}

func TestTheHeadFormulaHasNoReleaseInIt(t *testing.T) {
	f, err := brewFormula(t, "--head")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f, "releases/download") || strings.Contains(f, "version \"") {
		t.Errorf("a release in the head formula:\n%s", f)
	}
	if !strings.Contains(f, `url "https://github.com/ndthuan/kipp.git", branch: "main"`) {
		t.Errorf("no head url in\n%s", f)
	}
}

func TestAReleaseIsNotBuiltWithThePlaceholderKey(t *testing.T) {
	dir := t.TempDir()
	placeholder := filepath.Join(dir, "placeholder.pub")
	real := filepath.Join(dir, "real.pub")
	if err := os.WriteFile(placeholder, []byte("# TEST-ONLY PLACEHOLDER\nuntrusted comment: minisign public key 0\nRWQ=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("untrusted comment: minisign public key 0\nRWQ=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", "./check-release-key.sh", placeholder).CombinedOutput(); err == nil {
		t.Errorf("the placeholder passed:\n%s", out)
	}
	if out, err := exec.Command("sh", "./check-release-key.sh", real).CombinedOutput(); err != nil {
		t.Errorf("a key without the marker refused: %v\n%s", err, out)
	}
	if err := exec.Command("sh", "./check-release-key.sh", filepath.Join(dir, "none")).Run(); err == nil {
		t.Error("a missing key file passed")
	}
}

// The repository carries the placeholder until the real key lands, so the
// script must refuse the file kipp is built with today.
func TestTheKeyInTheRepositoryIsCheckedByDefault(t *testing.T) {
	data, err := os.ReadFile("../internal/update/release.pub")
	if err != nil {
		t.Fatal(err)
	}
	placeholder := strings.HasPrefix(string(data), "# TEST-ONLY PLACEHOLDER")
	err = exec.Command("sh", "./check-release-key.sh").Run()
	if placeholder == (err == nil) {
		t.Errorf("placeholder=%v but the script said %v", placeholder, err)
	}
}
