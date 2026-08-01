.DEFAULT_GOAL := help

GO      ?= go
MODULES := . ./runtime ./auth

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## //' | awk -F': ' '{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

## build: build the rig binary into ./bin
build:
	$(GO) build -o bin/rig ./cmd/rig

## test: run the fast suite (no Docker)
test:
	$(GO) test ./...
	cd runtime && $(GO) test ./...
	cd auth && $(GO) test ./...

## test-docker: run the suite that needs a Postgres container
test-docker:
	$(GO) test -tags docker ./...

## update-golden: rewrite golden files from current behavior
update-golden:
	$(GO) test ./internal/compile/... ./internal/gen/... -update

## vet: go vet every module
vet:
	@for m in $(MODULES); do (cd $$m && $(GO) vet ./...) || exit 1; done

## fmt: gofmt every module
fmt:
	$(GO) fmt ./...
	cd runtime && $(GO) fmt ./...
	cd auth && $(GO) fmt ./...

## tidy: go mod tidy every module
tidy:
	@for m in $(MODULES); do (cd $$m && $(GO) mod tidy) || exit 1; done

.PHONY: help build test test-docker update-golden vet fmt tidy
