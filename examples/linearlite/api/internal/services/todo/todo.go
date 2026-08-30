package todo

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/examples/linearlite/internal/generated/api"
	"github.com/simonjanss/rig/examples/linearlite/internal/generated/model"
	"github.com/simonjanss/rig/examples/linearlite/internal/generated/store"
	"github.com/simonjanss/rig/notify"
	"github.com/simonjanss/rig/runtime/dbhook"
	"github.com/simonjanss/rig/runtime/dbx"
	"github.com/simonjanss/rig/runtime/patch"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// The notification kinds this service announces. Two rather than one, because
// the front end says different sentences about them — "moved your item to
// Done" and "edited your item" — and the kind is the only thing the inbox line
// carries to choose between them.
const (
	KindTodoStatusChanged = "TodoStatusChanged"
	KindTodoUpdated       = "TodoUpdated"
)

// eventPayload rides in the notification as jsonb.
//
// The actor is here because notify.Notification carries no actor field, on
// purpose: most notifications are about a row, not a person. This one is about
// both — NotifyWho reads the actor back to keep them out of their own
// audience, and the front end reads the rest to say what happened without
// another fetch.
type eventPayload struct {
	ActorAccountID uuid.UUID `json:"actorAccountId"`
	FromStatus     string    `json:"fromStatus,omitempty"`
	ToStatus       string    `json:"toStatus,omitempty"`
	Title          string    `json:"title"`
	// PreviousAssigneeAccountID is who held the item before this change, and is
	// set only when this change is what took it from them.
	//
	// It is here for the same reason the actor is: the audience is computed from
	// the row at the moment of sending, and by then the person an item was taken
	// from is not a stakeholder in it. Without this, having your item taken is
	// the one change to it you are never told about — and it is the change most
	// worth hearing.
	PreviousAssigneeAccountID *uuid.UUID `json:"previousAssigneeAccountId,omitempty"`
}

// rules is Todo's business logic.
//
// A rule about a field goes in the validator, something that has to happen
// with a write goes in a hook, and the two notification questions the join
// table obliges this service to answer are at the bottom.
//
// Unlike the .gen.go files, this one is yours: rig writes it once and never
// touches it again.
type rules struct {
	repo store.TodoRepository
	// inbox is where a change is announced. Nil-able so a test can build the
	// service without the notification tables.
	inbox *notify.Service
	// nudge asks the in-process engine for a pass now, so an inbox line appears
	// moments after the drag rather than at the engine's next tick. It is an
	// optimization and nil is fine: the row is written either way, and the
	// dispatch task is the guarantee.
	nudge func()
	// pool is what this service reaches for when the generated repository is not
	// the right tool: the foundation's account table has no repository here, and
	// Claim needs a transaction of its own to hold a row in.
	pool *pgxpool.Pool
	// write performs a write with the hooks below already attached. Use it rather
	// than the repository: reaching for the repository means passing the hooks by
	// hand, and forgetting once is a second way into the table where the rules do
	// not run.
	write api.TodoWriter
}

// rules satisfies what the constructor asks for. The check is here so that a
// new endpoint in the configuration becomes a compile error rather than a
// route that answers 501 at runtime.
var _ api.TodoRules = (*rules)(nil)

// New builds the service.
//
// To override a generated operation, wrap what this returns and shadow the
// promoted method — see examples/todo. Nothing here needs to.
func New(repo store.TodoRepository, inbox *notify.Service, nudge func(), pool *pgxpool.Pool) api.DefaultTodoService {
	return api.NewTodoService(repo, &rules{repo: repo, inbox: inbox, nudge: nudge, pool: pool})
}

// Bind receives the writer built from the hooks below. rig calls it once,
// during construction.
func (s *rules) Bind(w api.TodoWriter) { s.write = w }

