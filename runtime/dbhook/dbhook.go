// Package dbhook is what a write carries besides the values it writes.
//
// A generated repository knows how to insert a row. It cannot know that
// creating a lesson should also reserve a room, that changing a price needs a
// second approval, or that a deletion should tell another service. Those belong
// to the application, and they have to run in the same transaction as the write
// — a hook that fires after a commit that later rolls back has told the world
// about something that did not happen.
//
// So every generated write takes one of the envelopes here: the input, the
// rules that check it, and the callbacks that run around it. The alternative is
// a repository method with seven parameters, six of which are almost always
// nil.
package dbhook

import (
	"context"

	"github.com/simonjanss/rig/runtime/tenancy"
)

// CreateValidator is the rule set a create is checked against.
//
// The generated model validator implements it. The generated static checks —
// NOT NULL, lengths, enum membership — run first and separately: this is where
// what the schema cannot express goes.
type CreateValidator[In any] interface {
	RunCreate(ctx context.Context, claims tenancy.Claims, in *In) error
}

// UpdateValidator is the rule set an update is checked against.
//
// It sees the row as it was as well as the change, because a rule about a
// transition — a status that may only move forward — needs both.
type UpdateValidator[In any, Out any] interface {
	RunUpdate(ctx context.Context, claims tenancy.Claims, in *In, prev *Out) error
}

// CreateHooks is everything the service layer supplies about a create, in the
// order it runs.
//
// The validator first, then Before and After inside the write's transaction —
// returning an error from either rolls the write back, which is the point of
// them being here rather than in the caller — then AfterCommit once the
// outermost transaction has committed, which is the only safe place to tell
// anything outside the database.
//
// A create is the one operation where the validator precedes Before. There is
// no existing row to read, so nothing has opened a transaction yet, and holding
// one across a rule that may call out to another service is a worse trade than
// the ordering buys. An update and a restore run their hook first, so that what
// it sets is what the rules judge.
type CreateHooks[In any, Out any] struct {
	// Validator holds the rules the schema cannot express. The generated
	// checks run first and separately, so a rule here is about the business
	// rather than about NOT NULL.
	Validator CreateValidator[In]

	// Before runs after validation and before the INSERT. Changes it makes to
	// the input are written.
	Before func(ctx context.Context, claims tenancy.Claims, in *In) error

	// After runs after the INSERT with the row as it was stored, including
	// everything the database filled in.
	After func(ctx context.Context, claims tenancy.Claims, created *Out) error

	// AfterCommit runs once the work has actually landed. It returns nothing:
	// there is no longer anything to fail, and a panic is contained rather than
	// allowed to unwind a request that succeeded.
	AfterCommit func(ctx context.Context, claims tenancy.Claims, created *Out)
}

// Create is one create: what to write, and everything that happens around it.
type Create[In any, Out any] struct {
	Input In
	Hooks CreateHooks[In, Out]
}

// UpdateHooks is everything the service layer supplies about an update, in the
// order it runs. prev is the row before the change and is read-only; modifying
// it changes nothing.
type UpdateHooks[In any, Out any] struct {
	// Validator holds the rules the schema cannot express. It is a different
	// type from the one a create takes: whether a row may exist is not whether
	// it may change, and an update cannot touch an immutable column at all.
	//
	// It runs after Before, so what it judges is the row as it will be written
	// rather than as it arrived.
	Validator UpdateValidator[In, Out]

	// Before runs first, before validation and before the UPDATE. Changes it
	// makes to the input are written — including to fields the request did not
	// mention, since setting a patch is what makes a column part of the
	// statement — and are then judged by the rules like anything else.
	Before func(ctx context.Context, claims tenancy.Claims, in *In, prev *Out) error

	// After runs after the UPDATE, with the new row and the old one.
	After func(ctx context.Context, claims tenancy.Claims, updated, prev *Out) error

	// AfterCommit runs once the change has landed.
	AfterCommit func(ctx context.Context, claims tenancy.Claims, updated, prev *Out)
}

// Update is one update.
type Update[In any, Out any] struct {
	Input In
	Hooks UpdateHooks[In, Out]
}

