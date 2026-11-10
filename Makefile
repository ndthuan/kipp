GO ?= go
GOLANGCI = github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
GOVULNCHECK = golang.org/x/vuln/cmd/govulncheck@v1.9.0
ACTIONLINT = github.com/rhysd/actionlint/cmd/actionlint@v1.7.12

.DEFAULT_GOAL := quality
.PHONY: help quality fmt lint vuln actions test cover docker js runners build check openapi dist
help:   ## list the targets
	@grep -E '^[a-z]+:.*## ' $(MAKEFILE_LIST) | sed -E 's/^([a-z]+):.*## (.*)/  \1\t\2/' | expand -t 10

quality: fmt lint vuln actions test docker js check   ## the gate: all of the below, run before proposing a change

fmt:   ## gofmt, as a check
	@unformatted="$$(gofmt -l .)"; if [ -n "$$unformatted" ]; then echo "gofmt: not formatted:"; echo "$$unformatted"; exit 1; fi

lint:   ## go vet and golangci-lint
	$(GO) vet ./...
	$(GO) run $(GOLANGCI) run ./...

# It fails on a known vulnerability the code reaches, in a module or in the
# standard library of the Go that builds it.
vuln:   ## govulncheck
	$(GO) run $(GOVULNCHECK) ./...

# actionlint runs shellcheck on the workflows' run steps when shellcheck is
# here; scripts/ is checked apart.
actions:   ## actionlint on the GitHub workflows, shellcheck on scripts/
	$(GO) run $(ACTIONLINT)
	@if command -v shellcheck >/dev/null 2>&1; then shellcheck scripts/*.sh && echo "scripts: ok"; else echo "workflow run steps, scripts/: NOT VERIFIED (shellcheck not found)"; fi

test:   ## go test -race, with the coverage profile in coverage.out
	$(GO) test -race -count=1 -covermode=atomic -coverprofile=coverage.out ./...

# The report is a kipp metrics file (metric coverage, one item per package
# and total); with COVERAGE_BASE naming an earlier report, the total is
# guarded against it as CI guards it against main.
COVERAGE_BASE ?=
cover:   ## coverage.json from coverage.out, and the total against COVERAGE_BASE
	$(GO) run ./internal/devtools/coverage report -profile coverage.out -o coverage.json
	$(GO) run ./internal/devtools/coverage guard -base '$(COVERAGE_BASE)' -head coverage.json

# The engine tests run against a real Docker engine when KIPP_TEST_DOCKER
# names one (unix://, tcp:// or ssh://, as kipp engine add takes it), with
# KIPP_TEST_RUNNER_HOST an address of this machine a container there
# reaches; the fake engine covers the same calls in test. The end-to-end
# smoke of the upgrade (scripts/smoke-upgrade.sh) runs in containers on the
# same engine.
docker: runners   ## the engine tests against the engine KIPP_TEST_DOCKER names
	@if [ -n "$$KIPP_TEST_DOCKER" ]; then $(GO) test -race -count=1 -tags dockerreal ./internal/engine/... ./internal/enginepool/... && sh scripts/smoke-upgrade.sh && echo "docker: verified against $$KIPP_TEST_DOCKER"; else echo "docker: NOT VERIFIED (KIPP_TEST_DOCKER not set)"; fi

# build.js, copy.js, metrics.js, nav.js, queue.js, grants.js, scopes.js, theme.js and upgrade.js run in a browser; node stands in for one when it is here.
js:   ## the tests of the page scripts in internal/web/static under node
	@if command -v node >/dev/null 2>&1; then node internal/web/build_js_test.mjs && node internal/web/copy_js_test.mjs && node internal/web/metrics_js_test.mjs && node internal/web/nav_js_test.mjs && node internal/web/queue_js_test.mjs && node internal/web/scopes_js_test.mjs && node internal/web/theme_js_test.mjs && node internal/web/upgrade_js_test.mjs && node internal/web/grants_js_test.mjs && node internal/web/jobs_js_test.mjs; else echo "build.js, copy.js, metrics.js, nav.js, queue.js, scopes.js, theme.js, upgrade.js, grants.js, jobs.js: NOT VERIFIED (node not found)"; fi

# The runners kipp puts into the containers it starts ride inside it, so
# they are built first: a kipp built without them runs every stage but one
# in a container.
runners:   ## the Linux runners, into internal/runnerbin/bin for kipp to embed
	@for arch in amd64 arm64; do \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build -trimpath -buildvcs=false -ldflags "-s -w -buildid=" \
			-o internal/runnerbin/bin/kipp-runner_linux_$$arch ./cmd/kipp-runner || exit 1; \
	done

build: runners   ## bin/kipp, with the runners in it
	$(GO) build -trimpath -o bin/kipp ./cmd/kipp

check: build   ## kipp check on the test jobs and the examples
	./bin/kipp check testdata/jobs
	./bin/kipp check examples/jobs

# The single-file description of the API, bundled from the split sources in
# api/. `go test` fails when the committed file is not what this writes.
openapi:   ## api/openapi.gen.yaml, bundled from the split sources in api/
	$(GO) run ./internal/devtools/openapibundle

# VERSION names the release; a tag's build passes the tag. It reaches
# dist.sh through the environment, so that the shell takes it as one word
# whatever it holds, and dist.sh checks it.
VERSION ?= $(shell git describe --tags --always --dirty)

dist: export VERSION := $(VERSION)
dist:   ## the release archives and SHA256SUMS in dist/
	GO='$(GO)' ./scripts/dist.sh "$$VERSION"