// Hooks is everything about Todo that the schema cannot describe.
//
// The one hook with a body is Update.After: a status change is what the board
// is about, and telling the stakeholders is part of the change — After rather
// than AfterCommit, because the notification row commits or rolls back with
// the update it describes.
func (s *rules) Hooks() api.TodoHooks {
	return api.TodoHooks{
		Read: dbhook.ReadHooks[model.TodoFilter, model.Todo]{
			Narrow: nil,
			Rows:   nil,
		},
		Create: dbhook.CreateHooks[model.TodoCreateInput, model.Todo]{
			Validator: model.TodoCreateValidator{
				Title: s.validateTitle,
			},
			Before:      nil,
			After:       nil,
			AfterCommit: nil,
		},
		Update: dbhook.UpdateHooks[model.TodoUpdateInput, model.Todo]{
			Validator: model.TodoUpdateValidator{
				Title: s.validateTitle,
			},
			Before: nil,
			After:  s.announceChange,
			// AfterCommit is the only safe place to touch something outside
			// the database, and this touches the engine: without the nudge the
			// line After wrote sits Pending until the next tick, and a toast a
			// minute after the drag reads as broken.
			AfterCommit: func(context.Context, tenancy.Claims, *model.Todo, *model.Todo) {
				if s.nudge != nil {
					s.nudge()
				}
			},
		},
		Delete: dbhook.DeleteHooks[model.TodoDeleteInput, model.Todo]{
			Before:      nil,
			After:       nil,
			AfterCommit: nil,
		},
		Restore: dbhook.RestoreHooks[model.TodoUpdateInput, model.Todo]{
			Validator: model.TodoUpdateValidator{
				Title: s.validateTitle,
			},
			Before:      nil,
			After:       nil,
			AfterCommit: nil,
		},
	}
}

// validateTitle keeps the board legible: something to read, short enough to
// read on a card. The schema already refuses NULL; this refuses "   ", which
// is NULL wearing spaces.
func (s *rules) validateTitle(_ context.Context, _ *model.TodoValidatorContext, value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return rigerr.NewFieldError(rigerr.FieldCodeCannotBeEmpty, "give the item a title")
	}
	if utf8.RuneCountInString(trimmed) > 200 {
		return rigerr.NewFieldError(rigerr.FieldCodeTooLong, "keep the title under 200 characters")
	}
	return nil
}

// announceChange tells the stakeholders inside the update's own transaction.
//
// After rather than AfterCommit, deliberately: the notification row is part of
// the change, so a change that committed without it is a notification nobody
// will ever send, and one that rolled back takes the notification with it.
// Who actually hears about it is decided later, in NotifyWho, at the moment of
// sending.
func (s *rules) announceChange(ctx context.Context, claims tenancy.Claims, updated, prev *model.Todo) error {
	if s.inbox == nil {
		return nil
	}

	kind := KindTodoUpdated
	payload := eventPayload{ActorAccountID: claims.AccountID, Title: updated.Title}
	if updated.Status != prev.Status {
		kind = KindTodoStatusChanged
		payload.FromStatus = string(prev.Status)
		payload.ToStatus = string(updated.Status)
	}
	// Recorded here because prev is only in hand here, and read in NotifyWho,
	// because that is where the audience is decided. A claim and a drag that
	// reassigns are the same thing to this: somebody no longer holds an item
	// they held a moment ago.
	if prev.AssigneeAccountID != nil &&
		(updated.AssigneeAccountID == nil || *updated.AssigneeAccountID != *prev.AssigneeAccountID) {
		payload.PreviousAssigneeAccountID = prev.AssigneeAccountID
	}

	a := api.AnnounceTodo(s, updated, kind)
	// One inbox line per item until it is read, however many edits arrive —
	// and the group key doubles as the front end's link target: "todo:<id>".
	a.Group = notify.GroupBySubject(a.Subject)
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	a.Payload = raw

	_, err = s.inbox.Announce(ctx, a)
	return err
}

