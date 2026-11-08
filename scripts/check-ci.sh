#!/bin/sh
# check-ci.sh REPO SHA: the ci workflow has a run on SHA, on a push to main,
# that completed with conclusion success. A release is built from a commit
# the gate has already passed; the gate is never run a second time, since
# every extra run is another chance for a flaky test to fail the release.
#
# A run still going is waited for, CI_WAIT_SECONDS (default 1800) at most,
# polling every CI_POLL_SECONDS (default 30). No run, a failed one or the
# wait running out fails. The newest run for SHA decides. Needs gh with a
# token that may read actions.
set -eu

if [ $# -ne 2 ]; then
	echo "usage: check-ci.sh REPO SHA" >&2
	exit 2
fi
repo=$1
sha=$2
wait=${CI_WAIT_SECONDS-1800}
poll=${CI_POLL_SECONDS-30}
waited=0

while :; do
	if ! state=$(gh api "repos/$repo/actions/workflows/ci.yml/runs?head_sha=$sha&event=push&branch=main&per_page=1" \
		--jq '.workflow_runs[0] | if . == null then "none" else "\(.status) \(.conclusion) \(.html_url)" end'); then
		echo "check-ci.sh: could not list the ci runs of $sha in $repo" >&2
		exit 1
	fi
	case "$state" in
	none)
		echo "check-ci.sh: ci has no run on $sha for a push to main; push it to main and let ci pass first" >&2
		exit 1
		;;
	"completed success "*)
		echo "ci passed on $sha: ${state#completed success }"
		exit 0
		;;
	completed*)
		echo "check-ci.sh: the newest ci run on $sha did not pass: $state" >&2
		exit 1
		;;
	esac
	if [ "$waited" -ge "$wait" ]; then
		echo "check-ci.sh: ci on $sha was still running after ${wait}s: $state" >&2
		exit 1
	fi
	echo "ci on $sha is still running; waiting"
	sleep "$poll"
	waited=$((waited + poll))
done
