package note

import (
	"context"

	"github.com/simonjanss/rig/runtime/tenancy"

	"github.com/simonjanss/rig/examples/auth/internal/api"
	"github.com/simonjanss/rig/examples/auth/internal/model"
	"github.com/simonjanss/rig/examples/auth/internal/store"
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
func New(repo store.NoteRepository) api.DefaultNoteService {
	return api.NewNoteService(repo, &rules{repo: repo})
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
			Before:      s.mayWrite,
			After:       nil,
			AfterCommit: nil,
		},
		Update: dbhook.UpdateHooks[model.NoteUpdateInput, model.Note]{
			Validator: model.NoteUpdateValidator{
				Title:  s.validateTitle,
				Body:   nil,
				Entity: nil,
			},
			Before:      s.mayChange,
			After:       nil,
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
