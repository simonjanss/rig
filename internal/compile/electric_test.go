package compile_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/compile"
	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/internal/project"
	"github.com/simonjanss/rig/internal/tableconf"
	"github.com/simonjanss/rig/pkg/ir"
)

const electricProject = "project:\n  name: demo\n  module: example.com/demo\n"

// compileElectric runs a fixture with its replication facts rewritten, so every
// answer a database can give about a publication gets a case without a fixture
// directory of its own.
//
// The facts are changed in Go rather than in the fixture's schema.json because
// that is where they come from in a real run: introspection reads them off the
// server, and no configuration file has a say.
func compileElectric(t *testing.T, fixture string, mutate func(*ir.Schema)) string {
	t.Helper()

	dir := filepath.Join("testdata", fixture)
	schema := readSchema(t, filepath.Join(dir, "schema.json"))
	if mutate != nil {
		mutate(&schema)
	}

	projectSrc := electricProject
	if b, err := os.ReadFile(filepath.Join(dir, "rig.yaml")); err == nil {
		projectSrc = string(b)
	}
	p, pdiags := project.Parse(filepath.Join(dir, "rig.yaml"), []byte(projectSrc))
	if pdiags.HasErrors() {
		t.Fatalf("rig.yaml:\n%s", pdiags.String())
	}

	paths, err := filepath.Glob(filepath.Join(dir, "tables", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	set, tdiags := tableconf.LoadDir(paths)

	_, cdiags := compile.Compile(schema, set, compile.Options{
		Project:    p,
		Tool:       "rig (test)",
		Foundation: readFoundation(t, dir),
	})

	var all diag.List
	all.Append(tdiags)
	all.Append(cdiags)
	return all.String()
}

// schemaTable finds a table so a case can rewrite one fact about it.
func schemaTable(t *testing.T, schema *ir.Schema, name string) *ir.Table {
	t.Helper()

	for i := range schema.Tables {
		if schema.Tables[i].Name == name {
			return &schema.Tables[i]
		}
	}
	t.Fatalf("the fixture has no table %q", name)
	return nil
}

// The whole rule rests on this case. A schema that came from `rig ir
// --dump-schema` under an older rig, or from a fixture written by hand, carries
// no replication block at all — and a table reported as unpublished on evidence
// nobody collected would fail `rig validate --schema` on a project where
// everything is right.
func TestReplicationNobodyReadIsNotReported(t *testing.T) {
	t.Parallel()

	out := compileElectric(t, "electricpublished", func(s *ir.Schema) {
		s.Replication = nil
		schemaTable(t, s, "todo").Publications = nil
		schemaTable(t, s, "todo").Unlogged = true
		// Left empty rather than set to something wrong, because empty is what a
		// dump written before rig read this carries — and it is the whole reason
		// the replica identity has a nil check of its own rather than riding on
		// the schema's replication block.
		schemaTable(t, s, "todo").ReplicaIdentity = ""
	})
	for _, code := range []string{"RIG5090", "RIG5091", "RIG5092", "RIG5093"} {
		if strings.Contains(out, code) {
			t.Errorf("%s fired on a schema with no replication facts:\n%s", code, out)
		}
	}
}

func TestAPublishedTableIsAccepted(t *testing.T) {
	t.Parallel()

	if out := compileElectric(t, "electricpublished", nil); out != "" {
		t.Errorf("the published fixture should compile clean:\n%s", out)
	}
}

// The two ways a table can be missing from a publication read differently, and
// the message says which one it is: a database with nothing published is a
// migration nobody wrote, and a database with a publication that carries other
// tables is one table left out of it.
func TestAnUnpublishedTableWithLiveSyncIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		mutate func(*ir.Schema)
		want   string
	}{
		{
			name: "no publication at all",
			mutate: func(s *ir.Schema) {
				s.Replication.Publications = nil
				schemaTable(t, s, "todo").Publications = nil
			},
			want: "this database has no publication at all",
		},
		{
			name: "published somewhere else",
			mutate: func(s *ir.Schema) {
				schemaTable(t, s, "todo").Publications = nil
			},
			want: "no publication carries it (this database has app_publication)",
		},
		{
			// The sync service publishes a table itself on the first
			// subscription, so on a machine where the app has been opened once
			// this is what "unpublished" looks like — and taking it as an
			// answer would leave the rule silent exactly where it is run.
			name: "carried only by the sync service's own publication",
			mutate: func(s *ir.Schema) {
				s.Replication.Publications = []ir.Publication{{Name: "electric_publication_default"}}
				schemaTable(t, s, "todo").Publications = []string{"electric_publication_default"}
			},
			want: "is one the sync service created and owns",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			out := compileElectric(t, "electricpublished", tc.mutate)
			if !strings.Contains(out, "RIG5090") || !strings.Contains(out, tc.want) {
				t.Errorf("want RIG5090 containing %q:\n%s", tc.want, out)
			}
		})
	}
}

