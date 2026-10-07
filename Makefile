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

.PHONY: build build-headless build-linux-amd64 build-linux-amd64-headless build-linux-arm64 release install tidy test test-race lint lint-full ci catalog version

# The default build includes the desktop GUI (`homelab --gui`), which needs
# cgo plus the OpenGL and X11 development headers
# (Debian/Ubuntu: libgl1-mesa-dev xorg-dev). The result links libGL/libX11 and
# will not start on a host without them — use build-headless for servers.
build:
	CGO_ENABLED=1 go build $(LDFLAGS) -o homelab .

# Pure Go, no GUI, no graphics libraries: for servers and cross-compiling.
build-headless:
	CGO_ENABLED=0 go build -tags nogui $(LDFLAGS) -o homelab .

build-linux-amd64:
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o "homelab_$(VERSION)_linux_amd64" .

build-linux-amd64-headless:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags nogui $(LDFLAGS) -o "homelab_$(VERSION)_linux_amd64_headless" .

# ARM64 is built headless: cross-compiling the cgo GUI needs an ARM sysroot,
# and ARM64 hosts here are servers and Raspberry Pis.
build-linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -tags nogui $(LDFLAGS) -o "homelab_$(VERSION)_linux_arm64" .

release: build-linux-amd64 build-linux-amd64-headless build-linux-arm64
	@echo "Release binaries:"
	@ls -lh homelab_$(VERSION)_linux_*

install:
	CGO_ENABLED=1 go install $(LDFLAGS) .

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

# The headless build is part of CI so the nogui stub keeps compiling.
ci: lint lint-full test-race build-headless build

# Export the embedded service catalog to a root services/ directory for local browsing.
# The canonical copy is assets/services/ — edit there, then run `make catalog`.
catalog:
	@rm -rf services && cp -r assets/services services
	@echo "services/ updated from assets/services/"

version:
	@echo "$(VERSION)"
