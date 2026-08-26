// Package notify is the half of a notification that is the same in every
// project.
//
// Three statements, and the split between them is the whole design.
//
// **A service says a thing happened.** One line in a hook it already has, with
// no recipients and no time: [Service.Announce] takes what happened and what it
// happened to, and nothing else.
//
// **The table it happened to says when, and who.** Two generated methods on its
// rules interface, which is where an application's own knowledge lives.
//
// **This package does everything in between**: it holds the announcement until
// it is due, asks who should hear about it, and writes an inbox line per
// account. None of that is in anybody's service, because none of it differs
// between applications — and all of it is what applications get subtly wrong.
//
// # The audience is computed late, and that is the decision everything follows
//
// A pending notification carries no recipients at all. A post scheduled for
// Friday notifies whoever can read it on Friday, and somebody added to the group
// on Thursday evening is one of those people; a recipient list computed on
// Monday does not know that, and a system that captures one spends the rest of
// its life patching around it — a job that reconciles lists, a second table of
// pending additions, a support ticket that says "I never got the announcement".
//
// So the audience is computed at the moment of sending, by asking the table the
// notification is about. The cost is honest and worth stating: that question is
// answered in a background job, without a request and without a caller, long
// after the transaction that caused it committed. Which is why the code that
// answers it has to be reachable from a job — see [Registry].
//
// # Immediacy is one column
//
// A direct notification is not a special case. Its `deliver_at` is now(), the
// dispatcher is nudged when the transaction commits, and the audience is
// computed microseconds later. A scheduled one has a later `deliver_at` and the
// same code runs when it arrives. There is no fast path to keep in step with a
// slow one, which is what makes late resolution cheap rather than expensive:
// the interesting case — a scheduled notification whose audience changed — is
// the same code as the boring one.
//
// # What this package does not do
//
// No templates, no rendering, no localisation, and no title or body anywhere. A
// notification is a kind, a payload and a link to the row it is about; the
// sentence is the application's, for the reason the mail notifier already gives.
// A copy of a rendered string in the row is a copy that goes stale the day
// somebody rewords it.
package notify

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/runtime/rigerr"
)

// State is where a notification is in its life.
//
// Resolved means the audience was determined and the inbox lines exist. It does
// not mean anything was sent, which is the delivery table's business and not
// this one's.
type State string

// The three states, and the whole of the state machine.
//
// Pending means the audience has not been computed yet, which is where every
// notification starts and where a scheduled one waits. Resolved means it was
// computed and the inbox lines exist. Cancelled means it will not be: a
// publish_at that was cleared, a row that was retired before its time came.
//
// Nothing here says anything was sent. That is the delivery table's business and
// deliberately not this one's, because a notification with an inbox line and no
// mail is a working notification and a state machine that conflated them would
// have to lie about one of them.
const (
	StatePending   State = "Pending"
	StateResolved  State = "Resolved"
	StateCancelled State = "Cancelled"
)

// ErrNotFound is what a lookup answers for a notification or an inbox line that
// is not there, is deleted, or belongs to somebody else.
//
// One error for all three, deliberately. Distinguishing them would tell a caller
// that a row exists and is not theirs, which is exactly what the narrowing is
// for.
//
// A [rigerr.Error] rather than a bare sentinel, so it carries its own status
// wherever it surfaces. As a bare sentinel it read as CodeInternal to anything
// that classified it, and the 404 existed only in this package's own fallback
// error writer — which every generated project replaces. So dismissing a
// notification that was not there answered 500 in every real application.
// Carrying the code is what makes that unable to happen again: there is no
// mapping left for a caller to be missing.
//
// [errors.Is] against it still works, and the message a client sees is unchanged.
// What changed is [error.Error], which now reads `NotFound: no such
// notification` — the code is in front because [rigerr.Error] puts it there for
// the log.
var ErrNotFound = rigerr.NotFound("no such notification")