// A table in a publication a migration wrote is published whatever else is also
// carrying it, and the sync service having added it to its own alongside is the
// normal state of a developer's database rather than something to report.
func TestASyncServicePublicationBesideYourOwnIsFine(t *testing.T) {
	t.Parallel()

	out := compileElectric(t, "electricpublished", func(s *ir.Schema) {
		s.Replication.Publications = append(s.Replication.Publications,
			ir.Publication{Name: "electric_publication_default"})
		tbl := schemaTable(t, s, "todo")
		tbl.Publications = append(tbl.Publications, "electric_publication_default")
	})
	if out != "" {
		t.Errorf("a table published by a migration and by Electric should compile clean:\n%s", out)
	}
}

// An UNLOGGED table can be added to a publication — Postgres accepts the ALTER
// — and it still emits nothing. So the two halves are reported independently:
// fixing the membership on a table that writes no WAL would change nothing, and
// hearing only about the membership is how somebody would spend an afternoon
// doing exactly that.
func TestAnUnloggedTableWithLiveSyncIsRefused(t *testing.T) {
	t.Parallel()

	published := compileElectric(t, "electricpublished", func(s *ir.Schema) {
		schemaTable(t, s, "todo").Unlogged = true
	})
	if !strings.Contains(published, "RIG5091") {
		t.Errorf("a published UNLOGGED table was accepted:\n%s", published)
	}
	if strings.Contains(published, "RIG5090") {
		t.Errorf("the membership rule fired on a published table:\n%s", published)
	}

	neither := compileElectric(t, "electricpublished", func(s *ir.Schema) {
		tbl := schemaTable(t, s, "todo")
		tbl.Unlogged = true
		tbl.Publications = nil
	})
	for _, code := range []string{"RIG5090", "RIG5091"} {
		if !strings.Contains(neither, code) {
			t.Errorf("want %s on a table that is neither logged nor published:\n%s", code, neither)
		}
	}
}

// A table that never asked for a stream is not asked about publications. Most
// tables in most projects are read over the API and nothing else, and a rule
// that nagged about all of them would say nothing about the one that matters.
func TestATableWithoutLiveSyncIsNotAskedAboutPublications(t *testing.T) {
	t.Parallel()

	out := compileElectric(t, "minimal", func(s *ir.Schema) {
		s.Replication = &ir.Replication{WALLevel: "replica"}
	})
	for _, code := range []string{"RIG5090", "RIG5091", "RIG5092"} {
		if strings.Contains(out, code) {
			t.Errorf("%s fired on a project with no shapes:\n%s", code, out)
		}
	}
}

// wal_level is the server's, so a project with a dozen shapes has one thing to
// fix rather than a dozen.
func TestTheWALLevelIsReportedOnce(t *testing.T) {
	t.Parallel()

	out := compileElectric(t, "electricunpublished", func(s *ir.Schema) {
		schemaTable(t, s, "todo").Publications = []string{"app_publication"}
	})
	if n := strings.Count(out, "RIG5092"); n != 1 {
		t.Errorf("wal_level reported %d times, want 1:\n%s", n, out)
	}
}

// The tables rig gives a shape to without being asked are held to the same rule,
// and this is the case a reader is most likely to be surprised by: a project
// with `notifications:` on and no table of its own mentioning live sync still
// has something to publish, because a client reads its inbox as a stream or not
// at all. Exempting rig's own tables would hide exactly the stream nobody would
// think to check.
func TestRigsOwnStreamedTablesNeedPublishingToo(t *testing.T) {
	t.Parallel()

	out := compileElectric(t, "notify", func(s *ir.Schema) {
		s.Replication = &ir.Replication{WALLevel: "logical"}
	})
	if !strings.Contains(out, "RIG5090") ||
		!strings.Contains(out, "rig_notification_recipient") {
		t.Errorf("want RIG5090 for the inbox:\n%s", out)
	}
}

