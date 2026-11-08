#!/bin/sh
# smoke-upgrade.sh: the upgrade, end to end, in real containers.
#
# Two images are built from this checkout with the kipptest tag, stamped
# v1.0.0 and v1.0.1; the second one's binary is served as the "release" by a
# local fake release server and signed with a throwaway key. The first is
# started on a volume and upgraded through the API while a stage runs.
# The image published never has the tag: the script also builds the default
# image and checks that its binary holds none of the test doors.
#
# It needs docker (named by KIPP_TEST_DOCKER when that is an address such as
# unix:///path/docker.sock), go and python3. Without docker it prints
# NOT VERIFIED and exits 0, as the other docker tests of the gate do.
set -eu
export LC_ALL=C

if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
	echo "smoke-upgrade: NOT VERIFIED (no docker engine)"
	exit 0
fi
case ${KIPP_TEST_DOCKER-} in *://*) export DOCKER_HOST="$KIPP_TEST_DOCKER" ;; esac

root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
id=$$
prefix="kipp-upgrade-smoke-$id"
img="$prefix"
pids=''
covered=''

cover() { covered="$covered
  - $*"; }
say() { echo "smoke-upgrade: $*"; }

cleanup() {
	for p in $pids; do kill "$p" 2>/dev/null || true; done
	for c in $(docker ps -aq --filter "label=$prefix"); do docker rm -f "$c" >/dev/null 2>&1 || true; done
	docker volume ls -q --filter "name=$prefix" | while read -r v; do docker volume rm "$v" >/dev/null 2>&1 || true; done
	docker rmi "$img:v1.0.0" "$img:v1.0.1" "$img:published" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT INT TERM

fail() {
	echo "smoke-upgrade: FAILED: $*" >&2
	for c in $(docker ps -aq --filter "label=$prefix"); do
		echo "--- logs of $(docker inspect -f '{{.Name}}' "$c")" >&2
		docker logs --tail 40 "$c" >&2 2>&1 || true
	done
	exit 1
}

arch=$(docker version --format '{{.Server.Arch}}')
case $arch in amd64 | arm64) ;; *) fail "unsupported engine architecture $arch" ;; esac

# ---- the key, the release tool and the images ------------------------------
cd "$root"
go build -o "$work/kipp-release" ./cmd/kipp-release
"$work/kipp-release" keygen -secret "$work/release.key" -pub "$work/release.pub" >/dev/null
chmod 644 "$work/release.pub"

build() { # build TAG [TAGS]
	docker build -q --build-arg "VERSION=$1" --build-arg "TAGS=${2-}" --label "$prefix" -t "$img:$1" . >/dev/null ||
		fail "docker build $1"
}
say "building the images (this is the slow part)"
build v1.0.0 kipptest
build v1.0.1 kipptest
build published ""

binary_of() { # binary_of IMAGE DEST: the kipp executable of an image
	c=$(docker create "$1")
	docker cp "$c:/usr/local/bin/kipp" "$2" >/dev/null
	docker rm "$c" >/dev/null
}
binary_of "$img:v1.0.1" "$work/kipp-v1.0.1"
binary_of "$img:published" "$work/kipp-published"
binary_of "$img:v1.0.0" "$work/kipp-v1.0.0"

grep -aq KIPP_TEST_UPDATE_API "$work/kipp-v1.0.0" || fail "the kipptest image has no test door: the check below proves nothing"
if grep -aq KIPP_TEST_ "$work/kipp-published"; then fail "the published image's kipp holds a KIPP_TEST_ door"; fi
cover "the default image (no TAGS) holds no KIPP_TEST_ door; the kipptest one does"

# ---- the fake release server -------------------------------------------------
rel="$work/rel"
mkdir -p "$rel/repos/ndthuan/kipp/releases"
port=$((20000 + id % 20000))
python3 -m http.server "$port" --bind 0.0.0.0 --directory "$rel" >"$work/release-server.log" 2>&1 &
pids="$pids $!"
api="http://host.docker.internal:$port"

