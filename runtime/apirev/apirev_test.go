package apirev_test

import (
	"testing"
	"time"

	"github.com/simonjanss/rig/runtime/apirev"
)

func TestParseReadsADate(t *testing.T) {
	t.Parallel()

	rev, ok := apirev.Parse("2026-04-30")
	if !ok {
		t.Fatal("a date did not parse")
	}
	if !rev.Known() {
		t.Error("a parsed revision reports itself unknown")
	}
	if rev.String() != "2026-04-30" {
		t.Errorf("String = %q, want the date back", rev)
	}
	if got := rev.Time(); !got.Equal(time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("Time = %s, want midnight UTC", got)
	}
}

// Nothing and nonsense are the same answer: both mean this caller cannot be
// placed on the timeline.
func TestParseRefusesWhatIsNotADate(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"", "yesterday-ish", "2026-04", "2026-04-30T00:00:00Z", "30/04/2026"} {
		if rev, ok := apirev.Parse(in); ok {
			t.Errorf("Parse(%q) = %s, want it refused", in, rev)
		}
	}
}

// A cutoff is a constant, and a typo in one is a bug rather than bad input. The
// panic is what makes it a startup failure instead of a shim that never fires.
func TestMustParsePanicsOnATypo(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Fatal("MustParse accepted something that is not a date")
		}
	}()
	apirev.MustParse("2026-04-3O") // a letter O, which is the typo worth catching
}

func TestMustParseReturnsTheRevision(t *testing.T) {
	t.Parallel()

	if got := apirev.MustParse("2026-04-30").String(); got != "2026-04-30" {
		t.Errorf("MustParse = %q, want the date back", got)
	}
}

func TestBeforeOrdersTwoRevisions(t *testing.T) {
	t.Parallel()

	older := apirev.MustParse("2026-03-01")
	newer := apirev.MustParse("2026-04-30")

	if !older.Before(newer) {
		t.Error("an older revision does not report itself before a newer one")
	}
	if newer.Before(older) {
		t.Error("a newer revision reports itself before an older one")
	}
	if older.Before(older) {
		t.Error("a revision reports itself before itself")
	}
}

// The safety property the whole type exists for: a caller nobody can place is
// served the current behavior, and a server with no minimum refuses nobody.
// Neither needs the caller to remember a check.
func TestAnUnknownRevisionIsBeforeNothing(t *testing.T) {
	t.Parallel()

	var unknown apirev.Revision
	known := apirev.MustParse("2026-04-30")

	if unknown.Known() {
		t.Error("the zero value reports itself known")
	}
	if unknown.Before(known) {
		t.Error("an unknown revision reports itself older than a known one")
	}
	if known.Before(unknown) {
		t.Error("a known revision reports itself older than an unknown one")
	}
	if unknown.String() != "" {
		t.Errorf("String = %q, want nothing for an unknown revision", unknown)
	}
}

func TestSubMeasuresTheGap(t *testing.T) {
	t.Parallel()

	older := apirev.MustParse("2026-03-01")
	newer := apirev.MustParse("2026-04-30")

	if got, want := newer.Sub(older), 60*24*time.Hour; got != want {
		t.Errorf("Sub = %s, want %s", got, want)
	}
	if got := older.Sub(newer); got != -60*24*time.Hour {
		t.Errorf("the gap the other way = %s, want it negative", got)
	}
	if got := newer.Sub(apirev.Revision{}); got != 0 {
		t.Errorf("Sub against an unknown revision = %s, want zero", got)
	}
}
