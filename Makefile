SHELL := /bin/bash
.DEFAULT_GOAL := build

.PHONY: build install tidy test test-race lint lint-full ci catalog

build:
	go build -o homelab .

install:
	go install .

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