# release TAG BINARY: serve TAG, signed, whose archive holds BINARY as kipp.
release() {
	tag=$1 v=${1#v}
	dir="$rel/ndthuan/kipp/releases/download/$tag"
	name="kipp_${v}_linux_$arch"
	rm -rf "$dir" "$work/stage"
	mkdir -p "$dir" "$work/stage/$name"
	cp "$2" "$work/stage/$name/kipp"
	chmod 755 "$work/stage/$name/kipp"
	echo "kipp $tag" >"$work/stage/$name/README.md"
	tar -C "$work/stage" -czf "$dir/$name.tar.gz" "$name"
	if command -v sha256sum >/dev/null 2>&1; then sum=$(sha256sum "$dir/$name.tar.gz"); else sum=$(shasum -a 256 "$dir/$name.tar.gz"); fi
	printf '%s  %s\n' "${sum%% *}" "$name.tar.gz" >"$dir/SHA256SUMS"
	"$work/kipp-release" sign -sums "$dir/SHA256SUMS" -key "$work/release.key" >/dev/null
	printf '{"tag_name":"%s","html_url":"x","assets":[{"name":"SHA256SUMS.sig"}]}' "$tag" >"$rel/repos/ndthuan/kipp/releases/latest"
	last_dir=$dir last_name=$name
}

# ---- containers --------------------------------------------------------------
# create NAME IMAGE: a container on volumes of its own, with the jobs and the
# test key put in, not yet started.
create() {
	c=$prefix-$1
	docker create --name "$c" --label "$prefix" -p 127.0.0.1::8085 \
		--add-host host.docker.internal:host-gateway \
		-e "KIPP_TEST_UPDATE_API=$api" -e KIPP_TEST_RELEASE_PUBKEY=/tmp/release.pub \
		-v "$c-data:/var/lib/kipp" -v "$c-config:/etc/kipp" "$2" >/dev/null || fail "create $1"
	docker cp "$work/release.pub" "$c:/tmp/release.pub" || fail "docker cp of the key"
	docker cp "$work/jobs/." "$c:/etc/kipp/" || fail "docker cp of the jobs"
}
mkdir -p "$work/jobs"
cat >"$work/jobs/hello.yml" <<'JOB'
name: hello
stages:
  greet: {run: echo hello}
JOB
cat >"$work/jobs/slow.yml" <<'JOB'
name: slow
stages:
  work: {run: "sleep 12; echo finished"}
JOB
chmod 644 "$work/jobs/"*.yml

url=http://127.0.0.1:8085
cx() { docker exec -i "$c" "$@"; }
kx() { docker exec -i -e KIPP_TOKEN="${token-}" "$c" kipp "$@" --url "$url"; }
wait_health() {
	i=0
	until docker exec "$c" curl -fsS "$url/api/v1/health" >/dev/null 2>&1; do
		i=$((i + 1))
		[ "$i" -lt 120 ] || fail "no health in 60 seconds"
		sleep 0.5
	done
}
# started_at and the start tick of PID 1 together say it is the same process.
pid1() {
	st=$(cx sh -c 'sed "s/.*) //" /proc/1/stat | cut -d" " -f20')
	echo "$(docker inspect -f '{{.State.StartedAt}} {{.RestartCount}}' "$c") $st"
}
# serving_exe is the path of the executable a kipp serve in the container runs.
find_serving=$(
	cat <<'SH'
for d in /proc/[0-9]*; do
	if tr "\0" " " <"$d/cmdline" 2>/dev/null | grep -q "kipp.* serve"; then readlink "$d/exe"; fi
done
SH
)
serving_exe() { cx sh -c "$find_serving"; }
api_get() { cx curl -fsS -H "Authorization: Bearer $token" "$url$1"; }
expect_in() { # expect_in TEXT WANT WHAT
	case $1 in *"$2"*) ;; *) fail "$3: wanted '$2' in: $1" ;; esac
}

make_admin() {
	cx kipp user add admin --admin --data /var/lib/kipp >"$work/admin" || fail "user add"
	token=$(cx kipp token create admin --label smoke --data /var/lib/kipp | tail -n 1) || fail "token create"
	case $token in kipp_*) ;; *) fail "no token" ;; esac
}

