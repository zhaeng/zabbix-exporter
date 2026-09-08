SHELL := /bin/bash

PROJECT_NAME := zabbix-exporter
MODULE := github.com/zhaeng/zabbix-exporter
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS := -s -w \
	-X '$(MODULE)/cmd/server/initial.Version=$(VERSION)' \
	-X '$(MODULE)/cmd/server/initial.GitCommit=$(GIT_COMMIT)' \
	-X '$(MODULE)/cmd/server/initial.BuildTime=$(BUILD_TIME)'

.PHONY: fmt
fmt:
	gofmt -w .

.PHONY: fmt-check
fmt-check:
	@test -z "$$(gofmt -l .)"

.PHONY: tidy-check
tidy-check:
	go mod tidy -diff

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
test:
	go test -count=1 ./...

.PHONY: test-race
test-race:
	go test -race -count=1 ./...

.PHONY: build
build:
	mkdir -p _output
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o _output/$(PROJECT_NAME) ./cmd/server

.PHONY: docker-build
docker-build:
	docker build \
		--build-arg VERSION="$(VERSION)" \
		--build-arg GIT_COMMIT="$(GIT_COMMIT)" \
		--build-arg BUILD_TIME="$(BUILD_TIME)" \
		-t $(PROJECT_NAME):local .

.PHONY: check
check: fmt-check tidy-check vet test test-race build

.PHONY: clean
clean:
	rm -rf _output cover.out

.PHONY: help
help:
	@echo "Usage: make <target>"
	@echo "Targets: fmt fmt-check tidy-check vet test test-race build docker-build check clean"

.DEFAULT_GOAL := help
