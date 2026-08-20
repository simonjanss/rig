// Package dbschema describes a migration set a rig module carries with it.
//
// rig owns a dozen tables — the identities, the tokens, the file rows, the
// notifications — and their DDL has to reach a project's database somehow. There
// are two readings of how. A project can vendor the set, in which case the files
// land in its own migrations directory and its own history records them; or it
// can leave each module to apply its own, in which case every set is applied and
// recorded separately. Both readings need the same facts about a set: where the
// files are, what order they go in, which tables each one creates, and where it
// writes down how far it got. That is what this package is, and it is all it is.
//
// It lives in runtime because runtime is the one module every generated
// application already imports, so a set declared in these terms costs nobody a
// dependency they did not already have. It deliberately holds no migration
// engine: applying a set is rig/migrate's job, and a module that carries schema
// should not thereby carry goose.
package dbschema

import (
	"fmt"
	"io/fs"
	"slices"
	"strings"
)

// A Set is one module's migrations: append-only, numbered from one, and recorded
// in a bookkeeping table of its own.
//
// The private table is what lets the modules be released independently. Two
// modules sharing one would share one numbering sequence, and two of them adding
// a migration in the same release would collide on a version number — which
// goose refuses outright rather than resolves. So the separation is not
// tidiness. It is the reason three separately tagged modules never have to agree
// about a number.
type Set struct {
	// Module is the module that owns the set, for a message that has to say
	// which one a database is behind.
	Module string
	// FS holds the files, and Dir names the directory inside it.
	FS  fs.FS
	Dir string
	// Table is where this set records how far it has been applied. It is not the
	// project's own bookkeeping table and must never be.
	Table string
	// Migrations are the set's entries, in the order they apply.
	Migrations []Migration
}

// A Migration is one file in a [Set].
//
// Number and Name are separate facts because vendoring renumbers. A set copied
// into a project's migrations directory is renumbered into that project's
// sequence, so the number identifies a migration inside its own set and nowhere
// else. The name identifies it everywhere, which is why a vendored file carries
// it: `00009_rig_tenancy.sql` is recognisable as this set's `tenancy` however it
// was numbered on the way in.
type Migration struct {
	// Number is the filename's numeric prefix within this set.
	Number int
	// Name is the migration's identity, stable across vendoring.
	Name string
	// Tables are the tables this migration creates.
	//
	// Written down rather than parsed out of the SQL, so that adding a table to
	// the foundation is a decision somebody makes here as well: which side of
	// that line a table falls on decides whether rig generates code for it, and
	// a heuristic would quietly move one.
	//
	// Empty for a migration that only alters what an earlier one created, which is
	// what most upgrades are. A table belongs to the migration that creates it and
	// to no other, so a later ALTER adds nothing here — the column it adds arrives
	// through introspection like any other.
	Tables []string
}

// File is the migration's name inside its set's filesystem.
func (m Migration) File() string {
	return fmt.Sprintf("%05d_%s.sql", m.Number, m.Name)
}

// Tables are every table a set creates, in apply order.
func (s Set) Tables() []string {
	var out []string
	for _, m := range s.Migrations {
		out = append(out, m.Tables...)
	}
	return out
}

// Find returns the migration with this name, and whether there was one.
func (s Set) Find(name string) (Migration, bool) {
	for _, m := range s.Migrations {
		if m.Name == name {
			return m, true
		}
	}
	return Migration{}, false
}

// Read returns a migration's SQL.
//
// The error is a programming mistake rather than a condition to handle: the
// manifest and the embedded files ship together, so a name in one and not the
// other is a set that should have failed [Set.Validate].
func (s Set) Read(name string) ([]byte, error) {
	m, ok := s.Find(name)
	if !ok {
		return nil, fmt.Errorf("dbschema: %s has no migration named %q", s.Module, name)
	}
	return fs.ReadFile(s.FS, s.path(m))
}

func (s Set) path(m Migration) string {
	if s.Dir == "" || s.Dir == "." {
		return m.File()
	}
	return s.Dir + "/" + m.File()
}

// Validate reports a set whose manifest and files disagree.
//
// A set ships both halves in one package, so a name in one and not the other is a
// mistake that belongs in the owning module's test run rather than in somebody's
// database. It is also where append-only stops being a convention: numbers start
// at one, ascend, and never repeat, so an entry inserted between two shipped ones
// fails here instead of renumbering a migration that has already been applied.
//
// Every module carrying a set is expected to call this in a test of its own. It
// is not called at run time — a set is a compile-time constant in all but name,
// and checking it on every start would be paying for a mistake that cannot
// survive a test run.
func (s Set) Validate() error {
	if s.Module == "" {
		return fmt.Errorf("dbschema: a set with no Module cannot say what is behind")
	}
	if s.Table == "" {
		return fmt.Errorf("dbschema: %s has no Table to record itself in", s.Module)
	}
	if len(s.Migrations) == 0 {
		return fmt.Errorf("dbschema: %s has no migrations", s.Module)
	}

	seenTable := map[string]string{}
	named := map[string]bool{}
	for i, m := range s.Migrations {
		if m.Number != i+1 {
			return fmt.Errorf("dbschema: %s migration %q is numbered %d, expected %d — "+
				"the set is append-only, so numbers start at one and ascend with no gaps",
				s.Module, m.Name, m.Number, i+1)
		}
		if m.Name == "" {
			return fmt.Errorf("dbschema: %s migration %d has no name, and the name is what "+
				"survives vendoring", s.Module, m.Number)
		}
		if named[m.Name] {
			return fmt.Errorf("dbschema: %s has two migrations named %q", s.Module, m.Name)
		}
		named[m.Name] = true

		if _, err := fs.Stat(s.FS, s.path(m)); err != nil {
			return fmt.Errorf("dbschema: %s names migration %q but %s is not in the set: %w",
				s.Module, m.Name, s.path(m), err)
		}
		// No check that Tables is non-empty: a migration that only alters what an
		// earlier one created is the ordinary shape of an upgrade, and it creates
		// nothing.
		for _, t := range m.Tables {
			if owner, taken := seenTable[t]; taken {
				return fmt.Errorf("dbschema: %s claims table %q in both %q and %q",
					s.Module, t, owner, m.Name)
			}
			seenTable[t] = m.Name
		}
	}

	// The other direction. A .sql file nobody names is a migration that would be
	// applied by goose — which reads the directory — and be invisible to every
	// caller that reads the manifest, vendoring included.
	dir := s.Dir
	if dir == "" {
		dir = "."
	}
	entries, err := fs.ReadDir(s.FS, dir)
	if err != nil {
		return fmt.Errorf("dbschema: %s cannot read %s: %w", s.Module, dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		if !slices.ContainsFunc(s.Migrations, func(m Migration) bool { return m.File() == name }) {
			return fmt.Errorf("dbschema: %s carries %s, which its manifest does not name — "+
				"goose would apply it and every other reader would miss it", s.Module, name)
		}
	}
	return nil
}
