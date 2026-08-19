#!/bin/sh
# Print the documentation page that goes with the file just edited.
#
# The mapping is the table in AGENTS.md under "## Documentation", and the two
# have to be changed together. It exists because "remember to update the docs"
# is not a rule anybody follows on a busy afternoon, and "which page, though" is
# the question that stops them when they try.
#
# Wired up as a PostToolUse hook in .claude/settings.json. It runs after the
# edit, never before, and it cannot refuse one: a reminder that blocks work is a
# reminder somebody turns off. Gating documentation in `make check` would fail
# every work-in-progress branch, which is the same mistake with a longer fuse.
#
# Silent for anything unmapped, which is the great majority of edits.
set -eu

# The hook payload arrives on stdin as JSON. jq if it is here, sed if it is not:
# this repository assumes Go and a container engine, and nothing else.
payload=$(cat)

if command -v jq >/dev/null 2>&1; then
	path=$(printf '%s' "$payload" | jq -r '.tool_input.file_path // empty')
else
	# One key, one string value, no escapes worth worrying about in a file path.
	path=$(printf '%s' "$payload" |
		sed -n 's/.*"file_path"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' |
		head -n 1)
fi

[ -n "$path" ] || exit 0

# Relative to the repository root, so the patterns below do not have to survive
# whatever absolute prefix a checkout happens to have.
root=$(git rev-parse --show-toplevel 2>/dev/null || true)
case "$root" in
"") rel=$path ;;
*) rel=${path#"$root"/} ;;
esac

# First match wins, so the specific cases come before the directory ones.
case "$rel" in
internal/project/config.go) pages="docs/rig-yaml.md" ;;
internal/project/auth.go) pages="docs/auth.md" ;;
internal/tableconf/config.go) pages="docs/tables.md" ;;
internal/compile/convention.go) pages="docs/schema.md" ;;
internal/compile/builtin.go) pages="docs/api.md" ;;
internal/gen/electricgo/*) pages="docs/electric.md docs/generators.md" ;;
internal/gen/goclient/*) pages="docs/clients.md docs/generators.md" ;;
internal/gen/*) pages="docs/generators.md README.md" ;;
internal/cli/*) pages="docs/cli.md" ;;
internal/diag/*) pages="docs/diagnostics.md" ;;
runtime/electric/*) pages="docs/electric.md" ;;
runtime/serve/* | runtime/dbhook/*) pages="docs/services.md" ;;
auth/*) pages="docs/auth.md" ;;
rigclient/*) pages="docs/clients.md" ;;
*) exit 0 ;;
esac

# Said as a question rather than an instruction. Plenty of edits under these
# paths change nothing a user can see, and the answer is often "no".
printf 'Docs: %s changed. Does %s still describe it? (AGENTS.md § Documentation)\n' \
	"$rel" "$pages" >&2

# Exit 2 is what puts stderr in front of the agent rather than in a log nobody
# reads. It does not undo the edit.
exit 2
