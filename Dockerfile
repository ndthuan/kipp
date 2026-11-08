# kipp in a container: the server, with a shell, git and ssh for the
# stages it runs itself. The image is built for linux/amd64 and
# linux/arm64; Docker on a Mac runs the one of its CPU.
#
#   docker run -d --name kipp -p 8085:8085 \
#     -v kipp-data:/var/lib/kipp -v ./jobs:/etc/kipp ghcr.io/ndthuan/kipp
#
# Two volumes, one per kind of state: /var/lib/kipp is the data directory
# (the database, the secrets' key, logs, workspaces), written by kipp
# alone and backed up as a whole; /etc/kipp is the jobs directory (the job
# files and _vars.yml), read by kipp and written by whoever manages the
# jobs.

# The build runs on the builder's own platform and cross-compiles: no
# emulated Go toolchain, and the Linux agents every kipp embeds are the
# same for both images.
FROM --platform=$BUILDPLATFORM golang:1.27-trixie@sha256:c65ecfe613b539ac74c62a4c3b2c27e8f8a6076384c8f27e8a3c0f3be17cc385 AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
# Only scripts/smoke-upgrade.sh sets TAGS (to kipptest, for the doors a test
# of the upgrade needs); the published image is built without it.
ARG TAGS=
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN set -eu; \
    ldflags="-s -w -buildid= -X github.com/ndthuan/kipp/internal/version.Version=$VERSION"; \
    for arch in amd64 arm64; do \
      CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -buildvcs=false -ldflags "$ldflags" \
        -o internal/agentbin/bin/kipp-agent_linux_$arch ./cmd/kipp-agent; \
    done; \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build ${TAGS:+-tags "$TAGS"} -trimpath -buildvcs=false -ldflags "$ldflags" \
      -o /out/kipp ./cmd/kipp

FROM debian:trixie-slim@sha256:f6b08b09a05725f9431502d3b5714f5c4576e82c1a84fa1debe0c24a7e0c3ec4
# What a stage most often reaches for; anything else is a stage's own
# image (runs_on with an engine), or an image built FROM this one.
RUN apt-get update \
 && apt-get install -y --no-install-recommends bash ca-certificates curl git openssh-client tzdata zstd \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --uid 10001 --user-group --home-dir /var/lib/kipp --shell /usr/sbin/nologin kipp \
 && mkdir -p /var/lib/kipp /etc/kipp \
 && chown kipp:kipp /var/lib/kipp /etc/kipp \
 && chmod 700 /var/lib/kipp
COPY --from=build /out/kipp /usr/local/bin/kipp
COPY LICENSE THIRD_PARTY.md /usr/share/doc/kipp/

# Names the image as the install source: the Server page and `kipp version`
# say so, and an upgrade keeps its binary in the data volume.
ENV KIPP_INSTALL_SOURCE=docker
USER kipp
WORKDIR /var/lib/kipp
VOLUME ["/var/lib/kipp", "/etc/kipp"]
EXPOSE 8085
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s \
  CMD curl -fsS http://127.0.0.1:8085/api/v1/health >/dev/null || exit 1
ENTRYPOINT ["kipp"]
CMD ["serve", "--data", "/var/lib/kipp", "--jobs", "/etc/kipp", "--listen", "0.0.0.0:8085", "--shell", "/bin/bash"]

LABEL org.opencontainers.image.title="kipp" \
      org.opencontainers.image.description="A job-first CI and scheduler in a single binary" \
      org.opencontainers.image.source="https://github.com/ndthuan/kipp" \
      org.opencontainers.image.licenses="MIT"
