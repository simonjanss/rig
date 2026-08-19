package note

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/runtime/tenancy"

	"github.com/simonjanss/rig/examples/auth/internal/api"
	"github.com/simonjanss/rig/examples/auth/internal/model"
	"github.com/simonjanss/rig/examples/auth/internal/store"
	"github.com/simonjanss/rig/notify"
	"github.com/simonjanss/rig/runtime/dbhook"
)

// rules is Note's business logic.
//
// It describes itself and nothing else: the hooks it wants, the endpoints the
// configuration declared, and the writer it is handed in return. Nothing here
// mentions the service — that is what makes New one line.
//
// A rule about a field goes in the validator, something that has to happen
// with a write goes in a hook, and an endpoint rig cannot write is a method
// here.
//
// Unlike the .gen.go files, this one is yours: rig writes it once and never
// touches it again.
type rules struct {
	repo store.NoteRepository
	// notify is how a note says that something happened. One line in a hook,
	// with no recipients and no time: NotifyAt below says when, NotifyWho says
	// who, and the engine does everything in between.
	notify *notify.Service
	// accounts answers the audience. It is the plain pool rather than a
	// repository because rig_account belongs to the auth module and this example
	// generates nothing for it.
	accounts *pgxpool.Pool
	// write performs a write with the hooks below already attached. Use it rather
	// than the repository: reaching for the repository means passing the hooks by
	// hand, and forgetting once is a second way into the table where the rules do
	// not run.
	write api.NoteWriter
}

// rules satisfies what the constructor asks for. The check is here so that a
// new endpoint in the configuration becomes a compile error rather than a
// route that answers 501 at runtime.
var _ api.NoteRules = (*rules)(nil)

// New builds the service.
//
// To override a generated operation, wrap what this returns and shadow the
// promoted method:
//
// type Service struct{ api.DefaultNoteService } func (s *Service) Get(ctx
// context.Context, r api.Request[…]) (…) { … }
//
// The custom endpoints keep working through the value inside it, so only what
// you shadow changes.
func New(repo store.NoteRepository, notifier *notify.Service, pool *pgxpool.Pool) api.DefaultNoteService {
	return api.NewNoteService(repo, &rules{repo: repo, notify: notifier, accounts: pool})
}

// Bind receives the writer built from the hooks below. rig calls it once,
// during construction.
func (s *rules) Bind(w api.NoteWriter) { s.write = w }

// Hooks is everything about Note that the schema cannot describe, in the order
// it runs: the rules, then Before and After inside the transaction —
// returning an error from either undoes the write — then AfterCommit once it
// has landed, which is the only safe place to tell anything outside the
// database.
//
// The rules are one function per field, against the row the request would
// produce. Two sets, because whether a row may exist is not whether it may
// change: an update has no entry for a column it cannot touch, and a create
// none for one it cannot set.
//
// It is asked for rather than set, so there is no way to end up with a service
// whose rules were never attached. An empty one is a fine answer; it is just
// an answer.
func (s *rules) Hooks() api.NoteHooks {
	return api.NoteHooks{
		Read: dbhook.ReadHooks[model.NoteFilter, model.Note]{
			Narrow: nil,
			Rows:   nil,
		},
		Create: dbhook.CreateHooks[model.NoteCreateInput, model.Note]{
			Validator: model.NoteCreateValidator{
				Title:  s.validateTitle,
				Body:   nil,
				Entity: nil,
			},
			Before: s.mayWrite,
			// After rather than AfterCommit, and that is deliberate: the
			// notification row is part of the change, so a change that committed
			// without it is a notification nobody will ever send. It is the
			// argument dbhook makes about the other direction, read backwards.
			After:       s.announce,
			AfterCommit: nil,
		},
		Update: dbhook.UpdateHooks[model.NoteUpdateInput, model.Note]{
			Validator: model.NoteUpdateValidator{
				Title:  s.validateTitle,
				Body:   nil,
				Entity: nil,
			},
			Before: s.mayChange,
			// A publish_at that moved takes its notifications with it, and one
			// that was cleared cancels them. Neither is a rule written here:
			// Reschedule asks NotifyAt again, which is why it is asked after
			// every update rather than once.
			After:       s.rescheduleAnnouncements,
			AfterCommit: nil,
		},
		Delete: dbhook.DeleteHooks[model.NoteDeleteInput, model.Note]{
			Before:      s.mayRemove,
			After:       nil,
			AfterCommit: nil,
		},
		Restore: dbhook.RestoreHooks[model.NoteUpdateInput, model.Note]{
			Validator: model.NoteUpdateValidator{
				Title:  s.validateTitle,
				Body:   nil,
				Entity: nil,
			},
			Before:      s.mayChange,
			After:       nil,
			AfterCommit: nil,
		},
	}
}

// PermissionWrite is what a caller needs to change a note.
//
// The name belongs to the application — a check only compares strings — and it is
// declared here rather than in main because this is the code that enforces it.
const PermissionWrite = "note.write"

