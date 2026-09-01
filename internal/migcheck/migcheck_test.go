package migcheck_test

import (
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/internal/migcheck"
)

func TestVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		file   string
		want   int64
		wantOK bool
	}{
		{"padded", "00025_add_column.sql", 25, true},
		{"unpadded", "25_add_column.sql", 25, true},
		{"single digit", "1_init.sql", 1, true},
		{"path is reduced to its base", "migrations/00025_add_column.sql", 25, true},
		{"windows separators", `migrations\00025_add_column.sql`, 25, true},
		{"mixed case description still has a version", "00025_addColumn.sql", 25, true},
		{"no underscore", "00025.sql", 0, false},
		{"no version", "add_column.sql", 0, false},
		{"not sql", "00025_add_column.txt", 0, false},
		{"empty", "", 0, false},
		{"prefix too long for an int64", strings.Repeat("9", 25) + "_x.sql", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := migcheck.Version(tt.file)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("Version(%q) = (%d, %v), want (%d, %v)", tt.file, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// A name goose applies is not the same set as a name rig would have written,
// and the gap between the two is where every interesting case lives.
func TestWellNamed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		file string
		want bool
	}{
		{"00025_add_column.sql", true},
		{"00001_a.sql", true},
		{"00025_add_column_2.sql", true},
		{"25_add_column.sql", false},     // too few digits
		{"000025_add_column.sql", false}, // too many
		{"00025_addColumn.sql", false},   // not snake_case
		{"00025_add-column.sql", false},
		{"00025_Add_Column.sql", false},
		{"00025.sql", false},
		{"README.md", false},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			t.Parallel()
			if got := migcheck.WellNamed(tt.file); got != tt.want {
				t.Errorf("WellNamed(%q) = %v, want %v", tt.file, got, tt.want)
			}
		})
	}
}

// The two parses disagreeing is the point, not an accident: a file rig would not
// have written is still a version goose will apply.
func TestBadlyNamedFileStillHasAVersion(t *testing.T) {
	t.Parallel()

	const name = "25_by_hand.sql"
	if migcheck.WellNamed(name) {
		t.Fatalf("WellNamed(%q) = true, want false", name)
	}
	if v, ok := migcheck.Version(name); !ok || v != 25 {
		t.Fatalf("Version(%q) = (%d, %v), want (25, true)", name, v, ok)
	}
}

func TestMaxVersion(t *testing.T) {
	t.Parallel()

	t.Run("empty is zero", func(t *testing.T) {
		t.Parallel()
		if got := migcheck.MaxVersion(nil); got != 0 {
			t.Errorf("MaxVersion(nil) = %d, want 0", got)
		}
	})

	t.Run("non-migrations are ignored", func(t *testing.T) {
		t.Parallel()
		got := migcheck.MaxVersion([]string{"00007_a.sql", "README.md", "00003_b.sql"})
		if got != 7 {
			t.Errorf("MaxVersion = %d, want 7", got)
		}
	})

	t.Run("padding does not decide the order", func(t *testing.T) {
		t.Parallel()
		// Lexically "9_a.sql" sorts above "00010_b.sql"; numerically it does not.
		got := migcheck.MaxVersion([]string{"9_a.sql", "00010_b.sql"})
		if got != 10 {
			t.Errorf("MaxVersion = %d, want 10", got)
		}
	})
}

func TestCheckNames(t *testing.T) {
	t.Parallel()

	t.Run("a well-named directory is silent", func(t *testing.T) {
		t.Parallel()
		d := migcheck.CheckNames("migrations",
			[]string{"00001_a.sql", "00002_b.sql"}, diag.SeverityError)
		if d.Len() != 0 {
			t.Fatalf("got %d diagnostics, want 0: %s", d.Len(), d.String())
		}
	})

	t.Run("a badly named migration is reported", func(t *testing.T) {
		t.Parallel()
		d := migcheck.CheckNames("migrations",
			[]string{"1_a.sql", "00002_b.sql"}, diag.SeverityError)
		if d.Len() != 1 {
			t.Fatalf("got %d diagnostics, want 1: %s", d.Len(), d.String())
		}
		got := d.All()[0]
		if got.Code.ID != "RIG6050" {
			t.Errorf("code = %s, want RIG6050", got.Code.ID)
		}
		if got.Anchor.File != "migrations/1_a.sql" {
			t.Errorf("anchor = %q, want migrations/1_a.sql", got.Anchor.File)
		}
	})

	// A README beside the migrations is not a badly named migration, and a rule
	// that said it was could not be left on.
	t.Run("a non-sql file is not a migration", func(t *testing.T) {
		t.Parallel()
		d := migcheck.CheckNames("migrations",
			[]string{"README.md", ".keep", "notes.txt"}, diag.SeverityError)
		if d.Len() != 0 {
			t.Fatalf("got %d diagnostics, want 0: %s", d.Len(), d.String())
		}
	})

	t.Run("an empty severity reports nothing", func(t *testing.T) {
		t.Parallel()
		d := migcheck.CheckNames("migrations", []string{"1_a.sql"}, "")
		if d.Len() != 0 {
			t.Fatalf("got %d diagnostics, want 0: %s", d.Len(), d.String())
		}
	})

	t.Run("the configured severity is used", func(t *testing.T) {
		t.Parallel()
		d := migcheck.CheckNames("migrations", []string{"1_a.sql"}, diag.SeverityWarning)
		if d.Len() != 1 || d.All()[0].Severity != diag.SeverityWarning {
			t.Fatalf("want one warning, got: %s", d.String())
		}
	})
}

