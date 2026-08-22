.DEFAULT_GOAL := help

GO       ?= go

# One checkout's throwaway containers, kept out of every other checkout's.
#
# rig names a container after the project and publishes it on the port in
# rig.yaml, which is right for a project and wrong for two clones of rig on one
# machine: they agree on both, so whichever runs second adopts the other's
# database and migrates its own branch on top. What arrives is not a collision —
# it is `rig check` reporting tables this branch never introduced, or a migration
# numbered below the database version. This variable is what tells rig there is
# more than one of us; see internal/dockerdb/isolate.go. Exported, so every
# recipe below and everything they run inherits it.
export RIG_DB_ISOLATE := $(CURDIR)

# The modules rig is written in, and the modules that only exercise it. Lint,
# vulnerability scanning and the Docker suite run over the first group only:
# the examples are mostly generated output, and their Docker tests are already
# run by `make examples`, which brings each example's own database up first.
CORE_MODULES    := . ./runtime ./auth ./files ./notify ./observe ./migrate ./rigclient
EXAMPLE_MODULES := ./examples/todo ./examples/fantasyfootball ./examples/auth ./examples/auth_oauth ./examples/sdk

# The core modules less the root: the ones a generated application imports, so
# their godoc is the documentation for a Go surface somebody depends on rather
# than commentary on it. That is why `godoc-check` runs over these and not over
# `internal/`, `pkg/ir` or `pkg/gen`, which are nobody else's dependency.
PUBLIC_MODULES  := ./runtime ./auth ./files ./notify ./observe ./migrate ./rigclient
MODULES         := $(CORE_MODULES) $(EXAMPLE_MODULES)
EXAMPLES        := todo fantasyfootball auth auth_oauth

# Versions of the external checkers, pinned so a run means the same thing on
# every machine. The linter is installed into ./bin rather than GOPATH/bin so
# that checking out rig does not replace the golangci-lint another project uses.
GOLANGCI_VERSION   := v2.12.2
GOVULNCHECK_VERSION := v1.7.0
VACUUM_VERSION      := v0.30.0

# A module with no packages yet — auth, until M4 lands — is skipped rather than
# treated as a failure, so `make test` says something useful during the build-out.
each = @for m in $(MODULES); do \
	[ -n "$$(cd $$m && $(GO) list ./... 2>/dev/null)" ] || continue; \
	(cd $$m &&

# The same loop over the core modules.
eachcore = @for m in $(CORE_MODULES); do \
	[ -n "$$(cd $$m && $(GO) list ./... 2>/dev/null)" ] || continue; \
	(cd $$m &&

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## //' | awk -F': ' '{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

## check: every check CI runs, cheapest first
##        This is what the pre-push hook runs. The last two need Docker, and
##        `deps` rewrites go.mod/go.sum before comparing, so run it on a clean
##        tree or it will report your own work in progress back to you.
check: fmt-check vet godoc-check build test deps lint vulncheck test-docker examples openapi-lint

## hooks: install the repository's git hooks into this clone
##        Cloning does not bring hooks with it, so this is opt-in and has to be
##        run once per clone.
hooks:
	@git config core.hooksPath .githooks
	@echo "hooks installed; 'git push --no-verify' skips them"

## build: build the rig binary into ./bin
build:
	$(GO) build -o bin/rig ./cmd/rig

## test: run the fast suite (no Docker)
test:
	$(each) $(GO) test ./...) || exit 1; done

## test-docker: run the suite that needs a Postgres container
##              The examples are left to `make examples`, which starts the
##              database each of them expects before running the same tests.
test-docker:
	$(eachcore) $(GO) test -tags docker ./...) || exit 1; done

## examples: regenerate every example and fail on any drift
##           This is the strongest regression test in the repository: the
##           examples are real projects, so a generator change that breaks one
##           breaks it visibly rather than in a golden file nobody reads.
##           Each example's tests are handed $DATABASE_URL rather than left to
##           the fallback compiled into them: under RIG_DB_ISOLATE the port is
##           the kernel's to choose, so `rig db url` is the only thing that
##           knows it.
examples: build
	@for e in $(EXAMPLES); do \
		(cd examples/$$e && ../../bin/rig generate && ../../bin/rig check) || exit 1; \
		(cd examples/$$e && DATABASE_URL=$$(../../bin/rig db url) \
			$(GO) build ./... && DATABASE_URL=$$(../../bin/rig db url) \
			$(GO) test -tags docker ./...) || exit 1; \
	done

