# aixecutor — build / test / lint / fmt / run
#
# Version stamping uses ONLY read-only git commands (rev-parse / describe /
# log). The application must never run mutating git operations (CLAUDE.md §2).

BINARY      := bin/aixecutor
PKG         := github.com/jaxmef/aixecutor/internal/cli

# Read-only git metadata, with safe fallbacks when git/tags are unavailable.
VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE        := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS     := -X $(PKG).ldflagVersion=$(VERSION) -X $(PKG).ldflagCommit=$(COMMIT) -X $(PKG).ldflagDate=$(DATE)

.PHONY: build test lint fmt run

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) .

test:
	go test ./...

lint:
	go vet ./...
	@test -z "$$(gofmt -l .)" || { echo "gofmt needs to run on:"; gofmt -l .; exit 1; }

fmt:
	gofmt -w .

run: build
	$(BINARY)
