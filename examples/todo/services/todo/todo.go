package todo

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/examples/todo/internal/api"
	"github.com/simonjanss/rig/examples/todo/internal/model"
	"github.com/simonjanss/rig/examples/todo/internal/store"
	"github.com/simonjanss/rig/files"
	"github.com/simonjanss/rig/runtime/dbhook"
	"github.com/simonjanss/rig/runtime/patch"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// rules is Todo's business logic.
//
// It describes itself and nothing else: the hooks it wants, the endpoints the
// configuration declared, and the writer it is handed in return. Nothing here
// mentions the service — that is what makes New one line.
//
// A rule about a field goes in the validator, something that has to happen with
// a write goes in a hook, and an endpoint rig cannot write is a method here.
//
// Unlike the .gen.go files, this one is yours: rig writes it once and never
// touches it again.
type rules struct {
	repo store.TodoRepository
	// write performs a write with the hooks below already attached. Use it
	// rather than the repository: reaching for the repository means passing the
	// hooks by hand, and forgetting once is a second way into the table where
	// the rules do not run.
	write    api.TodoWriter
	notifier Notifier
	logger   *slog.Logger
}

// Notifier is told about todos as they are created.
//
// It is an interface declared here rather than the concrete type from the
// notify package, so that this service depends on what it uses and a test can
// hand it something that only counts.
type Notifier interface {
	Record(message string)
}

// rules satisfies what the constructor asks for. The check is here so that a
// new endpoint in the configuration becomes a compile error rather than a route
// that answers 501 at runtime.
var _ api.TodoRules = (*rules)(nil)

// New builds the service.
//
// To override a generated operation, wrap what this returns and shadow the
// promoted method:
//
//	type Service struct{ api.DefaultTodoService }
//	func (s *rules) Get(ctx context.Context, r api.Request[…]) (…) { … }
//
// The custom endpoints keep working through the value inside it, so only what
// you shadow changes.
//
// The logger is the server's, so that what a rule or a hook says lands with
// everything else the process writes. Nil falls back to the default, the way
// serve.Config does, rather than panicking somewhere later.
// The file service is a parameter because todo has a cover_file_id, and a
// table with a file column has endpoints that cannot answer without one.
func New(repo store.TodoRepository, files *files.Service, notifier Notifier, logger *slog.Logger) api.DefaultTodoService {
	if logger == nil {
		logger = slog.Default()
	}
	return api.NewTodoService(repo, &rules{repo: repo, notifier: notifier, logger: logger}, files)
}

// Bind receives the writer built from the hooks below. rig calls it once,
// during construction.
func (s *rules) Bind(w api.TodoWriter) { s.write = w }

// Hooks is everything about Todo that the schema cannot describe.
//
// Every field is listed, nil included. Go does not require it, and that is the
// point: adding a column to the table shows up here as a field nobody filled
// in, rather than as nothing at all.
//
// It is asked for rather than set, so there is no way to end up with a service
// whose rules were never attached.
func (s *rules) Hooks() api.TodoHooks {
	return api.TodoHooks{
		// What a read answers with. Narrow adds a condition every filtered read
		// is limited to — ANDed with whatever the caller asked for, so a search
		// whose own filter is an OR cannot widen its way out of it. Rows sees
		// what is about to be returned, once per read rather than once per row.
		//
		// Both nil here: a todo is visible to its whole tenant, and the tenant
		// predicate is generated. A rule about which rows exist for a caller at
		// all belongs in a column, not here.
		Read: dbhook.ReadHooks[model.TodoFilter, model.Todo]{
			Narrow: nil,
			Rows:   nil,
		},

		// The same title rule on create and update, which is a choice rather
		// than the default: the two sets are separate so a rule can differ, or
		// apply to one and not the other. On an update the rule sees the title
		// the row would end up with, so a request that changes only the notes is
		// not judged on a title it never sent.
		Create: dbhook.CreateHooks[model.TodoCreateInput, model.Todo]{
			Validator: model.TodoCreateValidator{
				Title:    s.validateTitle,
				Notes:    nil,
				IsDone:   nil,
				Priority: nil,
				DueAt:    nil,
				Entity:   nil,
			},
			Before: nil,
			After:  nil,
			// The only place a write may be announced from: the row is committed
			// and nothing that happens here can take it back.
			AfterCommit: s.announceCreated,
		},
		Update: dbhook.UpdateHooks[model.TodoUpdateInput, model.Todo]{
			Validator: model.TodoUpdateValidator{
				Title:    s.validateTitle,
				Notes:    nil,
				IsDone:   nil,
				Priority: nil,
				DueAt:    nil,
				Entity:   nil,
			},
			Before:      nil,
			After:       nil,
			AfterCommit: nil,
		},
		// The deletion refuses in Before, inside the transaction, where
		// returning an error still undoes the write.
		Delete: dbhook.DeleteHooks[model.TodoDeleteInput, model.Todo]{
			Before:      s.beforeDelete,
			After:       nil,
			AfterCommit: nil,
		},
		// A restore carries no fields, so Before is where any come from. This
		// one renames rather than refuses; the validator then judges what it
		// settled on, with every rule running because none of the row was live
		// to have been checked.
		Restore: dbhook.RestoreHooks[model.TodoUpdateInput, model.Todo]{
			Validator: model.TodoUpdateValidator{
				Title:    s.validateTitle,
				Notes:    nil,
				IsDone:   nil,
				Priority: nil,
				DueAt:    nil,
				Entity:   nil,
			},
			Before:      s.beforeRestore,
			After:       nil,
			AfterCommit: nil,
		},
	}
}