func TestCheckDuplicates(t *testing.T) {
	t.Parallel()

	t.Run("distinct versions are silent", func(t *testing.T) {
		t.Parallel()
		d := migcheck.CheckDuplicates("migrations", []string{"00001_a.sql", "00002_b.sql"})
		if d.Len() != 0 {
			t.Fatalf("got %d diagnostics, want 0: %s", d.Len(), d.String())
		}
	})

	// The case the lenient parse exists for: two files rig's naming rule would
	// judge differently are one version to goose.
	t.Run("padding does not separate two files", func(t *testing.T) {
		t.Parallel()
		d := migcheck.CheckDuplicates("migrations", []string{"00025_a.sql", "25_b.sql"})
		if d.Len() != 1 {
			t.Fatalf("got %d diagnostics, want 1: %s", d.Len(), d.String())
		}
		got := d.All()[0]
		if got.Code.ID != "RIG6051" {
			t.Errorf("code = %s, want RIG6051", got.Code.ID)
		}
		if !strings.Contains(got.Message, "version 25") {
			t.Errorf("message does not name version 25: %s", got.Message)
		}
		for _, name := range []string{"00025_a.sql", "25_b.sql"} {
			if !strings.Contains(got.Message, name) {
				t.Errorf("message does not name %s: %s", name, got.Message)
			}
		}
	})

	// One mistake, one diagnostic. Anchoring each claimant would report a single
	// collision three times and bury whatever else the run found.
	t.Run("three files on one version are one diagnostic", func(t *testing.T) {
		t.Parallel()
		d := migcheck.CheckDuplicates("migrations",
			[]string{"00003_a.sql", "00003_b.sql", "3_c.sql"})
		if d.Len() != 1 {
			t.Fatalf("got %d diagnostics, want 1: %s", d.Len(), d.String())
		}
		if !strings.Contains(d.All()[0].Message, "3 files") {
			t.Errorf("message does not count the files: %s", d.All()[0].Message)
		}
	})

	t.Run("non-migrations cannot collide", func(t *testing.T) {
		t.Parallel()
		d := migcheck.CheckDuplicates("migrations", []string{"README.md", "notes.md"})
		if d.Len() != 0 {
			t.Fatalf("got %d diagnostics, want 0: %s", d.Len(), d.String())
		}
	})
}

func TestCheckOutOfOrder(t *testing.T) {
	t.Parallel()

	t.Run("above the base max passes", func(t *testing.T) {
		t.Parallel()
		d := migcheck.CheckOutOfOrder([]string{"migrations/00027_ok.sql"}, "origin/main", 26)
		if d.Len() != 0 {
			t.Fatalf("got %d diagnostics, want 0: %s", d.Len(), d.String())
		}
	})

	t.Run("below the base max fails and suggests the next number", func(t *testing.T) {
		t.Parallel()
		d := migcheck.CheckOutOfOrder([]string{"migrations/00025_late.sql"}, "origin/main", 26)
		if d.Len() != 1 {
			t.Fatalf("got %d diagnostics, want 1: %s", d.Len(), d.String())
		}
		got := d.All()[0]
		if got.Code.ID != "RIG6052" {
			t.Errorf("code = %s, want RIG6052", got.Code.ID)
		}
		if !strings.Contains(got.Message, "27 or higher") {
			t.Errorf("message does not suggest 27: %s", got.Message)
		}
		if !strings.Contains(got.Message, "origin/main") {
			t.Errorf("message does not name the base ref: %s", got.Message)
		}
	})

	// Equal, not just below: a migration on the base's highest number is either
	// a duplicate of it or a renumbering of something already applied.
	t.Run("equal to the base max fails", func(t *testing.T) {
		t.Parallel()
		d := migcheck.CheckOutOfOrder([]string{"migrations/00026_same.sql"}, "origin/main", 26)
		if d.Len() != 1 {
			t.Fatalf("got %d diagnostics, want 1: %s", d.Len(), d.String())
		}
	})

	t.Run("a base with no migrations passes everything", func(t *testing.T) {
		t.Parallel()
		d := migcheck.CheckOutOfOrder([]string{"migrations/00001_init.sql"}, "origin/main", 0)
		if d.Len() != 0 {
			t.Fatalf("got %d diagnostics, want 0: %s", d.Len(), d.String())
		}
	})

	t.Run("padding does not excuse an out-of-order version", func(t *testing.T) {
		t.Parallel()
		d := migcheck.CheckOutOfOrder([]string{"migrations/25_late.sql"}, "origin/main", 26)
		if d.Len() != 1 {
			t.Fatalf("got %d diagnostics, want 1: %s", d.Len(), d.String())
		}
	})

	t.Run("the anchor is the added file", func(t *testing.T) {
		t.Parallel()
		d := migcheck.CheckOutOfOrder([]string{"db/migrations/00001_late.sql"}, "main", 9)
		if got := d.All()[0].Anchor.File; got != "db/migrations/00001_late.sql" {
			t.Errorf("anchor = %q, want db/migrations/00001_late.sql", got)
		}
	})
}

// The caller's slice is not reordered under it, and the diagnostics come out in
// a fixed order whatever the directory read returned.
func TestChecksDoNotReorderTheirInput(t *testing.T) {
	t.Parallel()

	names := []string{"00003_c.sql", "00001_a.sql", "00002_b.sql"}
	migcheck.CheckNames("migrations", names, diag.SeverityError)
	migcheck.CheckDuplicates("migrations", names)
	if names[0] != "00003_c.sql" {
		t.Errorf("input was reordered: %v", names)
	}
}
