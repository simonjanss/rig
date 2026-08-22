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

	"github.com/simonjanss/rig/examples/linearlite/internal/api"
	"github.com/simonjanss/rig/examples/linearlite/internal/model"
	"github.com/simonjanss/rig/examples/linearlite/internal/store"
	"github.com/simonjanss/rig/notify"
	"github.com/simonjanss/rig/runtime/dbhook"
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
	// accounts answers who is live in a tenant, for NotifyWho. The foundation's
	// tables have no generated repository here, so the query is plain SQL.
	accounts *pgxpool.Pool
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
func New(repo store.TodoRepository, inbox *notify.Service, nudge func(), accounts *pgxpool.Pool) api.DefaultTodoService {
	return api.NewTodoService(repo, &rules{repo: repo, inbox: inbox, nudge: nudge, accounts: accounts})
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

// NotifyAt says when notifications about this row are due: now, always. The
// zero time means now — a scheduled notification is the same row with a later
// time, and examples/auth shows that half.
func (s *rules) NotifyAt(_ *model.Todo, _ string) (time.Time, bool) {
	return time.Time{}, true
}

// NotifyWho answers who should hear about a todo, at the moment of sending.
//
// The stakeholders are whoever created the item and whoever it is assigned to,
// minus the person who made the change — being told about your own edit is
// noise. The row in hand is current, so an assignment made after the change
// was announced still reaches the new assignee; that lateness is the point of
// answering here rather than at write time.
func (s *rules) NotifyWho(ctx context.Context, n *notify.Notification, row *model.Todo) ([]uuid.UUID, error) {
	var actor uuid.UUID
	if len(n.Payload) > 0 {
		var p eventPayload
		if err := json.Unmarshal(n.Payload, &p); err == nil {
			actor = p.ActorAccountID
		}
	}

	stakeholders := make([]uuid.UUID, 0, 2)
	for _, id := range []*uuid.UUID{row.CreatedByAccountID, row.AssigneeAccountID} {
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

	rows, err := s.accounts.Query(ctx, q, n.TenantID, stakeholders)
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
