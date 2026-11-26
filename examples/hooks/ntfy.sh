#!/bin/sh
# Tells an ntfy topic how a build ended (https://ntfy.sh, or a server of
# your own in NTFY_URL).
#
# The access token is a secret: kipp secret set NTFY_TOKEN --data <dir>
# The topic is not, so it goes in the job's env:
#
#   env: {NTFY_TOPIC: builds}
#   secrets: [NTFY_TOKEN]
#   on_failure: {run: /etc/kipp/hooks/ntfy.sh}
#
# The token goes to curl on stdin, not as an argument, which anyone on the
# machine can read with ps.
set -eu
printf 'header = "Authorization: Bearer %s"\n' "$NTFY_TOKEN" | curl -fsS --max-time 20 -K - \
	-H "Title: kipp: $KIPP_JOB #$KIPP_BUILD $KIPP_STATUS" \
	-H "Click: ${KIPP_URL%/}/jobs/$KIPP_JOB/$KIPP_BUILD" \
	-d "$KIPP_JOB #$KIPP_BUILD ended $KIPP_STATUS." \
	"${NTFY_URL:-https://ntfy.sh}/$NTFY_TOPIC" >/dev/null