// PermissionRead is what a caller needs to read notes at all, and PermissionReadAll
// is what widens that from its own rows to the whole tenant through ?scope=all.
//
// Both derived by rig — the first from the table being readable, the second from it
// declaring `access: { scope: own }` — so these constants name keys the generated
// handlers already check rather than inventing any. They are separate grants and
// the wide one is additional: a credential holding only PermissionReadAll could not
// read anything, because the endpoint's own check comes first.
const (
	PermissionRead    = "note.read"
	PermissionReadAll = "note.read.all"
)

// mayWrite refuses a caller without the permission.
//
// A hook rather than middleware, and that is the point worth taking from this
// example: the check runs inside the transaction that does the write, on the
// claims the request arrived with, so every path to a write goes through it —
// the generated endpoint, a custom endpoint, and anything the service layer calls
// itself. A route-level check only covers the route.
//
// 403 rather than 404: the caller is known and simply not allowed. A row in
// another tenant is the 404 case, and the repository handles that one.
func (s *rules) mayWrite(_ context.Context, claims tenancy.Claims, _ *model.NoteCreateInput) error {
	return tenancy.Require(claims, PermissionWrite)
}

// mayChange is the same rule for an update or a restore, and mayRemove for a
// delete. The signatures differ because what each write is handed differs; the
// rule does not.
func (s *rules) mayChange(_ context.Context, claims tenancy.Claims, _ *model.NoteUpdateInput, _ *model.Note) error {
	return tenancy.Require(claims, PermissionWrite)
}

func (s *rules) mayRemove(_ context.Context, claims tenancy.Claims, _ *model.NoteDeleteInput, _ *model.Note) error {
	return tenancy.Require(claims, PermissionWrite)
}

// validateTitle is an example rule. Delete it, and set Title back to nil in
// business, if Note needs none.
//
// A rule that needs something reaches it through s, which is why the rules are
// methods.
//
// Returning a FieldError attaches the message to title, so the client is
// answered with a 422 whose body names the field rather than a sentence it has
// to read. Returning any other error fails the request as that error.
func (s *rules) validateTitle(ctx context.Context, c *model.NoteValidatorContext, value string) error {
	// c.Values is the whole row as it will be, c.Previous() is how it was, and
	// c.TitleChanged() reports whether this request touched it — which is how an
	// expensive check is kept off every write.
	return nil
}

// NotifyAt says when notifications about a note are due.
//
// A draft has no publish_at, so nothing about it is due and returning false
// cancels whatever was still pending — which is what clearing the column has to
// mean, and why rig asks this after every update rather than once.
//
// A note that is already live returns the zero time, which means now: an
// immediate notification and one scheduled for Friday are the same row with a
// different column, and the same code sends both.
func (s *rules) NotifyAt(row *model.Note, _ string) (time.Time, bool) {
	if row.PublishAt == nil {
		return time.Time{}, false
	}
	return *row.PublishAt, true
}

// NotifyWho answers who should hear about a note, at the moment of sending.
//
// This example's answer is "everybody in the tenant with an active account",
// which is the shape most applications start from and the one a path expression
// would have covered. What a path expression could not cover is the sentence
// this becomes after the first month — everybody in the group, minus whoever
// muted the thread, plus the moderators if it was flagged, and not the author —
// and that is why this is a function.
//
// It runs in the dispatcher rather than in a request, so the answer is current
// rather than however old the note is: an account created after the note was
// written is in this list, because this list is built now.
func (s *rules) NotifyWho(ctx context.Context, n *notify.Notification, row *model.Note) ([]uuid.UUID, error) {
	// System claims for the note's own tenant, so this is scoped without
	// anybody threading a tenant through. Reading rig_account directly because
	// it belongs to the auth module: this example generates no repository for it.
	const q = `SELECT id FROM rig_account
		WHERE tenant_id = $1 AND deleted_at IS NULL AND is_active
		  AND id <> coalesce($2, '00000000-0000-0000-0000-000000000000'::uuid)`

	rows, err := s.accounts.Query(ctx, q, n.TenantID, row.CreatedByAccountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// KindNotePublished is what this application calls the event.
//
// A string, because rig cannot know a project's kinds. Narrowing
// rig_notification.kind to a Postgres enum of your own would make this a typed
// constant and a switch over kinds one the compiler can see — worth doing, and
// not made mandatory, because a foundation that shipped an empty enum type would
// be a worse start than a text column somebody narrows.
const KindNotePublished = "NotePublished"

// announce says that a note happened. It says nothing about who or when.
func (s *rules) announce(ctx context.Context, _ tenancy.Claims, row *model.Note) error {
	_, err := s.notify.Announce(ctx, notify.Announcement{
		Kind:    KindNotePublished,
		Subject: api.NotifyAboutNote(row.ID),
		// Several notifications about one note collapse into one inbox line
		// saying how many. Read the line and the next one starts fresh, which
		// falls out of a partial index rather than out of a rule here.
		Group: notify.GroupBySubject(api.NotifyAboutNote(row.ID)),
	})
	return err
}

// rescheduleAnnouncements moves what is pending about a note, or cancels it.
//
// One line, because the decision is NotifyAt's: this asks it again with the row
// as it now is.
func (s *rules) rescheduleAnnouncements(ctx context.Context, _ tenancy.Claims, row, _ *model.Note) error {
	at, due := s.NotifyAt(row, KindNotePublished)
	return s.notify.Reschedule(ctx, api.NotifyAboutNote(row.ID), at, due)
}
