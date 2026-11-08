#!/bin/sh
# check-release-key.sh [FILE]: FILE (default internal/update/release.pub) is
# a real release key, not the placeholder the repository carries until the
# maintainer makes one. A release is built only when this passes.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
file=${1-$here/../internal/update/release.pub}

if [ ! -f "$file" ]; then
	echo "check-release-key.sh: $file not found" >&2
	exit 1
fi
if grep -q '^# TEST-ONLY PLACEHOLDER' "$file"; then
	echo "check-release-key.sh: $file is the test-only placeholder; make the release key first" >&2
	exit 1
fi
