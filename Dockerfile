FROM --platform=$BUILDPLATFORM golang:1.27-trixie@sha256:c47c3fda018d13d43a6bf5ebac3e5bba8a23a35782aa4faf531afaf47c64c39f AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
ARG TAGS=
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN set -eu; \
    ldflags="-s -w -buildid= -X github.com/ndthuan/kipp/internal/version.Version=$VERSION"; \
    for arch in amd64 arm64; do \
      CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -buildvcs=false -ldflags "$ldflags" \
        -o internal/runnerbin/bin/kipp-runner_linux_$arch ./cmd/kipp-runner; \
    done; \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build ${TAGS:+-tags "$TAGS"} -trimpath -buildvcs=false -ldflags "$ldflags" \
      -o /out/kipp ./cmd/kipp

FROM debian:trixie-slim@sha256:f6b08b09a05725f9431502d3b5714f5c4576e82c1a84fa1debe0c24a7e0c3ec4
RUN apt-get update \
 && apt-get install -y --no-install-recommends bash ca-certificates curl git openssh-client tzdata zstd \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --uid 10001 --user-group --home-dir /var/lib/kipp --shell /usr/sbin/nologin kipp \
 && mkdir -p /var/lib/kipp /etc/kipp \
 && chown kipp:kipp /var/lib/kipp /etc/kipp \
 && chmod 700 /var/lib/kipp
COPY --from=build /out/kipp /usr/local/bin/kipp
COPY LICENSE THIRD_PARTY.md /usr/share/doc/kipp/

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
