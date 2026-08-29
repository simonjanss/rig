// Package apirev is the API revision as a value.
//
// The generated server reads a revision off every request and the generated
// client sends one, both as a string. This is what that string becomes once
// somebody wants to compare it: the date an API surface was cut, and the one
// question worth asking about it — was the caller built before some point.
//
// Nothing here is per project, which is why it is in the runtime rather than
// generated into every `api` package. A compatibility shim written against it
// reads the same in every service in every project.
//
// The type exists mostly so that a date written down in source cannot be wrong
// in a way nobody notices. [MustParse] is what a shim declares its cutoff with,
// and it panics: a mistyped date in a `var` initializer stops the process
// before it serves anything, rather than becoming a branch that silently never
// runs.
package apirev

import (
	"fmt"
	"time"
)

// Layout is how a revision is written.
//
// A date and nothing finer: the question a revision answers is "how many
// releases behind is this client", and an hour has never been the unit of that
// answer.
const Layout = "2006-01-02"

// Revision is a point on an API's own timeline.
//
// The zero value is unknown, and unknown is an ordinary thing for a caller to
// be: a curl sends no revision, a hand-rolled client sends no revision, and
// anything sending nonsense has said nothing useful. Every comparison here
// reads unknown as current — see [Revision.Before] — so a caller rig cannot
// place on the timeline gets today's behavior rather than a compatibility path
// nobody can show it needs.
type Revision struct {
	day   time.Time
	known bool
	// text is [Revision.String]'s answer, rendered once where the revision is
	// made rather than at each call. A server announces its own on the way out
	// of every response, so this is a formatting of the same date per request
	// otherwise — and the value cannot change, because nothing outside [Parse]
	// builds one.
	text string
}

// MustParse is for a revision written down in source: a compatibility shim's
// cutoff, or a [Revision] a server refuses below.
//
//	var notesAdded = apirev.MustParse("2026-04-30")
//
// It panics on anything that is not a date. That is the point of it existing
// beside [Parse]: a cutoff is a constant, a typo in one is a bug rather than
// bad input, and a shim that quietly never fires is the worst way to find out.
// Declared at package scope, the panic lands at startup.
func MustParse(date string) Revision {
	rev, ok := Parse(date)
	if !ok {
		panic(fmt.Sprintf("apirev: %q is not a revision; want a date like 2026-04-30", date))
	}
	return rev
}

// Parse is for a revision that arrived from somewhere — a request header, a
// configuration file — where being wrong is ordinary rather than a bug.
//
// ok is false for a string that is not a date, and equally for the empty
// string. They are the same answer on purpose: both mean this caller cannot be
// placed on the timeline, and splitting them would invite a caller to be
// treated as old for saying nothing.
func Parse(date string) (Revision, bool) {
	day, err := time.Parse(Layout, date)
	if err != nil {
		return Revision{}, false
	}
	// Rendered rather than kept, because the two are not the same string: Go's
	// parser accepts `2026-4-30` for this layout and the revision that travels is
	// always `2026-04-30`.
	return Revision{day: day, known: true, text: day.Format(Layout)}, true
}

// Known reports whether this is a revision at all.
//
// It is the honest form of the question [Revision.Before] answers safely: an
// unknown revision compares as current, so code that needs to count the callers
// it cannot place has to ask directly.
func (r Revision) Known() bool { return r.known }

// Before reports whether r was cut before other.
//
// False when either side is unknown, which is the whole safety property: a
// shim guarded by `rev.Before(cutoff)` does not fire for a caller that said
// nothing, and a server whose minimum is unset refuses nobody. Neither case
// needs the caller to remember a check.
func (r Revision) Before(other Revision) bool {
	if !r.known || !other.known {
		return false
	}
	return r.day.Before(other.day)
}

// Sub is how much older r is than other, or a negative duration the other way
// around. Zero when either side is unknown.
//
// The unit is whole days, because [Layout] is.
func (r Revision) Sub(other Revision) time.Duration {
	if !r.known || !other.known {
		return 0
	}
	return r.day.Sub(other.day)
}

// Time is the revision as an instant, at midnight UTC. The zero [time.Time] for
// an unknown revision.
func (r Revision) Time() time.Time { return r.day }

// String is the revision as it travels, and empty for an unknown one — so a log
// line that prints it says nothing rather than saying the wrong year.
//
// Rendered when the revision was parsed rather than here. It is on the way out
// of every response a generated server writes, and a date formatted per request
// is an allocation per request for an answer that cannot change.
func (r Revision) String() string { return r.text }
