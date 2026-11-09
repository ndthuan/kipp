// Package scripts holds the tests of the release scripts beside it.
package scripts

import (
	"crypto/sha256"
	"encoding/hex"
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

// Every kipp of a release carries the Linux runners of the same version,
// so dist.sh builds them before it builds a kipp, with the same flags.
func TestDistBuildsTheRunnersBeforeEveryKippAndWithItsVersion(t *testing.T) {
	data, err := os.ReadFile("dist.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	runners, kipp := strings.Index(script, "./cmd/kipp-runner"), strings.Index(script, "./cmd/kipp\n")
	if runners < 0 || kipp < 0 || runners > kipp {
		t.Fatal("dist.sh does not build the runners before kipp")
	}
	if strings.Count(script, `-ldflags "$ldflags"`) != 2 {
		t.Fatal("the runners and kipp are not built with the same flags")
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

// The dry run of the release's signing: a test keypair stands in for the
// secret and for the key in the repository, and the script is the one the
// workflow runs.
func signingDir(t *testing.T) (dir, pub, secret string) {
	t.Helper()
	root := t.TempDir()
	dir = filepath.Join(root, "dist")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "kipp_1.0.0_linux_amd64.tar.gz")
	if err := os.WriteFile(archive, []byte("an archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("an archive"))
	line := hex.EncodeToString(sum[:]) + "  kipp_1.0.0_linux_amd64.tar.gz\n"
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	pub, secret = filepath.Join(root, "test.pub"), filepath.Join(root, "test.key")
	keygen := exec.Command("go", "run", "../cmd/kipp-release", "keygen", "-secret", secret, "-pub", pub)
	if out, err := keygen.CombinedOutput(); err != nil {
		t.Fatalf("keygen: %v\n%s", err, out)
	}
	return dir, pub, secret
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func signRelease(t *testing.T, key string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{"./sign-release.sh"}, args...)...)
	cmd.Env = append(os.Environ(), "RELEASE_SIGNING_KEY="+key)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestTheReleaseIsSignedAndItsSignatureVerifiedBeforeAnythingIsPublished(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go")
	}
	dir, pub, secret := signingDir(t)
	key := string(mustRead(t, secret))
	if out, err := signRelease(t, key, "sign", dir, pub); err != nil {
		t.Fatalf("signing: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "SHA256SUMS.sig")); err != nil {
		t.Fatal("no signature written")
	}
	if out, err := signRelease(t, "", "verify", dir, pub); err != nil {
		t.Errorf("a good release refused: %v\n%s", err, out)
	}

	// The key in the repository is another one: the placeholder or a stale key.
	_, otherPub, _ := signingDir(t)
	if out, err := signRelease(t, key, "sign", dir, otherPub); err == nil {
		t.Errorf("a secret that does not match the public key was accepted:\n%s", out)
	}
	if out, err := signRelease(t, "", "verify", dir, otherPub); err == nil {
		t.Errorf("a signature by another key verified:\n%s", out)
	}

	// A changed archive list no longer matches its signature.
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := signRelease(t, "", "verify", dir, pub); err == nil {
		t.Errorf("a changed SHA256SUMS verified:\n%s", out)
	}
}

func TestTheReleaseIsNotSignedWithoutTheSecretAndNeverEchoesIt(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go")
	}
	dir, pub, _ := signingDir(t)
	if out, err := signRelease(t, "", "sign", dir, pub); err == nil {
		t.Errorf("signed without a key:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "SHA256SUMS.sig")); err == nil {
		t.Error("a signature written without a key")
	}
	const junk = "SECRET-NOT-A-KEY-1234"
	out, err := signRelease(t, junk, "sign", dir, pub)
	if err == nil {
		t.Error("a malformed key signed")
	}
	if strings.Contains(out, junk) {
		t.Errorf("the key was echoed:\n%s", out)
	}
}

// The workflow runs the placeholder check in its first job, signs before
// it uploads, and uploads the signature beside SHA256SUMS.
func TestTheReleaseWorkflowChecksTheKeySignsAndPublishesTheSignature(t *testing.T) {
	data, err := os.ReadFile("../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	w := string(data)
	check, tag, ciGreen := strings.Index(w, "check-release-key.sh"), strings.Index(w, "\n  tag:"), strings.Index(w, "\n  ci-green:")
	if check < tag || check > ciGreen {
		t.Error("check-release-key.sh is not in the first job")
	}
	sign, upload := strings.Index(w, "sign-release.sh sign"), strings.Index(w, "gh release upload")
	if sign < 0 || upload < 0 || sign > upload {
		t.Error("the release is not signed before its files are uploaded")
	}
	if !strings.Contains(w, "dist/SHA256SUMS dist/SHA256SUMS.sig") || !strings.Contains(w, "sign-release.sh verify") {
		t.Error("the signature is not uploaded and verified after publishing")
	}
	if strings.Count(w, "secrets.RELEASE_SIGNING_KEY") != 1 {
		t.Error("the signing key is not in exactly one step's environment")
	}
}

func TestTheUpgradeSmokeSaysNotVerifiedWithoutDocker(t *testing.T) {
	cmd := exec.Command("/bin/sh", "smoke-upgrade.sh")
	cmd.Env = []string{"PATH=" + t.TempDir()}
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "NOT VERIFIED") {
		t.Fatalf("%v: %s", err, out)
	}
}

// The test doors are compiled in by a build argument that only the smoke
// sets, and the image names its install source.
func TestTheImageTakesTestTagsOnlyByBuildArgumentAndNamesItsSource(t *testing.T) {
	b, err := os.ReadFile("../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, want := range []string{"\nARG TAGS=\n", "ENV KIPP_INSTALL_SOURCE=docker\n", `${TAGS:+-tags "$TAGS"}`} {
		if !strings.Contains(text, want) {
			t.Errorf("the Dockerfile lacks %q", want)
		}
	}
}

// ghStub puts a gh on PATH that prints the next line of answers on each
// call (the last one repeats), the way `gh api --jq` prints what the
// script asks for. An answer "fail" makes gh exit 1.
func ghStub(t *testing.T, answers ...string) []string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "answers"), []byte(strings.Join(answers, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := `#!/bin/sh
d=$(dirname "$0")
n=$(cat "$d/n" 2>/dev/null || echo 0)
n=$((n+1))
echo "$n" > "$d/n"
echo "$*" > "$d/args"
a=$(sed -n "${n}p" "$d/answers")
[ -n "$a" ] || a=$(tail -n 1 "$d/answers")
[ "$a" = fail ] && exit 1
echo "$a"
`
	gh := filepath.Join(dir, "gh")
	if err := os.WriteFile(gh, []byte(stub), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(gh, 0o700); err != nil { //nolint:gosec // a stub the test runs
		t.Fatal(err)
	}
	return []string{"PATH=" + dir + ":" + os.Getenv("PATH"), "STUB=" + dir}
}

func checkCI(t *testing.T, env []string, wait string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", "./check-ci.sh", "o/r", "abc123")
	cmd.Env = append([]string{"CI_WAIT_SECONDS=" + wait, "CI_POLL_SECONDS=1"}, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestAReleaseNeedsTheCIRunOfItsCommitToHavePassed(t *testing.T) {
	const url = "https://example.com/run/1"
	cases := []struct {
		name    string
		answers []string
		wait    string
		ok      bool
		want    string
	}{
		{"green", []string{"completed success " + url}, "5", true, "ci passed"},
		{"red", []string{"completed failure " + url}, "5", false, "did not pass"},
		{"cancelled", []string{"completed cancelled " + url}, "5", false, "did not pass"},
		{"missing", []string{"none"}, "5", false, "no run"},
		{"running then green", []string{"in_progress null " + url, "completed success " + url}, "5", true, "ci passed"},
		{"running then red", []string{"queued null " + url, "completed failure " + url}, "5", false, "did not pass"},
		{"running past the deadline", []string{"in_progress null " + url}, "2", false, "still running after 2s"},
		{"the API failing", []string{"fail"}, "5", false, "could not list"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := ghStub(t, c.answers...)
			out, err := checkCI(t, env, c.wait)
			if (err == nil) != c.ok || !strings.Contains(out, c.want) {
				t.Fatalf("ok=%v err=%v, want %q:\n%s", c.ok, err, c.want, out)
			}
		})
	}
}

func TestTheCIRunIsAskedForByCommitOnAPushToMain(t *testing.T) {
	env := ghStub(t, "completed success u")
	if out, err := checkCI(t, env, "5"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	args := string(mustRead(t, filepath.Join(strings.TrimPrefix(env[1], "STUB="), "args")))
	for _, want := range []string{"repos/o/r/actions/workflows/ci.yml/runs", "head_sha=abc123", "event=push", "branch=main"} {
		if !strings.Contains(args, want) {
			t.Errorf("gh was called without %q: %s", want, args)
		}
	}
}

func TestTheReleaseWorkflowChecksCIAndDoesNotRunTheGate(t *testing.T) {
	data, err := os.ReadFile("../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	w := string(data)
	if strings.Contains(w, "make quality") {
		t.Error("the release runs the gate again")
	}
	if !strings.Contains(w, "check-ci.sh") || !strings.Contains(w, "actions: read") {
		t.Error("the release does not check the ci run with actions: read")
	}
}
