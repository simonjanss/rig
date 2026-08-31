package scaffold

import (
	"fmt"
	"strings"
)

// DefaultPublication is the publication rig writes streaming tables into.
//
// The sync service's own name, and that is the whole point rather than a
// collision: **the sync service reads only its own publication.** A publication
// under a name of the project's own is never consulted, so a table published there
// and nowhere else streams nothing for a database role that owns no tables — which
// is every deployment worth granting least privilege.
//
// So the migration creates this one, before the service ever connects, and owns it.
// The service then finds its publication already correct and verifies it instead of
// building it, which is what ELECTRIC_MANUAL_TABLE_PUBLISHING=true asks for and
// what removes the table ownership it would otherwise need.
//
// The suffix is the replication stream id, which is "default" unless a deployment
// sets ELECTRIC_REPLICATION_STREAM_ID. A deployment that sets one has to rename
// this in the migration to match, and there is nothing rig can read to know.
const DefaultPublication = "electric_publication_default"

// PublishOptions describe the migration that makes a table streamable.
type PublishOptions struct {
	// Publication is the publication to write into. Empty means
	// [DefaultPublication].
	Publication string
	// Create says the publication does not exist yet, so this migration is the
	// one that makes it — which is also what decides whether Down may drop it.
	Create bool
	// Add are the tables to add to the publication, in the order they should be
	// named.
	Add []string
	// Identity are the tables whose replica identity has to become FULL.
	//
	// Usually the same list as [PublishOptions.Add] and not always: a table
	// published by an earlier migration that never had its identity set is in
	// this one and not that one, which is exactly the state RIG5093 reports and
	// RIG5090 does not.
	Identity []string
	// Already are the tables the publication carries already, named in a comment
	// so the file says what it left alone.
	Already []string
}

// Publish renders the migration that publishes a project's streaming tables and
// gives them a replica identity live sync can use.
//
// Both halves in one file because Postgres gates both on owning the table, so
// they are one job: a migration that publishes and stops has moved half of what a
// least-privilege deployment cannot do for itself.
//
// It is a migration rather than something rig applies because the answer has to
// be the same in every environment. The sync service will publish a table itself
// on the first subscription — which is why this is easy never to think about on a
// laptop, where its role owns everything — and cannot where it does not own the
// table, which is every deployment that grants least privilege.
func Publish(opt PublishOptions) string {
	pub := opt.Publication
	if pub == "" {
		pub = DefaultPublication
	}

	var b strings.Builder

	b.WriteString("-- +goose Up\n-- +goose StatementBegin\n\n")

	fmt.Fprintf(&b, "-- Publish the tables that carry a live-sync shape.\n"+
		"--\n"+
		"-- A shape is a filter in front of a stream the sync service follows over logical\n"+
		"-- replication, and logical replication carries only what a publication names. The sync\n"+
		"-- service will add a table itself on the first subscription, so this is not the\n"+
		"-- difference between a stream and no stream — it is the difference between one that\n"+
		"-- works everywhere and one that works wherever the sync service's database role happens\n"+
		"-- to own the table. Postgres wants ownership both to publish a table and to set\n"+
		"-- REPLICA IDENTITY FULL, and a deployment with least privilege grants neither. RIG5090\n"+
		"-- and RIG5093 are the diagnostics.\n"+
		"--\n"+
		"-- Written by `rig migration new --publish-shapes` from the tables that ask for a shape,\n"+
		"-- which is why rig's own are here beside the project's: an inbox line and a presence row\n"+
		"-- are streamed because `notifications:` and `presence:` are on, without any table's\n"+
		"-- configuration asking.\n"+
		"--\n"+
		"-- The publication is the sync service's own, %s, and that is\n"+
		"-- deliberate: it reads only that one. A publication under a name of ours would satisfy\n"+
		"-- nothing at run time. This migration creates it first and therefore owns it, which is\n"+
		"-- what lets the service run as a role owning no tables.\n"+
		"--\n"+
		"-- **The deployment has to set ELECTRIC_MANUAL_TABLE_PUBLISHING=true.** Without it the\n"+
		"-- service tries to maintain this publication itself and fails on table ownership, and\n"+
		"-- the error it reports — `must be owner of table <table>` — says nothing about either.\n"+
		"-- `rig db up` needs nothing: locally the service owns everything.\n"+
		"--\n"+
		"-- Under ELECTRIC_REPLICATION_STREAM_ID the name has a different suffix. Rename it here\n"+
		"-- to match; there is nothing rig can read to know.\n", pub)

	if len(opt.Already) > 0 {
		fmt.Fprintf(&b, "--\n-- Already carried by %s, and left alone: %s.\n",
			pub, strings.Join(opt.Already, ", "))
	}
	b.WriteString("\n")

	if opt.Create {
		fmt.Fprintf(&b,
			"-- Postgres has no CREATE PUBLICATION IF NOT EXISTS, hence the block.\n"+
				"DO $$\nBEGIN\n"+
				"    IF NOT EXISTS (SELECT 1 FROM pg_publication WHERE pubname = '%s') THEN\n"+
				"        CREATE PUBLICATION %s;\n"+
				"    END IF;\nEND\n$$;\n\n", pub, pub)
	}

	if len(opt.Add) > 0 {
		fmt.Fprintf(&b, "ALTER PUBLICATION %s ADD TABLE %s;\n\n", pub, strings.Join(opt.Add, ", "))
	}

	if len(opt.Identity) > 0 {
		// The first sentence differs by whether anything was published above it, so
		// that a file with one ALTER TABLE in it does not point at a line that is
		// not there. Both say the same thing: this is gated on ownership too.
		if len(opt.Add) > 0 {
			b.WriteString("-- REPLICA IDENTITY is the second half of the same job, and it is gated on\n" +
				"-- ownership exactly as the line above is:")
		} else {
			b.WriteString("-- REPLICA IDENTITY is the other half of publishing a table, and Postgres gates it\n" +
				"-- on owning the table just the same. These were published by an earlier migration\n" +
				"-- that did not set it:")
		}
		b.WriteString(" the sync service wants the whole old row on\n" +
			"-- an update or a delete, and the default identity carries only the primary key. A\n" +
			"-- subscriber then hears that a row changed with nothing to match against the one it is\n" +
			"-- holding — inserts keep working, which is what lets this survive a demo.\n")
		for _, t := range opt.Identity {
			fmt.Fprintf(&b, "ALTER TABLE %s REPLICA IDENTITY FULL;\n", t)
		}
		b.WriteString("\n")
	}

	b.WriteString("-- +goose StatementEnd\n\n-- +goose Down\n-- +goose StatementBegin\n\n")

	if opt.Create {
		fmt.Fprintf(&b, "DROP PUBLICATION %s;\n", pub)
	} else if len(opt.Add) > 0 {
		// Not DROP PUBLICATION: this migration did not create it, and an earlier
		// one's tables are in it. Down undoes what Up did and no more.
		fmt.Fprintf(&b, "ALTER PUBLICATION %s DROP TABLE %s;\n", pub, strings.Join(opt.Add, ", "))
	}

	if len(opt.Identity) > 0 {
		if opt.Create || len(opt.Add) > 0 {
			b.WriteString("\n")
		}
		for _, t := range opt.Identity {
			fmt.Fprintf(&b, "ALTER TABLE %s REPLICA IDENTITY DEFAULT;\n", t)
		}
	}

	b.WriteString("\n-- +goose StatementEnd\n")
	return b.String()
}
