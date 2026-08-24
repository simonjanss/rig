// Package presence is who is here, and what they are looking at.
//
// A presence row is one browser tab: it says which account, in which tenant, is
// in which part of the application, on which row of which table, in which field.
// A client rewrites its own row every twenty seconds or so; the row is streamed
// to every other client in the tenant over a live-sync shape; and it is deleted
// once nobody has heard from that tab in a while.
//
// # The row is not the answer to "is this person here"
//
// That is the one thing to understand about this package, and it is the opposite
// of how [github.com/simonjanss/rig/notify] works.
//
// A shape's filter is evaluated by the sync service when a row *changes*, so a
// predicate that moves on its own — `seen_at > now() - ttl` — would never fire
// again for a row that simply stopped being written. The row would sit in every
// subscriber's copy forever, filtered in appearance and not in fact. It is the
// same reason the trash shape has no restore window.
//
// So the freshness test belongs to whoever is reading, not to SQL:
//
//	Whoever is reading decides who is here. The sweeper decides how much of the
//	past a new subscriber has to download.
//
// A browser compares each row's SeenAt against the TTL on every tick and is
// correct within a second, for free, on the day the project was generated and
// before anybody wired a cron. [Sweeper] deletes what has expired, which keeps
// the table and every new subscriber's first fetch from carrying yesterday —
// and, because a DELETE is a change, converges every subscriber's copy for free.
// Skipping the sweep costs space and a slower first fetch and nothing else.
//
// [Service.Here] is the exception that proves the rule: a plain read is a moment
// rather than a subscription, so it can afford a moving predicate and applies the
// filter itself.
package presence

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Table is the managed table every presence row is in. Spelled once, here,
// because it is the only name in this package a migration also has to know.
const Table = "rig_presence"

// The defaults. Each is a number a project overrides in its `presence:` block
// rather than here, and each is the cost of the feature stated one way.
//
// DefaultTTL is a minute, which is a generous window for a ghost and affordable
// because an ordinary close sends a leave: the TTL only has to cover a crashed
// tab and a dead network. DefaultHeartbeat is a third of it, which is the floor
// a project's configuration is checked against — three beats before somebody
// vanishes, one for a pause, one for a slow network, one for the request that
// was actually lost. DefaultGrace keeps an expired row for five minutes after it
// stopped being drawn, so the reader's arithmetic and the sweeper's can never
// disagree in the direction that makes a row come back. DefaultSweep is how
// often the in-process sweeper looks, and it is housekeeping rather than a
// guarantee.
const (
	DefaultTTL       = time.Minute
	DefaultHeartbeat = 20 * time.Second
	DefaultGrace     = 5 * time.Minute
	DefaultSweep     = time.Minute
)

// MinTTL is the shortest window this package will start with.
//
// Under fifteen seconds presence flickers on an ordinary mobile connection, and
// what that presents as is a broken feature rather than a number somebody chose.
// Refused at construction for the reason notify refuses a short claim lease.
const MinTTL = 15 * time.Second

// Activity is whether somebody is looking or typing.
//
// It is separate from [Target.Field] because a client may know that somebody is
// editing before it knows which control has focus — a form with one text area
// and no per-field tracking is the ordinary case, and it should still be able to
// say more than "present".
type Activity string

// The activities. The Postgres labels are lower case and these are the values on
// the wire, so a client sends what the enum holds.
const (
	Viewing Activity = "viewing"
	Editing Activity = "editing"
)

// ParseActivity reads an activity from the wire, defaulting an empty string to
// [Viewing].
//
// Empty is not an error because "present, and not saying more than that" is the
// commonest thing a client means and should not need a keyword.
func ParseActivity(s string) (Activity, error) {
	switch Activity(s) {
	case "", Viewing:
		return Viewing, nil
	case Editing:
		return Editing, nil
	default:
		return "", fmt.Errorf("presence: %q is not an activity (%s or %s)", s, Viewing, Editing)
	}
}

// Target is what a presence is on: a table, a row in it, and a field of that
// row.
//
// All three are optional and they narrow in that order — a Table with no ID is
// somebody on a list rather than on one row, and an ID with no Field is somebody
// looking at a row rather than typing into it. The two combinations that skip a
// level are refused by the table's own check constraints, because an identifier
// with nothing to say which table it is in is one no reader can use.
type Target struct {
	Table string
	ID    uuid.UUID
	Field string
}

// Presence is one browser tab, and what it was last looking at.
//
// It is a fact about the last few seconds, which is why Target above is a table
// name and an identifier rather than a relation: nothing joins to one of these.
type Presence struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	AccountID  uuid.UUID
	SessionKey string
	Scope      string
	Target     Target
	Activity   Activity
	CreatedAt  time.Time
	// SeenAt is the last heartbeat. Whether this row means somebody is here is a
	// comparison against it, made by whoever is reading — see the package
	// documentation for why that is not decided in SQL.
	SeenAt time.Time
}

// Fresh reports whether this row still means somebody is here, measured against
// a clock the caller supplies.
//
// The clock is a parameter because the interesting caller is a browser, whose
// clock is not this one. A server-side caller passes its own now; the TypeScript
// package does the same arithmetic against the freshest SeenAt it can see, which
// is itself a reading of this clock and therefore cancels the skew.
func (p *Presence) Fresh(now time.Time, ttl time.Duration) bool {
	return p.SeenAt.After(now.Add(-ttl))
}
