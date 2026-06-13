SHELL := /bin/bash
.DEFAULT_GOAL := build

# Version in Go pseudo-version format: v{major}.{minor}.{patch} on tags,
# or v0.0.0-YYYYMMDDHHMMSS-commithash on untagged commits.
GIT_TAG := $(shell git tag --points-at HEAD 2>/dev/null | head -1)
GIT_COMMIT := $(shell git rev-parse --short=12 HEAD 2>/dev/null)
GIT_DATE := $(shell git log -1 --format=%cd --date=format:%Y%m%d%H%M%S 2>/dev/null)

ifeq ($(GIT_TAG),)
  VERSION := v0.0.0-$(GIT_DATE)-$(GIT_COMMIT)
else
  VERSION := $(GIT_TAG)
endif

LDFLAGS := -ldflags="-X github.com/groot/homelab/cmd.Version=$(VERSION)"

.PHONY: build build-linux-amd64 build-linux-arm64 release install tidy test test-race lint lint-full ci catalog version

build:
	go build $(LDFLAGS) -o homelab .

build-linux-amd64:
	GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o "homelab_$(VERSION)_linux_amd64" .

build-linux-arm64:
	GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o "homelab_$(VERSION)_linux_arm64" .

release: build-linux-amd64 build-linux-arm64
	@echo "Release binaries:"
	@ls -lh homelab_$(VERSION)_linux_*

install:
	go install $(LDFLAGS) .

tidy:
	go mod tidy

test:
	go test ./...

test-race:
	go test -race ./...

lint:
	go vet ./...

lint-full:
	golangci-lint run

ci: lint lint-full test-race build

# Export the embedded service catalog to a root services/ directory for local browsing.
# The canonical copy is assets/services/ — edit there, then run `make catalog`.
catalog:
	@rm -rf services && cp -r assets/services services
	@echo "services/ updated from assets/services/"

version:
	@echo "$(VERSION)"