# ============================================================================
# 1. A data volume the server cannot write its upgrade files to
# ============================================================================
release v1.0.1 "$work/kipp-v1.0.1"
create ro "$img:v1.0.0"
c=$prefix-ro
# A volume restored by root: bin/ and upgrade/ are there and not the server's.
docker run --rm --user 0 --label "$prefix" -v "$c-data:/var/lib/kipp" --entrypoint sh "$img:v1.0.0" \
	-c 'mkdir -p /var/lib/kipp/bin /var/lib/kipp/upgrade && chown root:root /var/lib/kipp/bin /var/lib/kipp/upgrade && chmod 755 /var/lib/kipp/bin /var/lib/kipp/upgrade' ||
	fail "preparing the read-only volume"
docker start "$c" >/dev/null || fail "start ro"
wait_health
make_admin
i=0
until out=$(api_get /api/v1/admin/server) && case $out in *'"latest":"v1.0.1"'*) true ;; *) false ;; esac; do
	i=$((i + 1))
	[ "$i" -lt 60 ] || fail "the read-only server never saw v1.0.1: $out"
	sleep 0.5
done
if out=$(kx upgrade 2>&1); then fail "kipp upgrade went ahead on a data volume it cannot write"; fi
expect_in "$out" "cannot upgrade itself" "the refusal"
expect_in "$out" "docker pull ghcr.io/ndthuan/kipp:1.0.1" "the refusal's manual lines"
expect_in "$out" "docker run" "the refusal's manual lines"
[ -z "$(cx ls /var/lib/kipp/bin)" ] || fail "the refused upgrade left files in bin/"
cover "a data volume the server cannot write to: the upgrade is refused with docker pull and docker run lines, and nothing is left behind"
docker rm -f "$c" >/dev/null

# ============================================================================
# 2. The upgrade, with a stage running
# ============================================================================
create a "$img:v1.0.0"
c=$prefix-a
docker start "$c" >/dev/null || fail "start a"
wait_health
make_admin
expect_in "$(cx kipp version)" "installed from: Docker image" "the install source"
expect_in "$(cx kipp version | head -n 1)" "kipp v1.0.0" "the version before"
i=0
until out=$(api_get /api/v1/admin/server) && case $out in *'"latest":"v1.0.1"'*) true ;; *) false ;; esac; do
	i=$((i + 1))
	[ "$i" -lt 60 ] || fail "the server never saw v1.0.1: $out"
	sleep 0.5
done
expect_in "$out" '"upgradable":true' "the release is upgradable"

kx run slow --wait >"$work/slow.out" 2>&1 &
slow=$!
i=0
until case $(kx quiet status 2>&1) in *"1 running"*) true ;; *) false ;; esac; do
	i=$((i + 1))
	[ "$i" -lt 60 ] || fail "the slow build never ran"
	sleep 0.5
done
before=$(pid1)
started=$(docker inspect -f '{{.State.StartedAt}}' "$c")
kx upgrade >"$work/up.out" 2>&1 &
up=$!
i=0
until case $(kx upgrade status 2>&1) in *waiting*) true ;; *) false ;; esac; do
	i=$((i + 1))
	[ "$i" -lt 60 ] || fail "the upgrade never waited for the build: $(cat "$work/up.out")"
	sleep 0.5
done
wait "$slow" || fail "the slow build: $(cat "$work/slow.out")"
grep -q '^slow #1: success$' "$work/slow.out" || fail "the build the upgrade waited for did not end success: $(cat "$work/slow.out")"
wait "$up" || fail "kipp upgrade: $(cat "$work/up.out")"
expect_in "$(cat "$work/up.out")" "kipp v1.0.1 is running" "the upgrade's last line"
cover "the upgrade with safe restart waited for a running stage, which ended success (not lost)"

after=$(pid1)
[ "$before" = "$after" ] || fail "PID 1 changed: '$before' then '$after'"
[ "$(docker inspect -f '{{.State.StartedAt}}' "$c")" = "$started" ] || fail "the container restarted"
cover "PID 1 is the same process before and after (no container restart)"

