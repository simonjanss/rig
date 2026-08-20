package dbschema_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/simonjanss/rig/runtime/dbschema"
)

// A set every module carrying one should look like, for the cases below to
// deviate from one field at a time.
func good() dbschema.Set {
	return dbschema.Set{
		Module: "rig/example",
		FS: fstest.MapFS{
			"00001_first.sql":  &fstest.MapFile{Data: []byte("-- +goose Up\nSELECT 1;\n")},
			"00002_second.sql": &fstest.MapFile{Data: []byte("-- +goose Up\nSELECT 2;\n")},
		},
		Dir:   ".",
		Table: "rig_example_migrations",
		Migrations: []dbschema.Migration{
			{Number: 1, Name: "first", Tables: []string{"rig_thing"}},
			// An upgrade: it alters what the first one created, so it creates
			// nothing and says so with an empty Tables.
			{Number: 2, Name: "second"},
		},
	}
}

func TestAGoodSetValidates(t *testing.T) {
	t.Parallel()

	if err := good().Validate(); err != nil {
		t.Fatal(err)
	}
}

// Every way the manifest and the files can disagree, and the two rules that make
// append-only mechanical rather than a convention.
func TestValidateRefuses(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		set  func(s *dbschema.Set)
		want string
	}{
		{
			"no module",
			func(s *dbschema.Set) { s.Module = "" },
			"Module",
		},
		{
			// A set writing into the project's own table, or into another set's,
			// would share a numbering sequence with it.
			"no table",
			func(s *dbschema.Set) { s.Table = "" },
			"no Table",
		},
		{
			"no migrations",
			func(s *dbschema.Set) { s.Migrations = nil },
			"no migrations",
		},
		{
			// The append-only rule. An entry inserted between two shipped ones
			// renumbers a migration somebody's database has already applied.
			"a gap in the numbers",
			func(s *dbschema.Set) { s.Migrations[1].Number = 3 },
			"append-only",
		},
		{
			"numbers that do not start at one",
			func(s *dbschema.Set) { s.Migrations[0].Number = 0 },
			"append-only",
		},
		{
			"a migration with no name",
			func(s *dbschema.Set) { s.Migrations[0].Name = "" },
			"no name",
		},
		{
			// The name is what survives vendoring, so two of them would make a
			// vendored file ambiguous about which migration it is.
			"two migrations by one name",
			func(s *dbschema.Set) { s.Migrations[1].Name = "first" },
			"two migrations named",
		},
		{
			"a manifest entry with no file",
			func(s *dbschema.Set) { s.Migrations[1].Name = "missing" },
			"not in the set",
		},
		{
			// A table belongs to the migration that creates it and to no other.
			"one table claimed twice",
			func(s *dbschema.Set) { s.Migrations[1].Tables = []string{"rig_thing"} },
			"claims table",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := good()
			tc.set(&s)

			err := s.Validate()
			if err == nil {
				t.Fatalf("want a refusal mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, should mention %q", err, tc.want)
			}
		})
	}
}

// A .sql file the manifest does not name is the dangerous direction: goose reads
// the directory, so it would be applied, while vendoring and every other reader
// works from the manifest and would miss it.
func TestAFileNobodyNamesIsRefused(t *testing.T) {
	t.Parallel()

	s := good()
	s.FS = fstest.MapFS{
		"00001_first.sql":  &fstest.MapFile{Data: []byte("-- +goose Up\nSELECT 1;\n")},
		"00002_second.sql": &fstest.MapFile{Data: []byte("-- +goose Up\nSELECT 2;\n")},
		"00003_stray.sql":  &fstest.MapFile{Data: []byte("-- +goose Up\nSELECT 3;\n")},
	}

	err := s.Validate()
	if err == nil {
		t.Fatal("a file the manifest does not name should be refused")
	}
	if !strings.Contains(err.Error(), "00003_stray.sql") {
		t.Errorf("err = %v, should name the stray file", err)
	}
}

func TestTablesAndFindAndRead(t *testing.T) {
	t.Parallel()

	s := good()

	if got := s.Tables(); len(got) != 1 || got[0] != "rig_thing" {
		t.Errorf("Tables() = %v, want just the one the first migration creates", got)
	}

	if m, ok := s.Find("second"); !ok || m.Number != 2 {
		t.Errorf("Find(second) = %+v, %v", m, ok)
	}
	if _, ok := s.Find("nothing"); ok {
		t.Error("Find should report a name it does not have")
	}

	sql, err := s.Read("first")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sql), "SELECT 1") {
		t.Errorf("Read(first) = %q", sql)
	}

	if _, err := s.Read("nothing"); err == nil {
		t.Error("Read should refuse a name the manifest does not have")
	}
}

// A set whose files sit in a subdirectory, which is what a project's own
// migrations directory looks like next to a module's package root.
func TestASubdirectorySetReadsAndValidates(t *testing.T) {
	t.Parallel()

	s := dbschema.Set{
		Module: "rig/example",
		FS: fstest.MapFS{
			"migrations/00001_first.sql": &fstest.MapFile{Data: []byte("-- +goose Up\nSELECT 1;\n")},
		},
		Dir:        "migrations",
		Table:      "rig_example_migrations",
		Migrations: []dbschema.Migration{{Number: 1, Name: "first", Tables: []string{"rig_thing"}}},
	}

	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	sql, err := s.Read("first")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sql), "SELECT 1") {
		t.Errorf("Read(first) = %q", sql)
	}
}

func TestFileIsTheNameOnDisk(t *testing.T) {
	t.Parallel()

	m := dbschema.Migration{Number: 7, Name: "tenancy_add_locale"}
	if got := m.File(); got != "00007_tenancy_add_locale.sql" {
		t.Errorf("File() = %q", got)
	}
}
