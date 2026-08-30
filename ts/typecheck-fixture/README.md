# typecheck-fixture

`tsc --noEmit` over the `ts-client` generator's golden output, against the real
`@rig-ts/client` and `@rig-ts/electric`.

It is the check the Go tests cannot make. A golden test proves the generator
emits the bytes it emitted last time; it cannot notice that those bytes stop
compiling because a runtime signature changed underneath them. This can, and it
is how `send` returning `T | undefined` was caught the day it was introduced.

`tsconfig.json` includes the generator's golden directories,
`examples/todo/client-ts` and `examples/linearlite/web/src/api` directly rather
than copies of them, so refreshing those is most of updating this.

It also turns on `noUnusedParameters`, which `tsconfig.base.json` does not. That
is a claim about the generated output: rig scaffolds no front end, so the
tsconfig this output lands under is always the project's, and the templates one
starts from enable it. An unused binding there is a TS6133 in a file whose
banner says not to edit it — the one kind of error a project cannot fix where it
lands.

The flag reaches the hand-written packages as well, because the `paths` mapping
puts their sources in the same program. That is how this fixture is built rather
than a rule anybody chose for them, and one that turns up is answered the way
the generator answers one: underscore the binding.

`src/` is the rest: the two claims that no generated file makes on its own.
`presence.ts` is there because nothing generated imports `@rig-ts/presence`, so a
project's own `tsc` would never compare the package against a generated row.
`fake-client.ts` is there because the generated output compiling says nothing
about whether anybody _outside_ it can write a stand-in for a resource — which
is what a test does, and what a class with a private field made impossible.

Three goldens, because they fail differently: `lifecycle` for the versioning and
streaming surface, `files` for the multipart one, `notify` for the tables a rig
module brings with it. The second is here because an upload's shape is what the
compiler is the only thing that checks — a JSON-only method for a multipart route
compiles, calls the right route, and sends nothing. The third is here because
those tables stream without declaring params, so their factories bind a params
object the body never reads, and `lifecycle` declares params so every factory in
it reads one.
