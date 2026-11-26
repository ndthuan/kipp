#!/bin/sh
set -eu
export LC_ALL=C

version=${1:?usage: dist.sh VERSION}
"$(dirname "$0")/check-version.sh" "$version"
name_version=${version#v}
go=${GO:-go}

epoch=$(git log -1 --format=%ct)
stamp=$(TZ=UTC date -u -r "$epoch" +%Y%m%d%H%M.%S 2>/dev/null || TZ=UTC date -u -d "@$epoch" +%Y%m%d%H%M.%S)

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
for arch in amd64 arm64; do
	CGO_ENABLED=0 GOOS=linux GOARCH=$arch "$go" build -trimpath -buildvcs=false -ldflags "$ldflags" \
		-o "internal/runnerbin/bin/kipp-runner_linux_$arch" ./cmd/kipp-runner
done
for platform in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
	os=${platform%/*}
	arch=${platform#*/}
	dir=kipp_${name_version}_${os}_${arch}
	mkdir -p "dist/$dir"
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch "$go" build -trimpath -buildvcs=false -ldflags "$ldflags" \
		-o "dist/$dir/kipp" ./cmd/kipp
	cp LICENSE README.md THIRD_PARTY.md CHANGELOG.md "dist/$dir/"
	mkdir "dist/$dir/docs"
	cp docs/*.md "dist/$dir/docs/"
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
