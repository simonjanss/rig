# The one-command path in, and the pieces it is made of. Everything here is
# idempotent — `make demo` twice reuses the containers, re-seeds nothing, and
# rebuilds only what changed — so this is also the "start my day" command.

# The repository's own build of the CLI, the same one every check uses. Built
# on demand, so a fresh clone needs nothing installed but Go and Docker.
RIG := ../../bin/rig

# What the demonstration needs at run time and the build cannot know.
#
# The span file and the log file are what rig's monitoring page reads — it is a
# reader and stores nothing of its own, so with neither of these set the page
# has nothing to show. The password is what mounts it at all: with the variable
# empty the route does not exist, which is the right default on a laptop and the
# reason it has to be said here rather than in rig.yaml, which is checked in.
#
# .run/ is throwaway and gitignored, like the database.
RUN_DIR ?= .run
# Twelve characters is rig's floor, and it refuses a shorter one at startup
# rather than serving the page behind something guessable. Overridden by
# anything already in the environment.
DEMO_MONITOR_PASSWORD := watch-this-demo
DEMO_ENV := \
	RIG_LOG_FILE=$(RUN_DIR)/linearlite.log \
	RIG_TRACE_FILE=$(RUN_DIR)/linearlite.spans \
	RIG_MONITOR_PASSWORD=$${RIG_MONITOR_PASSWORD:-$(DEMO_MONITOR_PASSWORD)}

.PHONY: demo up seed front run down

## demo: database + sync service, seed data, front end, then the server
demo: up seed front
	@mkdir -p $(RUN_DIR)
	@echo
	@echo "open http://localhost:8084 — demo@linearlite.dev / \"correct horse battery staple\""
	@echo "traces and logs at http://localhost:8084/_rig/monitor — password \"$${RIG_MONITOR_PASSWORD:-$(DEMO_MONITOR_PASSWORD)}\""
	@echo
	@DATABASE_URL=$$($(RIG) db url) $(DEMO_ENV) go run .

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
	@mkdir -p $(RUN_DIR)
	@DATABASE_URL=$$($(RIG) db url) $(DEMO_ENV) go run .

## down: stop this example's containers (the data is throwaway)
down:
	@$(RIG) db down
	@rm -rf $(RUN_DIR)
