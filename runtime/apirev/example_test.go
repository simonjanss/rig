package apirev_test

import (
	"fmt"

	"github.com/simonjanss/rig/runtime/apirev"
)

// notesAdded is how a compatibility shim declares its cutoff: a package-level
// var, so a mistyped date panics at startup rather than becoming a branch that
// silently never runs.
var notesAdded = apirev.MustParse("2026-04-30")

// A service reads the revision a caller was built against off the request and
// decides what to send it. Callers older than the cutoff get the old shape.
func ExampleRevision_Before() {
	for _, header := range []string{"2026-01-15", "2026-08-01", ""} {
		rev, ok := apirev.Parse(header)

		fmt.Printf("%-12q parsed=%-5t old=%t\n", header, ok, rev.Before(notesAdded))
	}

	// Output:
	// "2026-01-15" parsed=true  old=true
	// "2026-08-01" parsed=true  old=false
	// ""           parsed=false old=false
}

// An unknown revision reads as current, which is the safety property: a curl or
// a hand-rolled client sends nothing, and nothing should not select a
// compatibility path that only an old generated client needs.
func ExampleParse() {
	rev, ok := apirev.Parse("not a date")

	fmt.Println(ok, rev.Known())
	fmt.Printf("%q\n", rev.String())

	// Output:
	// false false
	// ""
}

// Sub is in whole days, because a revision is a date — "how many releases
// behind is this caller" has never been an hourly question.
func ExampleRevision_Sub() {
	old := apirev.MustParse("2026-04-30")
	recent := apirev.MustParse("2026-05-10")

	fmt.Println(recent.Sub(old))
	fmt.Println(old.Sub(recent))

	// Output:
	// 240h0m0s
	// -240h0m0s
}
