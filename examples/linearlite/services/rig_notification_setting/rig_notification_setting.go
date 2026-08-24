package rig_notification_setting

import (
	"context"

	"github.com/google/uuid"
	"github.com/simonjanss/rig/examples/linearlite/internal/api"
	"github.com/simonjanss/rig/examples/linearlite/internal/model"
	"github.com/simonjanss/rig/examples/linearlite/internal/store"
	"github.com/simonjanss/rig/runtime/dbhook"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// rules is RigNotificationSetting's business logic.
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
	repo store.RigNotificationSettingRepository
	// write performs a write with the hooks below already attached. Use it rather
	// than the repository: reaching for the repository means passing the hooks by
	// hand, and forgetting once is a second way into the table where the rules do
	// not run.
	write api.RigNotificationSettingWriter
}

// rules satisfies what the constructor asks for. The check is here so that a
// new endpoint in the configuration becomes a compile error rather than a
// route that answers 501 at runtime.
var _ api.RigNotificationSettingRules = (*rules)(nil)

// New builds the service.
//
// To override a generated operation, wrap what this returns and shadow the
// promoted method:
//
//	type Service struct{ api.DefaultRigNotificationSettingService }
//	func (s *Service) Get(ctx context.Context, r api.Request[…]) (…) { … }
//
// The custom endpoints keep working through the value inside it, so only what
// you shadow changes.
func New(repo store.RigNotificationSettingRepository) api.DefaultRigNotificationSettingService {
	return api.NewRigNotificationSettingService(repo, &rules{repo: repo})
}

// Bind receives the writer built from the hooks below. rig calls it once,
// during construction.
func (s *rules) Bind(w api.RigNotificationSettingWriter) { s.write = w }

// Hooks is everything about RigNotificationSetting that the schema cannot
// describe, in the order it runs: the rules, then Before and After inside the
// transaction — returning an error from either undoes the write — then
// AfterCommit once it has landed, which is the only safe place to tell
// anything outside the database.
//
// The rules are one function per field, against the row the request would
// produce. Two sets, because whether a row may exist is not whether it may
// change: an update has no entry for a column it cannot touch, and a create
// none for one it cannot set.
//
// It is asked for rather than set, so there is no way to end up with a service
// whose rules were never attached. An empty one is a fine answer; it is just
// an answer.
func (s *rules) Hooks() api.RigNotificationSettingHooks {
	return api.RigNotificationSettingHooks{
		Read: dbhook.ReadHooks[model.RigNotificationSettingFilter, model.RigNotificationSetting]{
			Narrow: nil,
			Rows:   nil,
		},
		Create: dbhook.CreateHooks[model.RigNotificationSettingCreateInput, model.RigNotificationSetting]{
			Validator: model.RigNotificationSettingCreateValidator{
				AccountID:   s.validateAccountID,
				Kind:        nil,
				Channel:     nil,
				IsEnabled:   nil,
				Digest:      nil,
				ActiveFrom:  nil,
				ActiveUntil: nil,
				ActiveDays:  nil,
				Entity:      nil,
			},
			Before:      nil,
			After:       nil,
			AfterCommit: nil,
		},
		Update: dbhook.UpdateHooks[model.RigNotificationSettingUpdateInput, model.RigNotificationSetting]{
			Validator: model.RigNotificationSettingUpdateValidator{
				AccountID:   s.validateAccountID,
				Kind:        nil,
				Channel:     nil,
				IsEnabled:   nil,
				Digest:      nil,
				ActiveFrom:  nil,
				ActiveUntil: nil,
				ActiveDays:  nil,
				Entity:      nil,
			},
			Before:      nil,
			After:       nil,
			AfterCommit: nil,
		},
		Delete: dbhook.DeleteHooks[model.RigNotificationSettingDeleteInput, model.RigNotificationSetting]{
			Before:      nil,
			After:       nil,
			AfterCommit: nil,
		},
	}
}

// validateAccountID refuses a row that names somebody else.
//
// `access: { scope: own, owner: account_id }` in the configuration narrows
// every read and every update to the caller's own rows — a generated write
// starts by reading the row, so a read that comes back empty is a 404 and the
// write never happens. Create has no row to read, which is the one hole that
// leaves, and this is it closed: a preference is a preference about yourself.
//
// It is a rule rather than a hook that overwrites the value, because a request
// asking for something it may not have should be told so rather than quietly
// given something else.
//
// The engine resolves a send by account_id, so the column this narrows on and
// the column that decides what somebody is sent are the same column.
func (s *rules) validateAccountID(ctx context.Context, _ *model.RigNotificationSettingValidatorContext, value uuid.UUID) error {
	claims, err := tenancy.FromContext(ctx)
	if err != nil {
		return err
	}
	if value != claims.AccountID {
		return rigerr.NewFieldError(rigerr.FieldCodeInvalidValue,
			"this is yours to set, so account_id has to be your own")
	}
	return nil
}
