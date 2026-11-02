#!/bin/sh
# smoke.sh bin PATH [VERSION]: start the kipp at PATH on a fresh data
# directory and a one-job jobs directory, make the first admin and a
# token, run the job to success through the API, and stop it.
# smoke.sh image REF [VERSION]: the same against the container image REF,
# with its two volumes and its own command line.
#
# With VERSION, `kipp version` must say it. What it proves is that the
# artifact a user downloads starts, serves, keeps its state where it says
# and runs a stage, not that every feature works: the gate does that.
set -eu
export LC_ALL=C

mode=${1:?usage: smoke.sh bin|image PATH|REF [VERSION]}
target=${2:?usage: smoke.sh bin|image PATH|REF [VERSION]}
want=${3-}
work=$(mktemp -d)
pid='' container=''

cleanup() {
	[ -z "$pid" ] || kill "$pid" 2>/dev/null || true
	[ -z "$container" ] || docker rm -f "$container" >/dev/null 2>&1 || true
	[ -z "$container" ] || docker volume rm "$container-data" "$container-config" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT INT TERM

fail() {
	echo "smoke: FAILED: $*" >&2
	if [ -n "$container" ]; then docker logs "$container" >&2 2>&1 || true; fi
	if [ -f "$work/serve.log" ]; then cat "$work/serve.log" >&2; fi
	exit 1
}

mkdir -p "$work/jobs"
# The stage's command is the job's, run by kipp's shell, not this one.
cat >"$work/jobs/hello.yml" <<'JOB'
name: hello
stages:
  greet: {run: echo "hello from $(uname -s)"}
JOB
chmod 644 "$work/jobs/hello.yml"

case $mode in
bin)
	port=$((20000 + $$ % 20000))
	url="http://127.0.0.1:$port"
	kipp() { "$target" "$@"; }
	data="$work/data"
	# Started directly, not through kipp(): $! is then the server itself,
	# which cleanup stops, and not a subshell that would leave it running.
	"$target" serve --data "$data" --jobs "$work/jobs" --listen "127.0.0.1:$port" >"$work/serve.log" 2>&1 &
	pid=$!
	;;
image)
	container="kipp-smoke-$$"
	url=''
	kipp() { docker exec -i -e KIPP_TOKEN="${KIPP_TOKEN-}" "$container" kipp "$@"; }
	data=/var/lib/kipp
	# The job goes into the config volume by docker cp rather than a bind
	# mount, which a Docker in a VM (Colima, Docker Desktop) may not share.
	docker create --name "$container" -p 127.0.0.1::8085 \
		-v "$container-data:/var/lib/kipp" -v "$container-config:/etc/kipp" "$target" >/dev/null ||
		fail "the container was not created"
	docker cp "$work/jobs/." "$container:/etc/kipp/" || fail "docker cp of the job"
	docker start "$container" >/dev/null || fail "the container did not start"
	;;
*)
	echo "usage: smoke.sh bin|image PATH|REF [VERSION]" >&2
	exit 2
	;;
esac

got=$(kipp version) || fail "kipp version"
echo "smoke: $got"
[ -z "$want" ] || [ "$got" = "kipp $want" ] || fail "kipp version says '$got', want 'kipp $want'"

# The server is up once its health answers; the image's from inside.
health() {
	if [ "$mode" = bin ]; then
		curl -fsS "$url/api/v1/health"
	else
		docker exec "$container" curl -fsS http://127.0.0.1:8085/api/v1/health
	fi
}
i=0
until out=$(health 2>/dev/null); do
	i=$((i + 1))
	[ "$i" -lt 60 ] || fail "no answer from /api/v1/health in 30 seconds"
	sleep 0.5
done
case $out in *'"ok":true'*) ;; *) fail "health answered '$out'" ;; esac
echo "smoke: health $out"

kipp user add admin --admin --data "$data" >"$work/admin" || fail "user add"
grep -q '^admin password: ' "$work/admin" || fail "user add printed no password"
KIPP_TOKEN=$(kipp token create admin --label smoke --data "$data" | tail -n 1) || fail "token create"
export KIPP_TOKEN
case $KIPP_TOKEN in kipp_*) ;; *) fail "token create printed no token" ;; esac

if [ "$mode" = bin ]; then
	run=$(KIPP_URL="$url" kipp run hello --wait) || fail "kipp run: $run"
else
	run=$(kipp run hello --wait --url http://127.0.0.1:8085) || fail "kipp run: $run"
fi
printf '%s\n' "$run"
printf '%s\n' "$run" | grep -q '^\[greet\] hello from ' || fail "the stage's output is missing"
printf '%s\n' "$run" | grep -q '^hello #1: success$' || fail "the build did not succeed"

# The state is where it was said to be: the database in the data directory.
if [ "$mode" = bin ]; then
	[ -f "$data/db.sqlite" ] || fail "no database in $data"
else
	docker exec "$container" test -f /var/lib/kipp/db.sqlite || fail "no database in the data volume"
fi
echo "smoke: ok ($mode $target)"
