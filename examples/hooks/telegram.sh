#!/bin/sh
# Tells a Telegram chat how a build ended, through a bot.
#
# The bot's token is a secret: kipp secret set TELEGRAM_BOT_TOKEN --data <dir>
# The chat's id is not, so it goes in the job's env:
#
#   env: {TELEGRAM_CHAT_ID: "123456789"}
#   secrets: [TELEGRAM_BOT_TOKEN]
#   on_failure: {run: /etc/kipp/hooks/telegram.sh}
#
# The token is part of the URL, which goes to curl on stdin, not as an
# argument, which anyone on the machine can read with ps.
set -eu
printf 'url = "https://api.telegram.org/bot%s/sendMessage"\n' "$TELEGRAM_BOT_TOKEN" | curl -fsS --max-time 20 -K - \
	--data-urlencode "chat_id=$TELEGRAM_CHAT_ID" \
	--data-urlencode "text=kipp: $KIPP_JOB #$KIPP_BUILD $KIPP_STATUS ${KIPP_URL%/}/jobs/$KIPP_JOB/$KIPP_BUILD" >/dev/null