// Announcement is a service saying that something happened.
//
// It carries no recipients and no time. Both of those are answered later and by
// somebody else: the time by the subject's own NotifyAt, the audience by its
// NotifyWho at the moment of sending.
type Announcement struct {
	// Kind is what happened, as the application names it. A string here and the
	// project's own Go enum type when it narrows rig_notification.kind to a
	// Postgres enum of its own — worth doing for one reason, which is that a
	// switch over kinds inside NotifyWho becomes one the compiler can see.
	Kind string

	// Subject is the row this is about. A generated helper builds it, so the
	// table name is written by rig and never by a request.
	Subject Subject

	// Payload is what a template needs beyond the linked row. It is stored as
	// jsonb and handed back whole; give the column a Go type with the `go_type`
	// key if it has a shape.
	Payload json.RawMessage

	// At is when this is due, and Due is whether it is due at all.
	//
	// They come from the subject's own NotifyAt, called by the hook that
	// announces — where the row is in hand. The zero time with Due means now,
	// which is the ordinary case and is not a special path: an immediate
	// notification and one scheduled for Friday are the same row with a
	// different column.
	//
	// Due false writes the notification and cancels it. The row is still
	// written and still linked, because a notification that was decided against
	// is a thing that happened and something has to be able to say so.
	At  time.Time
	Due bool

	// Group collapses several events into one inbox line. Nil is one line per
	// event; [GroupBySubject] is the ordinary answer; [GroupBy] sets a coarser
	// key of your own.
	Group *string

	// AccountIDs skips NotifyWho for this announcement, and is the documented
	// exception to everything above.
	//
	// Some audiences genuinely cannot be re-derived: the five people who were
	// @-mentioned in a body that has since been edited. Without this, NotifyWho
	// would have to read a version row to reconstruct a list somebody already
	// had in their hand.
	//
	// It is named as the exception rather than offered as the parameter, because
	// a list captured at write time is a list that stops being true, and that is
	// what late resolution exists to prevent.
	AccountIDs []uuid.UUID
}

// GroupBySubject derives a collapse key from the row a notification is about, so
// ten comments on one post become one inbox line saying ten.
//
// This is the answer most of the time. The coarser one — everything in a thread,
// however many posts it spans — is [GroupBy].
func GroupBySubject(s Subject) *string {
	key := s.Table + ":" + s.ID.String()
	return &key
}

// GroupBy sets a collapse key of your own.
//
// Two announcements with the same kind and the same key, to the same person,
// become one line — until it is read, after which the next one starts a fresh
// line. That falls out of a partial index rather than out of a rule, so there is
// nothing to get wrong about when a group ends.
func GroupBy(key string) *string { return &key }

// Subject is the row a notification is about: a table, and an identifier in it.
//
// The table name is written by a generator from the compiled document and never
// by a request, which is what makes it safe for this package to build a
// statement around it — the same bargain the file owner makes.
type Subject struct {
	// Table is the subject's own table, not the join. The join's name is derived
	// from it.
	Table string
	// LinkTable is the join table pointing at rig_notification, and Column is
	// the subject's own key in it.
	LinkTable string
	Column    string

	ID uuid.UUID
}

// Notification is one row of rig_notification, as the engine holds it.
//
// There is no title and no body, and there never will be. Those are rendering:
// locale-dependent, template-shaped, and stale the day somebody rewords them.
// What rig knows is that something happened and what it happened to.
type Notification struct {
	ID       uuid.UUID
	TenantID uuid.UUID

	Kind    string
	State   State
	Payload json.RawMessage

	// DeliverAt is when this is due. It is the only difference between a
	// notification that goes out now and one scheduled for Friday.
	DeliverAt  time.Time
	ResolvedAt *time.Time
	CreatedAt  time.Time

	// GroupKey is what will collapse this into an existing inbox line, decided
	// when the announcement was written because that is where the subject was
	// in hand, and read when the audience is resolved.
	GroupKey *string

	// AccountIDs is a list captured at write time, and empty in the ordinary
	// case. See [Announcement.AccountIDs] for why the exception exists and why
	// it is an exception.
	AccountIDs []uuid.UUID
}

// Recipient is one inbox line.
//
// Kind is copied from the notification rather than read through the join, and
// that is not denormalization for speed: it is what lets the collapse index and
// the live-sync shape work without the inbox touching rig_notification at all —
// a table holding rows for people who are not recipients yet and may never be.
type Recipient struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	NotificationID uuid.UUID
	AccountID      uuid.UUID

	Kind     string
	GroupKey *string
	// EventCount is how many events this line stands for. One, unless a group
	// key collapsed them.
	EventCount int
	ReadAt     *time.Time

	CreatedAt time.Time
	DeletedAt *time.Time
}

// Read reports whether the person has seen it. What the badge counts is the
// rest.
func (r *Recipient) Read() bool { return r.ReadAt != nil }
