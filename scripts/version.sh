#!/bin/sh
set -eu

if v=$(git describe --tags --match 'v[0-9]*' --dirty 2>/dev/null); then
	echo "$v"
	exit 0
fi
when=$(TZ=UTC git log -1 --format=%cd --date=format-local:%Y%m%d%H%M%S)
hash=$(git rev-parse --short=12 HEAD)
dirty=
git diff --quiet HEAD -- 2>/dev/null || dirty=-dirty
echo "latest-$when-$hash$dirty"
