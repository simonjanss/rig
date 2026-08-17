.DEFAULT_GOAL := help

GO       ?= go
MODULES  := . ./runtime ./auth ./migrate ./rigclient ./examples/todo ./examples/fantasyfootball ./examples/auth ./examples/auth_oauth ./examples/sdk
EXAMPLES := todo fantasyfootball auth auth_oauth

# A module with no packages yet — auth, until M4 lands — is skipped rather than
# treated as a failure, so `make test` says something useful during the build-out.
each = @for m in $(MODULES); do \
	[ -n "$$(cd $$m && $(GO) list ./... 2>/dev/null)" ] || continue; \
	(cd $$m &&

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## //' | awk -F': ' '{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

## build: build the rig binary into ./bin
build:
	$(GO) build -o bin/rig ./cmd/rig

## test: run the fast suite (no Docker)
test:
	$(each) $(GO) test ./...) || exit 1; done

## test-docker: run the suite that needs a Postgres container
test-docker:
	$(each) $(GO) test -tags docker ./...) || exit 1; done

## examples: regenerate every example and fail on any drift
##           This is the strongest regression test in the repository: the
##           examples are real projects, so a generator change that breaks one
##           breaks it visibly rather than in a golden file nobody reads.
examples: build
	@for e in $(EXAMPLES); do \
		(cd examples/$$e && ../../bin/rig generate && ../../bin/rig check) || exit 1; \
		(cd examples/$$e && $(GO) build ./... && $(GO) test -tags docker ./...) || exit 1; \
	done

## update-schema: rewrite the introspection golden from a real Postgres
update-schema:
	$(GO) test -tags docker ./internal/introspect/ -update

## update-golden: rewrite golden files from current behavior
##                Only the packages that define -update: passing it to a test
##                binary without the flag is a usage error, which used to fail
##                this target no matter what.
GOLDEN := ./internal/compile ./internal/gen/electricgo ./internal/gen/modelgo \
          ./internal/gen/persistgo ./internal/gen/servergo ./internal/gen/servicego
update-golden:
	$(GO) test $(GOLDEN) -update

## vet: go vet every module
vet:
	$(each) $(GO) vet ./...) || exit 1; done

## fmt: gofmt every module
fmt:
	$(each) $(GO) fmt ./...) || exit 1; done

## tidy: go mod tidy every module
tidy:
	@for m in $(MODULES); do (cd $$m && $(GO) mod tidy) || exit 1; done

.PHONY: help build test test-docker examples update-schema update-golden vet fmt tidy
