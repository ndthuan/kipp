GO ?= go
GOLANGCI = github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
GOVULNCHECK = golang.org/x/vuln/cmd/govulncheck@v1.9.0
ACTIONLINT = github.com/rhysd/actionlint/cmd/actionlint@v1.7.12

export GOTOOLCHAIN ?= go$(shell sed -n 's/^go //p' go.mod)+auto

.DEFAULT_GOAL := quality
.PHONY: help quality gogate fmt lint vuln actions test cover docker js runners build check openapi dist screenshots
help:   ## list the targets
	@grep -E '^[a-z]+:.*## ' $(MAKEFILE_LIST) | sed -E 's/^([a-z]+):.*## (.*)/\1\t\2/' | awk -F '\t' '{ printf "  %-12s %s\n", $$1, $$2 }'

quality: gogate js   ## the gate: gogate and js, run before proposing a change

gogate: fmt lint vuln actions test docker check   ## the gate without the page scripts

fmt:   ## gofmt, as a check
	@unformatted="$$(gofmt -l .)"; if [ -n "$$unformatted" ]; then echo "gofmt: not formatted:"; echo "$$unformatted"; exit 1; fi

lint:   ## go vet and golangci-lint
	$(GO) vet ./...
	$(GO) run $(GOLANGCI) run ./...

vuln:   ## govulncheck
	$(GO) run $(GOVULNCHECK) ./...

actions:   ## actionlint on the GitHub workflows, shellcheck on scripts/
	$(GO) run $(ACTIONLINT)
	@if command -v shellcheck >/dev/null 2>&1; then shellcheck scripts/*.sh && echo "scripts: ok"; else echo "workflow run steps, scripts/: NOT VERIFIED (shellcheck not found)"; fi

test:   ## go test -race, with the coverage profile in coverage.out
	$(GO) test -race -count=1 -covermode=atomic -coverprofile=coverage.out ./...

COVERAGE_BASE ?=
cover:   ## coverage.json from coverage.out, and the total against COVERAGE_BASE
	$(GO) run ./internal/devtools/coverage report -profile coverage.out -o coverage.json
	$(GO) run ./internal/devtools/coverage guard -base '$(COVERAGE_BASE)' -head coverage.json

docker: runners   ## the engine tests against the engine KIPP_TEST_DOCKER names
	@if [ -n "$$KIPP_TEST_DOCKER" ]; then $(GO) test -race -count=1 -tags dockerreal ./internal/engine/... ./internal/enginepool/... && sh scripts/smoke-upgrade.sh && echo "docker: verified against $$KIPP_TEST_DOCKER"; else echo "docker: NOT VERIFIED (KIPP_TEST_DOCKER not set)"; fi

JS_TESTS = $(sort $(wildcard internal/web/*_js_test.mjs))
js:   ## the tests of the page scripts in internal/web/static under node
	@if ! command -v node >/dev/null 2>&1; then echo "page scripts: NOT VERIFIED (node not found)"; exit 0; fi; \
	for t in $(JS_TESTS); do node "$$t" || exit 1; done

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

openapi:   ## api/openapi.gen.yaml, bundled from the split sources in api/
	$(GO) run ./internal/devtools/openapibundle

VERSION ?= $(shell ./scripts/version.sh)

PLAYWRIGHT_VERSION ?= 1.65.1
SHOTS_TOOLS ?= $(or $(TMPDIR),/tmp)/kipp-screenshots

screenshots:   ## docs/screenshots from a running demo server (AGENTS.md)
	npm install --silent --no-save --no-audit --no-fund --prefix '$(SHOTS_TOOLS)' playwright@$(PLAYWRIGHT_VERSION)
	NODE_PATH='$(SHOTS_TOOLS)/node_modules' node internal/devtools/screenshots/shoot.cjs

dist: export VERSION := $(VERSION)
dist:   ## the release archives and SHA256SUMS in dist/
	GO='$(GO)' ./scripts/dist.sh "$$VERSION"
