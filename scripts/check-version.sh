#!/bin/sh
set -eu
export LC_ALL=C

release=false
if [ "${1-}" = --release ]; then
	release=true
	shift
fi
version=${1-}

case $version in
'' | *[!0-9A-Za-z.-]*)
	echo "check-version.sh: refused version '$version'" >&2
	exit 1
	;;
esac

if $release; then
	pattern='v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.]+)?'
else
	pattern='v?[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+(-[0-9A-Za-z.]+)*)?|[0-9a-f]{7,40}(-dirty)?|latest-[0-9]{14}-[0-9a-f]{12}(-dirty)?'
fi
if ! printf '%s\n' "$version" | grep -Eqx "$pattern"; then
	echo "check-version.sh: refused version '$version'" >&2
	exit 1
fi
