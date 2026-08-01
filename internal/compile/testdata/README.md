# Compiler fixtures

Each directory is one case:

```
<case>/
  schema.json       an ir.Schema, exactly as introspection would hand it over
  rig.yaml          the project configuration (optional; a default is used)
  tables/*.yaml     table configuration (optional)
  ir.golden.json    the expected document
  diags.golden.txt  the expected diagnostics (omitted when there are none)
```

The compiler takes its schema by value, so none of this needs Docker or a
database. That is deliberate: it is what keeps the bulk of rig's test suite
under a second, and it means a compiler change can be iterated on without a
container in the loop.

`schema.json` files are not written by hand for real schemas — `rig ir
--dump-schema` mints them from a live database, which is what keeps these
fixtures honest about what Postgres actually reports.

Regenerate the golden files after an intentional change:

```bash
go test ./internal/compile/ -update
```

Read the resulting diff carefully. It is the review surface for every change to
the compiler.
