package scaffold

import "maps"

// TablePrefix is on every table rig creates, and rig keeps it.
//
// A project's own `rig_…` table would be indistinguishable in psql from one
// `setup-project` wrote, which is the whole reason the prefix exists — and the
// next part rig adds could create a table by that name outright, turning
// somebody's schema into a migration that will not apply. Reserving the prefix
// costs a project one naming choice and buys rig the freedom to grow the
// foundation without breaking anybody.
const TablePrefix = "rig_"

// ReservedResources maps every API name rig's own tables project to, to the
// table that takes it: Account to rig_account, File to rig_file, and so on.
//
// They are reserved whether or not a project exposes rig's tables. The name a
// project cannot have is the price of `auth.expose` staying one line in
// rig.yaml: without the reservation, a project that called its own table
// `account` would find out on the day it exposed rig_account, and the fix then
// is a migration plus a rename in every client that reads the resource.
//
// Derived from the table configurations `setup-project` writes rather than
// listed, so a part added to the foundation reserves its names by existing. The
// planned entries are the one exception, and they say so.
func ReservedResources() map[string]string {
	out := make(map[string]string, len(plannedResources))
	for _, name := range Parts() {
		for _, c := range foundationPart(name).configs {
			out[c.resource] = c.table
		}
	}
	maps.Copy(out, plannedResources)
	return out
}

// TableConfig is rig's own configuration for one of its own tables, as YAML.
//
// It is the same text `rig setup-project --expose` writes to disk, returned as
// bytes so the compiler can read it without a file. That is the whole point: how
// rig's tables project — the API name the `rig_` prefix strips to, which
// operations exist, which columns are read-only, what each enum value means — is
// rig's answer and not a thing every project should have to write out. A project
// that wants to differ runs `--expose` and edits the file it gets, and a file on
// disk wins over this outright.
//
// ok is false for a table rig does not own, and for one it owns and has nothing
// to say about.
func TableConfig(table string) (yaml []byte, ok bool) {
	for _, name := range Parts() {
		for _, c := range foundationPart(name).configs {
			if c.table == table {
				return []byte(c.content), true
			}
		}
	}
	return nil, false
}

// plannedResources are names rig has decided on and not built yet.
//
// Empty, and that is the healthy state: everything rig has decided on exists,
// so [ReservedResources] derives the whole set and there is nothing here to
// drift from it. `Notification` and the four names beside it were entries here
// until the notification part shipped, at which point their configurations
// started reserving them and these lines became a second copy of the answer.
// [TestPlannedNamesAreNotBuiltYet] is what makes leaving one behind a failure.
//
// Reserving a name early is the cheaper mistake: a project that named a table
// `notification` before the part landed would have paid a migration and a
// rename in every client that read the resource. Reserving one that never ships
// costs that project a second choice of name.
//
// So what earns a place here is a schema that is settled — decided, written
// down, and not yet in `PartTables`. A name held for a table whose shape is
// still an open question is a cost every project pays for rig's benefit.
var plannedResources = map[string]string{}