// The replica identity is the other half of publishing a table, gated on the same
// privilege, and the three values that are not `full` fail for three different
// reasons — so the message says which one this is rather than only that it is
// wrong.
func TestATableWithoutAFullReplicaIdentityIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		identity ir.ReplicaIdentity
		want     string
	}{
		{ir.ReplicaIdentityDefault, "the default carries the primary key of the old row"},
		{ir.ReplicaIdentityIndex, "an index identity carries only that index's columns"},
		{ir.ReplicaIdentityNothing, "nothing carries no old row at all"},
	} {
		t.Run(string(tc.identity), func(t *testing.T) {
			out := compileElectric(t, "electricpublished", func(s *ir.Schema) {
				schemaTable(t, s, "todo").ReplicaIdentity = tc.identity
			})
			if !strings.Contains(out, "RIG5093") {
				t.Errorf("a %s replica identity was accepted:\n%s", tc.identity, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("the message does not say what this identity costs:\n%s", out)
			}
		})
	}
}

func TestAFullReplicaIdentityIsAccepted(t *testing.T) {
	t.Parallel()

	out := compileElectric(t, "electricpublished", func(s *ir.Schema) {
		schemaTable(t, s, "todo").ReplicaIdentity = ir.ReplicaIdentityFull
	})
	if out != "" {
		t.Errorf("a published table with a full replica identity should compile clean:\n%s", out)
	}
}

// Both halves at once, because they are one job: a project that published the
// table and left the identity alone is the case RIG5090's own remedy text used to
// have to describe in prose because nothing could see it.
func TestAnUnpublishedTableWithoutAFullIdentityIsToldBoth(t *testing.T) {
	t.Parallel()

	out := compileElectric(t, "electricpublished", func(s *ir.Schema) {
		schemaTable(t, s, "todo").Publications = nil
		schemaTable(t, s, "todo").ReplicaIdentity = ir.ReplicaIdentityDefault
	})
	for _, code := range []string{"RIG5090", "RIG5093"} {
		if !strings.Contains(out, code) {
			t.Errorf("%s did not fire, so only half the job was reported:\n%s", code, out)
		}
	}
}

// The case the owner check exists for, and the one that took a real server to find.
//
// The sync service reads only its own publication, so a table published into
// `rig_publication` and nowhere else streams nothing for a role that owns no tables.
// What works is a migration creating `electric_publication_default` itself, before the
// service ever connects — and the only thing telling that apart from the service having
// made it is who owns it.
func TestAnElectricPublicationAMigrationOwnsIsAccepted(t *testing.T) {
	t.Parallel()

	out := compileElectric(t, "electricpublished", func(s *ir.Schema) {
		s.Replication.Publications = []ir.Publication{
			{Name: "electric_publication_default", Owned: true},
		}
		schemaTable(t, s, "todo").Publications = []string{"electric_publication_default"}
		schemaTable(t, s, "todo").ReplicaIdentity = ir.ReplicaIdentityFull
	})
	if out != "" {
		t.Errorf("a publication this project's migrations own was refused:\n%s", out)
	}
}

// And the same publication, same name, owned by the sync service instead. Refused,
// because a table in it got there on privileges the service will not have elsewhere —
// which is the dependency the rule is about.
func TestAnElectricPublicationTheServiceOwnsIsStillRefused(t *testing.T) {
	t.Parallel()

	out := compileElectric(t, "electricpublished", func(s *ir.Schema) {
		s.Replication.Publications = []ir.Publication{
			{Name: "electric_publication_default", Owned: false},
		}
		schemaTable(t, s, "todo").Publications = []string{"electric_publication_default"}
		schemaTable(t, s, "todo").ReplicaIdentity = ir.ReplicaIdentityFull
	})
	if !strings.Contains(out, "RIG5090") {
		t.Errorf("the sync service's own publication was taken as an answer:\n%s", out)
	}
}
