#!/bin/sh
set -eu

if [ $# -lt 2 ] || [ $# -gt 3 ]; then
	echo "usage: sign-release.sh sign|verify DIR [PUBFILE]" >&2
	exit 2
fi
mode=$1
dir=$2
pub=${3-}

cd "$(dirname "$0")/.."
case $dir in /*) ;; *) dir=$OLDPWD/$dir ;; esac
if [ -n "$pub" ]; then
	case $pub in /*) ;; *) pub=$OLDPWD/$pub ;; esac
fi

verify() {
	archive=
	for f in "$dir"/kipp_*.tar.gz; do
		[ -e "$f" ] && archive=$f && break
	done
	if [ -z "$archive" ]; then
		echo "sign-release.sh: no archive in $dir" >&2
		exit 1
	fi
	if [ -n "$pub" ]; then
		go run ./cmd/kipp-release verify -sums "$dir/SHA256SUMS" -sig "$dir/SHA256SUMS.sig" -archive "$archive" -pub "$pub"
	else
		go run ./cmd/kipp-release verify -sums "$dir/SHA256SUMS" -sig "$dir/SHA256SUMS.sig" -archive "$archive"
	fi
}

case $mode in
sign)
	if [ -z "${RELEASE_SIGNING_KEY-}" ]; then
		echo "sign-release.sh: RELEASE_SIGNING_KEY is not set" >&2
		exit 1
	fi
	go run ./cmd/kipp-release sign -sums "$dir/SHA256SUMS" -key-env RELEASE_SIGNING_KEY
	verify
	;;
verify)
	verify
	;;
*)
	echo "usage: sign-release.sh sign|verify DIR [PUBFILE]" >&2
	exit 2
	;;
esac
