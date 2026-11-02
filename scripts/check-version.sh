#!/bin/sh
# check-version.sh VERSION: VERSION is one a build may be named: a release
# tag (v1.2.3, v1.2.3-rc.1, with or without the v) or what
# `git describe --tags --always --dirty` says of a local tree.
# check-version.sh --release TAG: TAG is one a release may have, v1.2.3
# with an optional pre-release after a hyphen.
#
# The name reaches a shell, a file name and the binary: whatever else a
# tag may hold (git allows ; ` $ and quotes in one) is refused here.
set -eu
export LC_ALL=C

release=false
if [ "${1-}" = --release ]; then
	release=true
	shift
fi
version=${1-}

# One line of letters, digits, dots and hyphens and nothing else: grep
# matches line by line, so a newline is refused before it looks.
case $version in
'' | *[!0-9A-Za-z.-]*)
	echo "check-version.sh: refused version '$version'" >&2
	exit 1
	;;
esac

if $release; then
	pattern='v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?'
else
	pattern='v?[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+(-[0-9A-Za-z.]+)*)?|[0-9a-f]{7,40}(-dirty)?'
fi
if ! printf '%s\n' "$version" | grep -Eqx "$pattern"; then
	echo "check-version.sh: refused version '$version'" >&2
	exit 1
fi
