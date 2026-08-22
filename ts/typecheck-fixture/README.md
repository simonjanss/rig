# typecheck-fixture

`tsc --noEmit` over the `ts-client` generator's golden output, against the real
`@rig/client` and `@rig/electric`.

It is the check the Go tests cannot make. A golden test proves the generator
emits the bytes it emitted last time; it cannot notice that those bytes stop
compiling because a runtime signature changed underneath them. This can, and it
is how `send` returning `T | undefined` was caught the day it was introduced.

There is no source of its own here — `tsconfig.json` includes the generator's
golden directories and `examples/todo/client-ts` directly, so refreshing those
is the whole of updating this.

Two goldens, because they fail differently: `lifecycle` for the versioning and
streaming surface, `files` for the multipart one. That second one is here because
an upload's shape is what the compiler is the only thing that checks — a
JSON-only method for a multipart route compiles, calls the right route, and sends
nothing.
