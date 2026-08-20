package compile

import (
	"fmt"
	"slices"
	"strings"

	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/internal/project"
	"github.com/simonjanss/rig/internal/scaffold"
	"github.com/simonjanss/rig/internal/tableconf"
	"github.com/simonjanss/rig/pkg/ir"
)

// checkReserved refuses a table that has taken a name rig keeps for itself.
//
// Two rules, and the second is the one with teeth. The `rig_` prefix is rig's,
// so a project cannot create a table under it. And the API names rig's own
// tables project to — Account, Tenant, File and the rest — are rig's whether or
// not this project exposes them: a project that called its own table `account`
// would otherwise discover the collision on the day it set `auth.expose`, and
// the fix then is a migration plus a rename in every client that reads the
// resource. Reserving them now makes that day one line in rig.yaml.
//
// This runs after [Freeze] rather than beside the collision check in [Project],
// because the frozen document holds the name that actually won: [Project] sets
// it from the table name and [ApplyConfig] overwrites it from `resource:`. A
// check any earlier would see one spelling and miss the other.
func checkReserved(doc *ir.Document, set *tableconf.Set, p *project.Project, foundation []string) diag.List {
	var diags diag.List

	// `auth.own` takes the schema over, tables and names alike. It is already a
	// one-way door and this is part of what is behind it: a project that has
	// forked the foundation owns rig_account, and Account with it.
	if p.Config.Auth.Own {
		return diags
	}

	reserved := scaffold.ReservedResources()

	for i := range doc.Schema.Tables {
		t := &doc.Schema.Tables[i]
		loaded := set.Get(t.Name)

		if d, bad := checkReservedTable(t, loaded, p, foundation); bad {
			// One diagnostic per table. A rig_account nobody scaffolded would
			// otherwise be reported twice, and renaming it answers both.
			diags.Append(d)
			continue
		}
		diags.Append(checkReservedResource(doc, t, loaded, foundation, reserved))
	}

	return diags
}

// Reserved reports why rig will not let a project have a table by this name, or
// "" when nothing stands in the way. `escapable` is whether a `resource:` key
// still answers it.
//
// It asks the two questions [checkReserved] asks, from a name alone: the
// commands that write a file — `rig migration new --table`, `rig sync` — call it
// before they write, so the answer arrives before the DDL exists rather than
// from the next `rig validate`. The compile stage deliberately does not call it,
// because by then the resource name has been through configuration and the
// frozen document is the better source.
//
// The two halves of the rule are not equally final, and a caller that could not
// tell them apart would have to refuse both. The `rig_` prefix is rig's and
// nothing moves a table off it. A reserved resource name is only what the table
// projects to *by default*, so a `resource:` of its own answers it — which is
// what [diag.CodeReservedResource]'s hint offers, and a caller here must not
// turn that into a refusal: a table rig's own rules allow would then be one rig
// cannot scaffold.
func Reserved(p *project.Project, foundation []string, table string) (why string, escapable bool) {
	if p.Config.Auth.Own {
		return "", false
	}

	if d, bad := checkReservedTable(&ir.Table{Name: table}, nil, p, foundation); bad {
		return d.All()[0].Message, false
	}

	// The name a table projects to before any configuration touches it.
	resource := p.Namer().Go(table)
	if owner, taken := scaffold.ReservedResources()[resource]; taken && owner != table {
		return fmt.Sprintf("a table named %q projects to the resource name %q, which rig's own %s takes",
			table, resource, owner), true
	}
	return "", false
}

// Bookkeeping are the tables no project ever gets a resource for: the migration
// histories.
//
// The project's own is named in rig.yaml. The rest belong to the foundation's
// sets, one per module that can carry its own schema, and they exist only in a
// project whose `migrations.foundation` is `embedded` — but they are listed in
// every mode, because a table that used to be there after somebody switched off
// embedded is still not an API.
//
// Introspection returns all of them like any other table, so both the ignore list
// and the reserved-prefix rule have to know the names. A resource over a migration
// history would be a generated PATCH on `is_applied`.
func Bookkeeping(p *project.Project) []string {
	return append([]string{p.Config.Migrations.Table}, scaffold.BookkeepingTables()...)
}