// validateTitle refuses a placeholder title.
//
// The generated validation already refuses an empty one — the column is NOT
// NULL and the model checks it. This is the kind of rule the schema cannot
// express, and returning a FieldError is what puts the message under title in
// the 422 rather than in a sentence the client has to read. The code is what a
// client switches on; the message is what a person reads.
//
// Returning anything else would say the rule could not be run, which is a 500
// and not the caller's fault.
func (s *rules) validateTitle(ctx context.Context, c *model.TodoValidatorContext, title string) error {
	if strings.EqualFold(title, "untitled") {
		return rigerr.NewFieldError(rigerr.FieldCodeNotAllowed, "give the todo a real title")
	}

	// Nothing to check when the request did not touch the title: the only row
	// with that title is this one. This is what c.Changed is for — the check
	// below is a query, and running it on every update of every other field
	// would be a round trip to prove nothing.
	if c.IsUpdate() && !c.TitleChanged() {
		return nil
	}

	taken, err := s.titleTaken(ctx, c, title)
	if err != nil {
		// Not a field error, so it does not land under title: the caller's
		// input may be perfectly good and we simply could not tell.
		return err
	}
	if taken {
		return rigerr.NewFieldError(rigerr.FieldCodeAlreadyExists,
			"another todo already has that title")
	}
	return nil
}

// titleTaken reports whether another row already has the title.
//
// Another: on an update the row being changed matches itself, and refusing a
// request for conflicting with the thing it is changing would be absurd.
func (s *rules) titleTaken(ctx context.Context, c *model.TodoValidatorContext, title string) (bool, error) {
	self := uuid.Nil
	if c.IsUpdate() {
		self = c.Previous().ID
	}
	return s.titleHeldBy(ctx, title, self)
}

// titleHeldBy reports whether a live todo other than self already has the title.
//
// Live: List does not return retired rows, and that is deliberate rather than
// incidental. A title in the trash is a title going spare — refusing to reuse
// it for the thirty days the deleted row stays restorable would be a strange
// thing to explain to somebody.
//
// This is a check and then a write, so two requests racing can both pass it.
// The partial unique index is what actually prevents the duplicate; this is
// what turns the constraint violation into a message that names the field and
// says what to do about it.
func (s *rules) titleHeldBy(ctx context.Context, title string, self uuid.UUID) (bool, error) {
	filter := model.NewTodoFilter()
	filter.Equals = model.NewTodoFilterEquals()
	filter.Equals.Title = &title

	// Two, so that finding only itself is distinguishable from finding another.
	rows, _, err := s.repo.List(ctx, filter, model.TodoPage{Limit: 2})
	if err != nil {
		return false, err
	}

	for _, row := range rows {
		if row.ID != self {
			return true, nil
		}
	}
	return false, nil
}

// beforeDelete refuses to retire a task that is not finished.
//
// It is a hook rather than a validator rule because it is not about a field of
// the request: the request carries an identifier, and what makes it wrong is
// the state of the row it names. The hook is handed that row, so the check
// costs nothing — overriding Delete to look it up would read it a second time,
// and outside the transaction that is about to write.
func (s *rules) beforeDelete(_ context.Context, _ tenancy.Claims, _ *model.TodoDeleteInput, prev *model.Todo) error {
	if !prev.IsDone {
		return rigerr.Conflict("finish the todo before deleting it")
	}
	return nil
}

