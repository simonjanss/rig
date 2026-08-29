// Package notifymodel is the Go over rig's own notification tables: the rows,
// the write inputs, and the enums their columns are.
//
// It is here rather than generated into each application because the schema is
// rig's. The five tables come from notify/foundation, a project cannot change
// them, and the Go over them was the same five thousand lines copied into every
// repository that turned notifications on. What a project still generates for
// them is what its own schema reaches into: the filter grammar, which carries a
// member per table of its own that points at rig_notification, and the
// repository that renders those subqueries.
//
// An application does not usually import this package. `model.Notification` in
// a generated project is an alias for [Notification], so a service signature, a
// hook and a repository are all talking about this struct without naming it.
//
// **The JSON keys are camelCase and do not follow `naming.json_case`.** Struct tags
// are fixed at compile time and this package is compiled once, so a project that
// asked for snake_case gets it on its own tables and camelCase on rig's. That is
// the trade rig already made for its hand-written routes — the authentication
// endpoints have answered camelCase in a snake_case project since they existed —
// and `rig check` says so rather than leaving it to be discovered.
package notifymodel