// checkReservedTable reports a table under the `rig_` prefix that rig did not
// create.
//
// Which tables rig created is read from the project's own evidence, not from the
// names — see [scaffold.Managed] and [scaffold.Wanted.Parts]. The bookkeeping
// tables are exempt because they are rig's own and arrive through a different
// door: goose writes them, no migration creates them, so a project that renamed
// `migrations.table` and left the old one behind should not be told its migration
// history is illegal. That is now four tables rather than one — the project's, and
// one per module that can carry its own schema.
func checkReservedTable(
	t *ir.Table,
	loaded *tableconf.Loaded,
	p *project.Project,
	foundation []string,
) (diag.List, bool) {
	var diags diag.List

	if !strings.HasPrefix(t.Name, scaffold.TablePrefix) {
		return diags, false
	}
	if slices.Contains(foundation, t.Name) {
		return diags, false
	}
	if t.Name == p.Config.Migrations.Table || t.Name == project.DefaultMigrationsTable ||
		slices.Contains(scaffold.BookkeepingTables(), t.Name) {
		return diags, false
	}

	// The message differs because the fix does. A name rig uses is most often
	// somebody who wrote the foundation's DDL by hand, or squashed the migration
	// that created it: the table is rig's, and what is missing is only the
	// filename that says so.
	//
	// Which is why the advice is that filename and not `rig setup-project`. That
	// command reads the same filenames to decide what to write — see
	// [scaffold.Foundation] — so it would answer a table that already exists with
	// a second CREATE TABLE for it, and the next `rig db up` would fail.
	if part := scaffold.PartOf(t.Name); part != "" {
		diags.Add(diag.CodeReservedTablePrefix, at(loaded, t),
			"table %q is one rig's own foundation creates, and no migration here created it; "+
				"rename the table, or — if it is rig's — rename the migration that creates it to end "+
				"in `_rig_%s.sql`, which is how rig recognises the %s part",
			t.Name, part, part)
		return diags, true
	}

	diags.Add(diag.CodeReservedTablePrefix, at(loaded, t),
		"table %q uses the %s prefix, which rig keeps for the tables it creates",
		t.Name, scaffold.TablePrefix)
	return diags, true
}

// checkReservedResource reports a table projecting to a name rig's own tables
// take.
//
// A link table has no resource and is skipped: it was already checked for the
// prefix, which is the rule that applies to it.
func checkReservedResource(
	doc *ir.Document,
	t *ir.Table,
	loaded *tableconf.Loaded,
	foundation []string,
	reserved map[string]string,
) diag.List {
	var diags diag.List

	res := doc.ResourceForTable(t.Name)
	if res == nil {
		return diags
	}

	owner, taken := reserved[res.Name]
	if !taken || owner == t.Name {
		return diags
	}

	// Only the name. Plural and PathSegment follow from it, and reserving those
	// too would refuse a project's `accounts` table for no further benefit.
	//
	// The anchor is the `resource:` key when one was written, because that is
	// where the reader would go; without it the table's own name is the fix and
	// the message has to carry it.
	anchor := at(loaded, t)
	if loaded != nil && loaded.File.Resource != "" {
		anchor = at(loaded, t, "resource")
	}

	what := "table %q projects to the resource name %q, which rig's own %s takes"
	if slices.Contains(foundation, owner) {
		what += "; rename it, or give it a `resource:` of its own"
	} else {
		what += " — reserved whether or not this project exposes that table, " +
			"so that exposing it later is never a rename"
	}

	diags.Add(diag.CodeReservedResource, anchor, what, t.Name, res.Name, owner)
	return diags
}
