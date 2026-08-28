// Package authmodel is the Go over rig_account: the row a tenant's membership
// is, its write inputs, and the two enums its columns are.
//
// It is a module of its own, and that is the whole point of it. The table
// belongs to rig's authentication schema and the obvious home would be
// [github.com/simonjanss/rig/auth] — but `auth.expose: [rig_account]` does not
// require `auth.enabled`. A project may read account rows without using rig's
// authentication at all, and for that project a package under the auth module
// would mean argon2, OAuth and x/crypto in its go.mod for the sake of a struct.
// So the schema's Go and the machinery over it are separate things to depend on.
//
// Nothing here does anything. There are no queries, no transport and no
// policy — those are auth's, and a project that wants them imports that module
// too, alongside this one rather than instead of it.
//
// Only the row and its inputs. The filter grammar stays with the project,
// because a table of its own that points at an account — an assignee, an
// author, a reviewer — puts a member on AccountFilter, and that makes the type
// the project's even though the row is not.
//
// An application does not usually name this package: `model.Account` in a
// generated project is an alias for [Account].
//
// **The JSON keys are camelCase and do not follow `naming.json_case`.** Struct
// tags are fixed at compile time and this package is compiled once, so a project
// that asked for snake_case gets it on its own tables and camelCase here. That
// is the trade rig's hand-written routes have always made, and RIG3260 reports
// it rather than leaving it to be found in a response body.
package authmodel