// Claim takes the item for whoever is asking.
//
// This is the endpoint services/todo/todo.yaml declares, and the reason it is
// one: the answer depends on the value already in the column, so a client
// doing it with a PATCH would read, decide and write — and two clients reading
// the same unassigned item both decide yes.
//
// Which is why the read is inside a transaction and takes the row's lock. A
// decision about a column is only worth what the read under it is worth: with
// an ordinary SELECT, two requests a millisecond apart both see nobody holding
// the item, both write, and both are told they hold it — the very race the
// endpoint exists to answer, moved from the client to here. FOR UPDATE makes
// the second one wait and then read what the first one wrote.
//
// The write goes through s.write rather than the repository, so the validator,
// the snapshot, the notification and the nudge all happen exactly as they do
// for a drag, and it joins this transaction rather than opening its own —
// dbx.InTx takes the one already on the context — so the lock is still held
// when the update lands, and AfterCommit still runs after the commit below.
func (s *rules) Claim(ctx context.Context, r api.Request[api.TodoClaimPath, struct{}, api.TodoClaimBody]) (*model.Todo, error) {
	me := r.Claims.AccountID
	if me == uuid.Nil {
		// An API key minted for a machine has no account behind it, and there
		// is nobody for it to claim on behalf of. Better said here than
		// recorded as an item assigned to nobody.
		return nil, rigerr.Forbidden("a claim needs somebody to claim it")
	}

	var out *model.Todo
	err := dbx.InTx(ctx, s.pool, func(ctx context.Context, tx dbx.Conn) error {
		// The lock, and nothing else: what the row says is read back through
		// the repository below, which is where tenancy, soft delete and a 404
		// are already decided. A row this misses is a row Get is about to
		// refuse.
		if _, err := tx.Exec(ctx, `
			SELECT 1 FROM todo
			 WHERE id = $1 AND tenant_id = $2 FOR UPDATE`,
			r.Path.ID, r.Claims.TenantID); err != nil {
			return err
		}

		existing, err := s.repo.Get(ctx, r.Path.ID)
		if err != nil {
			return err
		}

		switch {
		case existing.AssigneeAccountID != nil && *existing.AssigneeAccountID == me:
			// Already yours. A button pressed twice is not a disagreement, so
			// this is the row unchanged rather than a 409 — and unchanged means
			// no update, so nobody is notified about nothing happening.
			out = existing
			return nil
		case existing.AssigneeAccountID != nil && !steal(r.Body):
			return rigerr.Conflict("somebody else holds this item; send steal to take it anyway")
		}

		out, err = s.write.Update(ctx, r.Path.ID, model.TodoUpdateInput{
			AssigneeAccountID: patch.NewNullable(me),
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// steal reads the optional flag. Absent is false: taking somebody else's item
// is the thing you have to ask for.
func steal(b api.TodoClaimBody) bool { return b.Steal != nil && *b.Steal }

// NotifyAt says when notifications about this row are due: now, always. The
// zero time means now — a scheduled notification is the same row with a later
// time, and examples/auth shows that half.
func (s *rules) NotifyAt(_ *model.Todo, _ string) (time.Time, bool) {
	return time.Time{}, true
}

// NotifyWho answers who should hear about a todo, at the moment of sending.
//
// The stakeholders are whoever created the item, whoever it is assigned to,
// and — when the change took the item from somebody — whoever held it before,
// minus the person who made the change, because being told about your own edit
// is noise. The row in hand is current, so an assignment made after the change
// was announced still reaches the new assignee; that lateness is the point of
// answering here rather than at write time. It is also why the previous holder
// cannot be found in the row and has to be carried in the payload: by now the
// item is somebody else's.
func (s *rules) NotifyWho(ctx context.Context, n *notify.Notification, row *model.Todo) ([]uuid.UUID, error) {
	var p eventPayload
	if len(n.Payload) > 0 {
		// A payload that will not parse is not a reason to tell nobody: the
		// audience is a fact about the row, and what is in here refines it.
		_ = json.Unmarshal(n.Payload, &p)
	}
	actor := p.ActorAccountID

	stakeholders := make([]uuid.UUID, 0, 3)
	for _, id := range []*uuid.UUID{row.CreatedByAccountID, row.AssigneeAccountID, p.PreviousAssigneeAccountID} {
		if id == nil || *id == actor || slices.Contains(stakeholders, *id) {
			continue
		}
		stakeholders = append(stakeholders, *id)
	}
	if len(stakeholders) == 0 {
		return nil, nil
	}

	// Filtered through the account table, because a stakeholder may have been
	// deactivated since the row was written — the audience is computed now, and
	// now is when that matters.
	const q = `SELECT id FROM rig_account
		WHERE tenant_id = $1 AND id = ANY($2) AND deleted_at IS NULL AND is_active`

	rows, err := s.pool.Query(ctx, q, n.TenantID, stakeholders)
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
