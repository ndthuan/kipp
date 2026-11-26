#!/bin/sh
# Tells a Slack channel how a build ended, through an incoming webhook.
#
# The webhook's URL is a secret: kipp secret set SLACK_WEBHOOK_URL --data <dir>
# and in the job:
#
#   secrets: [SLACK_WEBHOOK_URL]
#   on_failure: {run: /etc/kipp/hooks/slack.sh}
#
# KIPP_JOB is a job name (letters, digits, . - _) and KIPP_STATUS a word,
# so neither needs escaping in the JSON. The URL goes to curl on stdin,
# not as an argument, which anyone on the machine can read with ps.
set -eu
printf 'url = "%s"\n' "$SLACK_WEBHOOK_URL" | curl -fsS --max-time 20 -K - \
	-H 'Content-Type: application/json' \
	-d "{\"text\": \"kipp: $KIPP_JOB #$KIPP_BUILD $KIPP_STATUS ${KIPP_URL%/}/jobs/$KIPP_JOB/$KIPP_BUILD\"}" >/dev/null
