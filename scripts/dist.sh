#!/bin/sh
# dist.sh VERSION: one kipp_<version>_<os>_<arch>.tar.gz per platform in
# dist/, and dist/SHA256SUMS. <version> is VERSION without a leading v.
#
# Two runs from the same commit with the same Go and the same tar give the
# same bytes, so anyone can rebuild an archive and compare it with a
# release's: the binary has no build id or paths of this machine in it,
# and the archive has its entries in a fixed order, the commit's time on
# every file, no owner names and no gzip timestamp.
set -eu
# Globs expand in byte order whatever the caller's language.
export LC_ALL=C

version=${1:?usage: dist.sh VERSION}
# Before anything is built or removed.
"$(dirname "$0")/check-version.sh" "$version"
name_version=${version#v}
go=${GO:-go}

epoch=$(git log -1 --format=%ct)
# touch -t reads local time; UTC on both sides keeps it the commit's.
stamp=$(TZ=UTC date -u -r "$epoch" +%Y%m%d%H%M.%S 2>/dev/null || TZ=UTC date -u -d "@$epoch" +%Y%m%d%H%M.%S)

# archive DIR: dist/DIR.tar.gz of dist/DIR and the files in it, in byte
# order. GNU tar and the BSD tar of macOS name the same options
# differently.
archive() {
	dir=$1
	if tar --version 2>/dev/null | grep -q 'GNU tar'; then
		set -- --owner=0 --group=0 --numeric-owner
	else
		set -- --uid 0 --gid 0 --uname root --gname root --no-xattrs --no-mac-metadata
	fi
	(cd dist && COPYFILE_DISABLE=1 tar "$@" --no-recursion -cf - "$dir" "$dir"/* "$dir"/docs/* | gzip -9n >"$dir.tar.gz")
}

ldflags="-s -w -buildid= -X github.com/ndthuan/kipp/internal/version.Version=$version"

rm -rf dist
mkdir -p dist
# The Linux agents first: every kipp embeds them, and puts the one of an
# engine's architecture into the containers it starts there. They are the
# release's version, as the server must be.
for arch in amd64 arm64; do
	CGO_ENABLED=0 GOOS=linux GOARCH=$arch "$go" build -trimpath -buildvcs=false -ldflags "$ldflags" \
		-o "internal/agentbin/bin/kipp-agent_linux_$arch" ./cmd/kipp-agent
done
for platform in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
	os=${platform%/*}
	arch=${platform#*/}
	dir=kipp_${name_version}_${os}_${arch}
	mkdir -p "dist/$dir"
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch "$go" build -trimpath -buildvcs=false -ldflags "$ldflags" \
		-o "dist/$dir/kipp" ./cmd/kipp
	cp LICENSE README.md THIRD_PARTY.md CHANGELOG.md "dist/$dir/"
	# The README's links lead into docs/; the pictures stay on GitHub.
	mkdir "dist/$dir/docs"
	cp docs/*.md "dist/$dir/docs/"
	# The files inside docs/ first: 644 on the directory itself takes away
	# the search bit chmod needs to reach them.
	chmod 644 "dist/$dir/docs"/*
	chmod 644 "dist/$dir"/*
	chmod 755 "dist/$dir" "dist/$dir/kipp" "dist/$dir/docs"
	TZ=UTC touch -t "$stamp" "dist/$dir" "dist/$dir"/* "dist/$dir/docs"/*
	archive "$dir"
	rm -rf "dist/$dir"
done

cd dist
if command -v sha256sum >/dev/null 2>&1; then
	sha256sum kipp_*.tar.gz >SHA256SUMS
else
	shasum -a 256 kipp_*.tar.gz >SHA256SUMS
fi