// beforeRestore gets a retired task past a title somebody else has taken.
//
// This is the gap a restore leaves open, and where the decision about it lives.
// Deleting a task frees its title; creating a new one with that title is
// allowed, and should be. Nothing has gone wrong — but the retired row still
// carries the old title, and bringing it back would put two live tasks under
// one name.
//
// A restore carries no fields, so there is nothing for a rule to judge and
// nothing for a caller to fix. What it has instead is this hook, handed the row
// as it was retired and an empty input. Setting a field on that input writes it
// as the row comes back.
//
// Refusing is the other option, and the choice is the application's: a task is
// somebody's note to themselves, and getting it back under a slightly longer
// name beats being told to go and rename something else first. A resource where
// the name means something to another system would want the error.
func (s *rules) beforeRestore(ctx context.Context, _ tenancy.Claims, in *model.TodoUpdateInput, prev *model.Todo) error {
	taken, err := s.titleHeldBy(ctx, prev.Title, prev.ID)
	if err != nil {
		// Not a refusal: the title may be perfectly free and we could not tell.
		return err
	}
	if !taken {
		return nil
	}

	in.Title = patch.NewOptional(restoredTitle(prev.Title, time.Now()))
	s.logger.InfoContext(ctx, "renamed a todo to restore it",
		"todo", prev.ID, "from", prev.Title)
	return nil
}

// restoredTitle is the name a task comes back under when its own is taken.
//
// The time is in it because the alternative — a counter — needs a query to know
// which number is free, and would still collide with the one somebody typed by
// hand. To the minute: a second restore within the same minute is a case the
// unique index refuses, which is the right outcome for two restores nobody
// asked for separately.
func restoredTitle(title string, at time.Time) string {
	return title + " (restored @ " + at.Format("2006-01-02 15:04") + ")"
}

// Complete marks the task as done.
//
// It is a custom endpoint because it is not an update: the caller does not say
// what the row should become, it says what happened, and the rule about doing
// it twice is not something the schema could have expressed.
//
// The write goes through the same writer a PATCH does, so it carries the same
// validator and the same hooks. That is what Writer is for: a custom endpoint
// reaching for the repository would have to pass them by hand, and one that
// forgot would be a second way into the table where the rules do not run.
func (s *rules) Complete(ctx context.Context, r api.Request[api.TodoCompletePath, struct{}, api.TodoCompleteBody]) (*model.Todo, error) {
	existing, err := s.repo.Get(ctx, r.Path.ID)
	if err != nil {
		return nil, err
	}
	if existing.IsDone {
		return nil, rigerr.Conflict("the todo is already done")
	}

	in := model.TodoUpdateInput{IsDone: patch.NewOptional(true)}
	if note := noteToAppend(existing, r.Body.Note); note != "" {
		in.Notes = patch.NewNullable(note)
	}

	return s.write.Update(ctx, r.Path.ID, in)
}

// noteToAppend is the task's notes with the completion note added, or empty
// when there is nothing to add.
func noteToAppend(existing *model.Todo, note *string) string {
	if note == nil || strings.TrimSpace(*note) == "" {
		return ""
	}

	added := strings.TrimSpace(*note)
	if existing.Notes == nil || *existing.Notes == "" {
		return added
	}
	return *existing.Notes + "\n" + added
}

// announceCreated tells the notifier about a todo that now certainly exists.
//
// It returns nothing, because there is nothing left to fail: the row is
// committed, the request is answered, and an error here could only be reported
// to somebody who has stopped listening. Whatever it hands off has to be able
// to absorb that itself.
func (s *rules) announceCreated(ctx context.Context, claims tenancy.Claims, created *model.Todo) {
	if s.notifier == nil {
		return
	}
	s.notifier.Record("created " + created.ID.String() + ": " + created.Title)

	// DebugContext rather than Debug: a handler that puts a trace or a request
	// identifier on the context has somewhere to be picked up from, and "did
	// the hook fire" is exactly the question this answers at three in the
	// morning.
	// The claims came in as an argument rather than off the context, which is
	// what makes them safe to read here: this runs after the commit, and the
	// request's context may already be cancelled.
	s.logger.DebugContext(ctx, "announced a new todo", "todo", created.ID, "by", claims.AccountID)
}
