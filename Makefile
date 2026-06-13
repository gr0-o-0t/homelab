SHELL := /bin/bash
.DEFAULT_GOAL := build

# Version from git tag or commit
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
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
