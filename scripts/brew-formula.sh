#!/bin/sh
# brew-formula.sh VERSION SUMS: the Homebrew formula of the release VERSION
# (v1.2.3), on stdout, its archives' checksums read from SUMS (the
# release's SHA256SUMS). A release installs the archive of the machine's
# platform; --HEAD builds main from source with Go.
# brew-formula.sh --head: the formula with no release in it, which only
# --HEAD installs: the tap's until the first release.
set -eu
export LC_ALL=C

usage="usage: brew-formula.sh VERSION SUMS | --head"
version=${1:?$usage}

sum() {
	file="kipp_${v}_$1.tar.gz"
	s=$(awk -v f="$file" '$2 == f || $2 == "*" f { print $1 }' "$sums")
	case $s in
	'' | *[!0-9a-f]*)
		echo "brew-formula.sh: no checksum of $file in $sums" >&2
		exit 1
		;;
	esac
	printf '%s' "$s"
}

stable() {
	darwin_arm64=$(sum darwin_arm64)
	darwin_amd64=$(sum darwin_amd64)
	linux_arm64=$(sum linux_arm64)
	linux_amd64=$(sum linux_amd64)
	cat <<STABLE
  version "$v"

  on_macos do
    on_arm do
      url "$base/kipp_${v}_darwin_arm64.tar.gz"
      sha256 "$darwin_arm64"
    end
    on_intel do
      url "$base/kipp_${v}_darwin_amd64.tar.gz"
      sha256 "$darwin_amd64"
    end
  end

  on_linux do
    on_arm do
      url "$base/kipp_${v}_linux_arm64.tar.gz"
      sha256 "$linux_arm64"
    end
    on_intel do
      url "$base/kipp_${v}_linux_amd64.tar.gz"
      sha256 "$linux_amd64"
    end
  end
STABLE
}

if [ "$version" = --head ]; then
	release=''
else
	sums=${2:?$usage}
	"$(dirname "$0")/check-version.sh" --release "$version"
	v=${version#v}
	base="https://github.com/ndthuan/kipp/releases/download/$version"
	release=$(stable)
fi

cat <<FORMULA
# This file is written by the release workflow of github.com/ndthuan/kipp
# (scripts/brew-formula.sh); a change made here is lost at the next release.
class Kipp < Formula
  desc "Job-first CI and scheduler in a single binary"
  homepage "https://github.com/ndthuan/kipp"
  license "MIT"
${release:+$release
}
  head do
    url "https://github.com/ndthuan/kipp.git", branch: "main"
    depends_on "go" => :build
  end

  def install
    if build.head?
      ldflags = "-s -w -buildid= -X github.com/ndthuan/kipp/internal/version.Version=#{version}"
      %w[amd64 arm64].each do |arch|
        with_env(CGO_ENABLED: "0", GOOS: "linux", GOARCH: arch) do
          system "go", "build", "-trimpath", "-buildvcs=false", "-ldflags", ldflags,
                 "-o", "internal/agentbin/bin/kipp-agent_linux_#{arch}", "./cmd/kipp-agent"
        end
      end
      with_env(CGO_ENABLED: "0") do
        system "go", "build", "-trimpath", "-buildvcs=false", "-ldflags", ldflags, "-o", bin/"kipp", "./cmd/kipp"
      end
    else
      bin.install "kipp"
    end
  end

  def post_install
    (var/"kipp").mkpath
    (etc/"kipp").mkpath
  end

  service do
    run [opt_bin/"kipp", "serve", "--data", var/"kipp", "--jobs", etc/"kipp", "--listen", "127.0.0.1:8085"]
    keep_alive true
    working_dir var/"kipp"
    log_path var/"log/kipp.log"
    error_log_path var/"log/kipp.log"
  end

  def caveats
    <<~EOS
      Job files go in #{etc}/kipp, the data directory is #{var}/kipp.
      Make the first admin before the first login:
        kipp user add admin --admin --data #{var}/kipp
    EOS
  end

  test do
    (testpath/"jobs/hello.yml").write "name: hello\\nstages:\\n  greet: {run: echo hello}\\n"
    system bin/"kipp", "check", testpath/"jobs"
    assert_match "kipp", shell_output("#{bin}/kipp version")
  end
end
FORMULA