// DeleteHooks run around one delete, soft or hard.
//
// There is no validator: a delete carries an identifier and a flag, and there
// is nothing about it for a field rule to check. A reason to refuse the
// deletion goes in Before, which can return an error.
type DeleteHooks[In any, Out any] struct {
	// Before runs after the row is read and before it is retired or removed.
	Before func(ctx context.Context, claims tenancy.Claims, in *In, prev *Out) error

	// After runs after the delete, with the row as it was.
	After func(ctx context.Context, claims tenancy.Claims, prev *Out) error

	// AfterCommit runs once the deletion has landed.
	AfterCommit func(ctx context.Context, claims tenancy.Claims, prev *Out)
}

// Delete is one delete.
type Delete[In any, Out any] struct {
	Input In
	Hooks DeleteHooks[In, Out]
}

// RestoreValidator is the rule set a restore is checked against.
//
// It is the update validator, and the generated one satisfies both: a restore
// is a row re-entering the live set, possibly with changes, and the rules about
// what a live row may look like do not depend on how it got there.
//
// What differs is what counts as changed. On an update a rule may skip a field
// the request did not mention, because its value is already live and already
// passed. On a restore nothing is live — every value is entering the set — so
// every rule runs. That is the difference between RunRestore and RunUpdate, and
// the only one.
type RestoreValidator[In any, Out any] interface {
	RunRestore(ctx context.Context, claims tenancy.Claims, in *In, prev *Out) error
}

// RestoreHooks run around one restore.
//
// The input is the update input, and it does not come from the caller: a
// restore request carries an identifier and nothing else. It is there for
// Before to fill in, because the world may have moved on while the row was
// retired — the value that made it unique can have been taken by something
// created since, and the way back is either to refuse or to bring the row back
// under a different one.
type RestoreHooks[In any, Out any] struct {
	// Validator holds the rules the row has to satisfy to be live again. It
	// runs on whatever Before settled on.
	Validator RestoreValidator[In, Out]

	// Before is where a restore's values come from.
	//
	// It is handed the row as it was retired and an empty input. Setting a
	// field on that input writes it as the row comes back, which is how a value
	// the world has taken since gets changed on the way in:
	//
	//	if taken { in.Title = patch.NewOptional(prev.Title + " (restored)") }
	//
	// Returning an error refuses the restore instead. Which of the two to do is
	// the application's call, which is why this is a hook and not a rule.
	Before func(ctx context.Context, claims tenancy.Claims, in *In, prev *Out) error

	// After runs after the restore, with the live row and the state it was
	// restored from.
	After func(ctx context.Context, claims tenancy.Claims, restored, prev *Out) error

	// AfterCommit runs once the restore has landed.
	AfterCommit func(ctx context.Context, claims tenancy.Claims, restored, prev *Out)
}

// ReadHooks are what the service layer supplies about reading.
//
// Reads take a hook set rather than an envelope because there is nothing to put
// in one: a read carries a filter or an identifier and no input to validate.
//
// They run in the generated service, not in the repository. That is deliberate:
// a write reads the row it is about to change, and a hook that redacted or
// narrowed *that* read would have the update judge a row that is not the stored
// one, and snapshot it. These shape what an answer looks like on the way out,
// which is a different job from deciding what exists.
//
// Their claims are a pointer, and every other hook set takes a value. That is
// the difference between the two halves of the API and not an oversight: a
// write cannot happen without a caller — the repository refuses before a hook
// of any kind runs — while a read marked public in the table configuration is
// reached by somebody who presented nothing. Nil is that somebody. Handing a
// zero-valued Claims instead would be a tenant of all zeroes that reads like a
// real one.
type ReadHooks[Filter any, Out any] struct {
	// Narrow returns conditions every filtered read is limited to, or nil for
	// none.
	//
	// It returns a filter rather than editing the caller's, and the difference
	// matters: the two are combined with AND, so a caller whose own filter is
	// an OR cannot widen its way out of this one. Editing theirs in place could
	// not promise that.
	//
	// Get is not filtered — it fetches by primary key — so this does not apply
	// to it. Nor does it apply to the read a write makes to find its row: a
	// rule about which rows exist for a caller at all is what the tenant column
	// is for, and this is a view on top of that.
	Narrow func(ctx context.Context, claims *tenancy.Claims) (*Filter, error)

	// Rows runs on what a read is about to answer with, whether that is a page
	// of them or the single row a Get found.
	//
	// One call per read rather than one per row, so a hook that has to look
	// something up can do it once. Returning an error fails the read.
	Rows func(ctx context.Context, claims *tenancy.Claims, rows []*Out) error
}

// Restore is one restore.
type Restore[In any, Out any] struct {
	Input In
	Hooks RestoreHooks[In, Out]
}
