package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/simonjanss/rig/internal/project"
	"github.com/simonjanss/rig/internal/scaffold"
	"github.com/simonjanss/rig/pkg/ir"
)

// publishShapes writes the migration that makes this project's streaming tables
// streamable, and says what it left alone.
//
// The one thing rig knew and would not write down. It has always been able to say
// that a table streams and is not published — that is RIG5090, and RIG5093 is the
// replica identity beside it — and the remedy was a paragraph telling somebody to
// write SQL rig could have written, over a table list rig had compiled and they
// had to reassemble by reading a dozen configuration files.
//
// Diagnostics are held back, for `rig sync`'s reason: this command exists to fix
// the very thing they report, and refusing to run until the project validates
// would make the fix reachable only from a project that does not need it.
//
// It reads the database, and that is what makes the second run different from the
// first. Introspection already carries which publications hold each table and
// (since RIG5093) each table's replica identity, so what goes in the file is the
// difference between what streams and what Postgres is already arranged to
// replicate. A table that gains a shape six months later gets a two-line
// migration rather than a rewrite of the one that is already applied.
func (e *env) publishShapes(ctx context.Context, p *project.Project, name string) error {
	doc, _, err := e.compile(ctx, p, "")
	if err != nil {
		return err
	}

	streaming := streamingTables(doc)
	if len(streaming) == 0 {
		return errors.New("no table in this project has a live-sync shape, so there is nothing to publish; " +
			"add `electric: {enabled: true}` to a table's configuration first")
	}

	plan, err := publicationPlan(doc, streaming)
	if err != nil {
		return err
	}
	if len(plan.Add) == 0 && len(plan.Identity) == 0 {
		fmt.Fprintf(e.errOut, "every streaming table is already published with REPLICA IDENTITY FULL; nothing to write\n")
		return nil
	}

	dir := p.MigrationsDir()
	if err := mkdirAll(dir); err != nil {
		return err
	}
	number, err := scaffold.NextMigrationNumber(dir)
	if err != nil {
		return err
	}
	path := scaffold.MigrationFilename(dir, number, name)
	if fileExists(path) {
		return fmt.Errorf("%s already exists", p.Rel(path))
	}

	if err := os.WriteFile(path, []byte(scaffold.Publish(plan)), 0o644); err != nil {
		return err
	}

	fmt.Fprintf(e.errOut, "created %s\n", p.Rel(path))
	if len(plan.Already) > 0 {
		fmt.Fprintf(e.errOut, "  already published: %s\n", strings.Join(plan.Already, ", "))
	}
	if len(plan.Add) > 0 {
		fmt.Fprintf(e.errOut, "  publishing:        %s\n", strings.Join(plan.Add, ", "))
	}
	if len(plan.Identity) > 0 {
		fmt.Fprintf(e.errOut, "  identity to FULL:  %s\n", strings.Join(plan.Identity, ", "))
	}
	fmt.Fprintf(e.errOut, "\nApply it with `rig db up`.\n")
	return nil
}

// streamingTables are the tables this project streams, in schema order.
//
// The same predicate the generated shape routes use — a resource with an electric
// endpoint and a table behind it — so the migration and the routes cannot
// disagree about which tables need publishing. It is also why rig's own tables
// appear without a project having asked: an inbox line and a presence row stream
// because `notifications:` and `presence:` are on.
func streamingTables(doc *ir.Document) []string {
	var out []string
	for i := range doc.API.Resources {
		res := &doc.API.Resources[i]
		if res.Electric == nil || res.Storage == nil {
			continue
		}
		if !slices.Contains(out, res.Storage.Table) {
			out = append(out, res.Storage.Table)
		}
	}
	slices.Sort(out)
	return out
}

// publicationPlan works out what is missing.
//
// The publication is not a choice. The sync service reads only its own —
// [scaffold.DefaultPublication] — so that is what the migration has to create and
// fill, and a publication under any other name is not an alternative but a no-op.
//
// Whether this migration creates it is the ordering that makes the whole thing
// work. `rig db up` migrates and then starts the sync service, so on a fresh
// database the migration gets there first and owns what it made; the service then
// finds its publication already correct. That ownership is also what RIG5090 reads
// to tell a publication a migration wrote from one the service made for itself.
//
// Which leaves one state this cannot repair, and it says so rather than writing SQL
// that will fail: a database where the sync service created the publication before
// any migration did. Nothing can transfer ownership from here, and the migration's
// ALTER PUBLICATION would be refused for the same reason the service's was. On a
// throwaway database the answer is `rig db reset`.
func publicationPlan(doc *ir.Document, streaming []string) (scaffold.PublishOptions, error) {
	if doc.Schema.Replication == nil {
		return scaffold.PublishOptions{}, errors.New(
			"this project's schema carries no replication facts, so there is nothing to compare against; " +
				"run `rig db up` and try again — the migration is written from what the database already has")
	}

	plan := scaffold.PublishOptions{Publication: scaffold.DefaultPublication, Create: true}
	for _, pub := range doc.Schema.Replication.Publications {
		if pub.Name != scaffold.DefaultPublication {
			continue
		}
		plan.Create = false
		if !pub.Owned {
			// Two ways out, and which one applies is about the database rather than
			// about rig — so both are named rather than the one that fits a laptop.
			// `rig db reset` is not advice anybody can take on a managed database,
			// and the ownership transfer is a single statement that costs nothing:
			// the sync service keeps running, it rebuilds its slot if it has to, and
			// a publication it no longer owns is one it will not empty.
			return scaffold.PublishOptions{}, fmt.Errorf(
				"%[1]s already exists and the sync service owns it, so this project's migrations "+
					"cannot maintain it — it was created on first connection, before anything "+
					"published these tables, and rig will not write an ALTER PUBLICATION that "+
					"Postgres would refuse.\n\n"+
					"Hand it over, which is one statement and loses nothing:\n"+
					"    ALTER PUBLICATION %[1]s OWNER TO <the role that runs migrations>;\n"+
					"then run this again. On a throwaway database `rig db reset` does the same "+
					"thing by dropping it and letting the migration create it first",
				scaffold.DefaultPublication)
		}
		break
	}

	for _, name := range streaming {
		t := schemaTable(doc, name)
		if t == nil {
			// A resource whose table is not in the schema is a document the
			// compiler would not have produced. Nothing to say about it here.
			continue
		}

		if slices.Contains(t.Publications, scaffold.DefaultPublication) {
			plan.Already = append(plan.Already, name)
		} else {
			plan.Add = append(plan.Add, name)
		}

		// Separately, and not in the same branch: a table published by an earlier
		// migration that never had its identity set is exactly the state RIG5093
		// reports and RIG5090 does not, and it is the one this half exists for.
		//
		// An empty identity is a schema nobody read this off, which cannot happen
		// on the path that got here — e.compile introspects — but treating it as
		// "already fine" rather than "needs changing" is the reading that never
		// writes a statement on a guess.
		if t.ReplicaIdentity != "" && t.ReplicaIdentity != ir.ReplicaIdentityFull {
			plan.Identity = append(plan.Identity, name)
		}
	}

	return plan, nil
}

// schemaTable finds a table in the document's schema.
func schemaTable(doc *ir.Document, name string) *ir.Table {
	for i := range doc.Schema.Tables {
		if doc.Schema.Tables[i].Name == name {
			return &doc.Schema.Tables[i]
		}
	}
	return nil
}
