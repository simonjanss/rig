# typecheck-fixture

`tsc --noEmit` over the `ts-client` generator's golden output, against the real
`@rig-ts/client` and `@rig-ts/electric`.

It is the check the Go tests cannot make. A golden test proves the generator
emits the bytes it emitted last time; it cannot notice that those bytes stop
compiling because a runtime signature changed underneath them. This can, and it
is how `send` returning `T | undefined` was caught the day it was introduced.

`tsconfig.json` includes the generator's golden directories and
`examples/todo/client-ts` directly rather than copies of them, so refreshing
those is most of updating this.

`src/` is the rest: the two claims that no generated file makes on its own.
`presence.ts` is there because nothing generated imports `@rig-ts/presence`, so a
project's own `tsc` would never compare the package against a generated row.
`fake-client.ts` is there because the generated output compiling says nothing
about whether anybody _outside_ it can write a stand-in for a resource — which
is what a test does, and what a class with a private field made impossible.

Two goldens, because they fail differently: `lifecycle` for the versioning and
streaming surface, `files` for the multipart one. That second one is here because
an upload's shape is what the compiler is the only thing that checks — a
JSON-only method for a multipart route compiles, calls the right route, and sends
nothing.