wait_health
exe=$(serving_exe)
expect_in "$exe" "/var/lib/kipp/bin/kipp-v1.0.1" "the binary serving"
expect_in "$(cx /var/lib/kipp/bin/kipp-v1.0.1 version | head -n 1)" "kipp v1.0.1" "the data directory's binary"
info=$(api_get /api/v1/admin/server)
expect_in "$info" '"version":"v1.0.1"' "the server's version"
expect_in "$info" "data directory" "the server's origin"
expect_in "$(kx upgrade status)" "done" "the status after"
cover "kipp v1.0.1 serves from the data directory (the image's own executable is v1.0.0)"

# a restart of the same container keeps the upgraded version
marker=$(cx kipp user list --data /var/lib/kipp)
docker restart "$c" >/dev/null || fail "docker restart"
wait_health
expect_in "$(serving_exe)" "/var/lib/kipp/bin/kipp-v1.0.1" "the binary serving after docker restart"
expect_in "$(api_get /api/v1/admin/server)" '"version":"v1.0.1"' "the version after docker restart"
cover "docker restart keeps v1.0.1"

# ============================================================================
# 3. A corrupt release is refused before the server is touched
# ============================================================================
release v1.0.2 "$work/kipp-v1.0.1"
printf 'x' >>"$last_dir/$last_name.tar.gz"
# The answer of the last lookup is kept for twelve hours, across a restart.
cx rm -f /var/lib/kipp/update-check.json
docker restart "$c" >/dev/null || fail "docker restart"
wait_health
i=0
until out=$(api_get /api/v1/admin/server) && case $out in *'"latest":"v1.0.2"'*) true ;; *) false ;; esac; do
	i=$((i + 1))
	[ "$i" -lt 60 ] || fail "the server never saw v1.0.2: $out"
	sleep 0.5
done
before=$(pid1)
if out=$(kx upgrade 2>&1); then fail "kipp upgrade took a corrupt release: $out"; fi
say "the corrupt release was refused: $(printf '%s' "$out" | tail -n 1)"
[ "$before" = "$(pid1)" ] || fail "the refusal touched the server (PID 1 changed)"
cx test ! -e /var/lib/kipp/bin/pending || fail "the refusal wrote a pending record"
expect_in "$(kx quiet status)" "quiet off" "quiet after the refusal"
expect_in "$(kx run hello --wait)" "hello #1: success" "a build after the refusal"
cover "a release whose archive does not match its signed checksum is refused before the server is touched (same PID 1, no pending, not quiet)"

# ============================================================================
# 4. A release whose binary exits at once is rolled back
# ============================================================================
cat >"$work/dies" <<'SCRIPT'
#!/bin/sh
# Says it is v1.0.2, and as a server writes over the data and stops.
if [ "$1" = version ]; then echo "kipp v1.0.2"; exit 0; fi
printf 'junk' >/var/lib/kipp/secret.key
printf 'junk' >>/var/lib/kipp/db.sqlite
exit 1
SCRIPT
release v1.0.2 "$work/dies"
rows_before=$(cx kipp user list --data /var/lib/kipp)
key_before=$(cx sha256sum /var/lib/kipp/secret.key)
if out=$(kx upgrade 2>&1); then fail "kipp upgrade reported success for a binary that dies: $out"; fi
say "the dying release: $(printf '%s' "$out" | tail -n 1)"
wait_health
i=0
until case $(kx upgrade status 2>&1) in *failed*) true ;; *) false ;; esac; do
	i=$((i + 1))
	[ "$i" -lt 60 ] || fail "no failed status after the rollback: $(kx upgrade status 2>&1)"
	sleep 0.5
done
expect_in "$(serving_exe)" "/var/lib/kipp/bin/kipp-v1.0.1" "the binary serving after the rollback"
expect_in "$(api_get /api/v1/admin/server)" '"version":"v1.0.1"' "the version after the rollback"
[ "$(cx kipp user list --data /var/lib/kipp)" = "$rows_before" ] || fail "the users differ after the rollback"
[ "$(cx sha256sum /var/lib/kipp/secret.key)" = "$key_before" ] || fail "the key differs after the rollback"
[ "$(cx kipp user list --data /var/lib/kipp)" = "$marker" ] || fail "the users differ from before the first upgrade"
expect_in "$(kx run hello --wait)" "hello #2: success" "a build after the rollback"
cover "a release whose binary exits at once is rolled back to v1.0.1, with the database rows and the key as before"

echo "smoke-upgrade: ok. Covered:$covered"