## db-down: stop every example's database
##          Isolation means a container per example per checkout, so a machine
##          with a dozen workspaces open accumulates them. Stopping is enough:
##          the data is on a tmpfs, and the next command rebuilds it from the
##          migrations.
db-down: build
	@for e in $(EXAMPLES); do \
		(cd examples/$$e && ../../bin/rig db down) || true; \
	done

## update-schema: rewrite the introspection golden from a real Postgres
update-schema:
	$(GO) test -tags docker ./internal/introspect/ -update

## update-golden: rewrite golden files from current behavior
##                Only the packages that define -update: passing it to a test
##                binary without the flag is a usage error, which used to fail
##                this target no matter what.
GOLDEN := ./internal/compile ./internal/gen/electricgo ./internal/gen/goclient \
          ./internal/gen/modelgo ./internal/gen/openapigen ./internal/gen/persistgo \
          ./internal/gen/servergo ./internal/gen/servicego
update-golden:
	$(GO) test $(GOLDEN) -update

## vet: go vet every module
vet:
	$(each) $(GO) vet ./...) || exit 1; done

## godoc-check: fail on an exported symbol with no doc comment
##              Only the modules somebody else imports: their godoc is the
##              documentation for their Go surface, and there is no other.
##              Presence, not form — what a comment says is a reviewer's
##              question, which is why ST1000 and ST1020-ST1022 stay off.
godoc-check:
	@$(GO) run ./internal/godoccheck $(PUBLIC_MODULES)

## fmt: gofmt every module
fmt:
	$(each) $(GO) fmt ./...) || exit 1; done

## fmt-check: fail if anything is not gofmt-clean
##            One walk of the tree rather than one per module: gofmt reads
##            files, not packages, so the module boundaries do not matter.
fmt-check:
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then \
		echo "not gofmt-clean — run \`make fmt\`:"; \
		echo "$$out"; \
		exit 1; \
	fi

## tidy: go mod tidy every module
tidy:
	@for m in $(MODULES); do (cd $$m && $(GO) mod tidy) || exit 1; done

## deps: fail if any go.mod or go.sum is not what `go mod tidy` produces
##       Untracked files count: a module whose go.sum was never committed
##       builds only because the workspace is hiding the gap.
deps: tidy
	@out=$$(git status --porcelain -- '*go.mod' '*go.sum'); \
	if [ -n "$$out" ]; then \
		git diff -- '*go.mod' '*go.sum'; \
		echo "$$out"; \
		echo "dependencies are not tidy — run \`make tidy\` and commit the result"; \
		exit 1; \
	fi

## lint: run golangci-lint over the modules rig is written in
##       Each module is a separate run: with a workspace, `./...` names the
##       packages of one module and nothing else.
lint:
	@GOBIN=$(CURDIR)/bin $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)
	$(eachcore) $(CURDIR)/bin/golangci-lint run ./...) || exit 1; done

## openapi-lint: lint every emitted OpenAPI document against vacuum's rules
##               Two circular-reference flags, and both are the document being
##               right rather than the linter being wrong. A filter holds nested
##               filters, which is how AND and OR mix to any depth; a relation
##               filter reaches the related resource's filter, which is how you
##               ask about a row's parent. Both cycles run through optional
##               fields and terminate.
##
##               It reads what is on disk rather than depending on `examples`,
##               the way `lint` reads the source on disk. In `check` it is
##               ordered after `examples`, which is what rewrites the four
##               documents under examples/*/docs; run on its own against a stale
##               tree it lints a stale document, and `rig check` is what catches
##               that.
openapi-lint:
	@GOBIN=$(CURDIR)/bin $(GO) install github.com/daveshanley/vacuum@$(VACUUM_VERSION)
	@for f in internal/gen/openapigen/testdata/*/openapi.gen.yaml \
	          examples/*/docs/openapi.gen.yaml; do \
		[ -f "$$f" ] || continue; \
		$(CURDIR)/bin/vacuum lint --ruleset .vacuum.yaml --fail-severity warn \
			--ignore-array-circle-ref --ignore-polymorph-circle-ref \
			--no-banner --details "$$f" || exit 1; \
	done

## vulncheck: scan the modules rig is written in for known vulnerabilities
vulncheck:
	@GOBIN=$(CURDIR)/bin $(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	$(eachcore) $(CURDIR)/bin/govulncheck ./...) || exit 1; done

.PHONY: help check hooks build test test-docker examples db-down update-schema update-golden vet godoc-check fmt fmt-check tidy deps lint vulncheck openapi-lint
