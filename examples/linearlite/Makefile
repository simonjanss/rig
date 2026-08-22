# The one-command path in, and the pieces it is made of. Everything here is
# idempotent — `make demo` twice reuses the containers, re-seeds nothing, and
# rebuilds only what changed — so this is also the "start my day" command.

# The repository's own build of the CLI, the same one every check uses. Built
# on demand, so a fresh clone needs nothing installed but Go and Docker.
RIG := ../../bin/rig

.PHONY: demo up seed front run down

## demo: database + sync service, seed data, front end, then the server
demo: up seed front
	@echo
	@echo "open http://localhost:8084 — demo@linearlite.dev / \"correct horse battery staple\""
	@echo
	@DATABASE_URL=$$($(RIG) db url) go run .

## up: start Postgres and the ElectricSQL sync service, apply migrations
up:
	@$(MAKE) --no-print-directory -C ../.. build
	@$(RIG) db up

## seed: the demo tenant, two people, the roles, and a board's worth of items
seed: up
	@DATABASE_URL=$$($(RIG) db url) go run . seed

## front: build the React app the server serves from web/dist
##        Skipped with a note when pnpm is not installed: the server runs
##        without it and answers with build instructions instead of a board.
front:
	@if command -v pnpm >/dev/null 2>&1; then \
		cd web && pnpm install --frozen-lockfile && pnpm build; \
	else \
		echo "pnpm not found — skipping the front end build; the server will say how"; \
	fi

## run: just the server, when everything above is already in place
run:
	@DATABASE_URL=$$($(RIG) db url) go run .

## down: stop this example's containers (the data is throwaway)
down:
	@$(RIG) db down
