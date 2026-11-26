#!/bin/sh
set -eu

version=${1:?usage: release-notes.sh VERSION}
version=${version#v}

notes=$(awk -v v="$version" '
	/^## / { if (found) exit; if (index($0, "## [" v "]") == 1) { found = 1; next } }
	found { print }
' CHANGELOG.md)

if [ -z "$(printf '%s' "$notes" | tr -d '[:space:]')" ]; then
	echo "release-notes.sh: CHANGELOG.md has no section \"## [$version]\" with anything in it" >&2
	exit 1
fi
printf '%s\n' "$notes"
