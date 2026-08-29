package bookmark

import (
	"context"

	"github.com/simonjanss/rig/examples/auth_oauth/internal/api"
	"github.com/simonjanss/rig/examples/auth_oauth/internal/model"
	"github.com/simonjanss/rig/examples/auth_oauth/internal/store"
	"github.com/simonjanss/rig/runtime/dbhook"
)

// rules is Bookmark's business logic.
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
	repo store.BookmarkRepository
	// write performs a write with the hooks below already attached. Use it rather
	// than the repository: reaching for the repository means passing the hooks by
	// hand, and forgetting once is a second way into the table where the rules do
	// not run.
	write api.BookmarkWriter
}

// rules satisfies what the constructor asks for. The check is here so that a
// new endpoint in the configuration becomes a compile error rather than a
// route that answers 501 at runtime.
var _ api.BookmarkRules = (*rules)(nil)

// New builds the service.
//
// To override a generated operation, wrap what this returns and shadow the
// promoted method:
//
// type Service struct{ api.DefaultBookmarkService } func (s *Service) Get(ctx
// context.Context, r api.Request[…]) (…) { … }
//
// The custom endpoints keep working through the value inside it, so only what
// you shadow changes.
func New(repo store.BookmarkRepository) api.DefaultBookmarkService {
	return api.NewBookmarkService(repo, &rules{repo: repo})
}

// Bind receives the writer built from the hooks below. rig calls it once,
// during construction.
func (s *rules) Bind(w api.BookmarkWriter) { s.write = w }

// Hooks is everything about Bookmark that the schema cannot describe, in the
// order it runs: the rules, then Before and After inside the transaction —
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
func (s *rules) Hooks() api.BookmarkHooks {
	return api.BookmarkHooks{
		Read: dbhook.ReadHooks[model.BookmarkFilter, model.Bookmark]{
			Narrow: nil,
			Rows:   nil,
		},
		Create: dbhook.CreateHooks[model.BookmarkCreateInput, model.Bookmark]{
			Validator: model.BookmarkCreateValidator{
				Title:  s.validateTitle,
				URL:    nil,
				Entity: nil,
			},
			Before:      nil,
			After:       nil,
			AfterCommit: nil,
		},
		Update: dbhook.UpdateHooks[model.BookmarkUpdateInput, model.Bookmark]{
			Validator: model.BookmarkUpdateValidator{
				Title:  s.validateTitle,
				URL:    nil,
				Entity: nil,
			},
			Before:      nil,
			After:       nil,
			AfterCommit: nil,
		},
		Delete: dbhook.DeleteHooks[model.BookmarkDeleteInput, model.Bookmark]{
			Before:      nil,
			After:       nil,
			AfterCommit: nil,
		},
	}
}

// validateTitle is an example rule. Delete it, and set Title back to nil in
// business, if Bookmark needs none.
//
// A rule that needs something reaches it through s, which is why the rules are
// methods.
//
// Returning a FieldError attaches the message to title, so the client is
// answered with a 422 whose body names the field rather than a sentence it has
// to read. Returning any other error fails the request as that error.
func (s *rules) validateTitle(ctx context.Context, c *model.BookmarkValidatorContext, value string) error {
	// c.Values is the whole row as it will be, c.Previous() is how it was, and
	// c.TitleChanged() reports whether this request touched it — which is how an
	// expensive check is kept off every write.
	return nil
}
