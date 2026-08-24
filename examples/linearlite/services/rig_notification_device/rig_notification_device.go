package rig_notification_device

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/examples/linearlite/internal/api"
	"github.com/simonjanss/rig/examples/linearlite/internal/model"
	"github.com/simonjanss/rig/examples/linearlite/internal/store"
	"github.com/simonjanss/rig/runtime/dbhook"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// rules is RigNotificationDevice's business logic.
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
	repo store.RigNotificationDeviceRepository
	// write performs a write with the hooks below already attached. Use it rather
	// than the repository: reaching for the repository means passing the hooks by
	// hand, and forgetting once is a second way into the table where the rules do
	// not run.
	write api.RigNotificationDeviceWriter
}

// rules satisfies what the constructor asks for. The check is here so that a
// new endpoint in the configuration becomes a compile error rather than a
// route that answers 501 at runtime.
var _ api.RigNotificationDeviceRules = (*rules)(nil)

// New builds the service.
//
// To override a generated operation, wrap what this returns and shadow the
// promoted method:
//
//	type Service struct{ api.DefaultRigNotificationDeviceService }
//	func (s *Service) Get(ctx context.Context, r api.Request[…]) (…) { … }
//
// The custom endpoints keep working through the value inside it, so only what
// you shadow changes.
func New(repo store.RigNotificationDeviceRepository) api.DefaultRigNotificationDeviceService {
	return api.NewRigNotificationDeviceService(repo, &rules{repo: repo})
}

// Bind receives the writer built from the hooks below. rig calls it once,
// during construction.
func (s *rules) Bind(w api.RigNotificationDeviceWriter) { s.write = w }

// Hooks is everything about RigNotificationDevice that the schema cannot
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
func (s *rules) Hooks() api.RigNotificationDeviceHooks {
	return api.RigNotificationDeviceHooks{
		Read: dbhook.ReadHooks[model.RigNotificationDeviceFilter, model.RigNotificationDevice]{
			Narrow: nil,
			Rows:   nil,
		},
		Create: dbhook.CreateHooks[model.RigNotificationDeviceCreateInput, model.RigNotificationDevice]{
			Validator: model.RigNotificationDeviceCreateValidator{
				AccountID:  s.validateAccountID,
				Channel:    nil,
				Token:      s.validateToken,
				Label:      nil,
				LastSeenAt: nil,
				RevokedAt:  nil,
				Entity:     nil,
			},
			Before:      nil,
			After:       nil,
			AfterCommit: nil,
		},
		Update: dbhook.UpdateHooks[model.RigNotificationDeviceUpdateInput, model.RigNotificationDevice]{
			Validator: model.RigNotificationDeviceUpdateValidator{
				AccountID:  s.validateAccountID,
				Channel:    nil,
				Token:      s.validateToken,
				Label:      nil,
				LastSeenAt: nil,
				RevokedAt:  nil,
				Entity:     nil,
			},
			Before:      nil,
			After:       nil,
			AfterCommit: nil,
		},
		Delete: dbhook.DeleteHooks[model.RigNotificationDeviceDeleteInput, model.RigNotificationDevice]{
			Before:      nil,
			After:       nil,
			AfterCommit: nil,
		},
	}
}

// validateAccountID refuses a device that names somebody else.
//
// `access: { scope: own, owner: account_id }` in the configuration narrows every
// read and every update to the caller's own rows — a generated write starts by
// reading the row, so a read that comes back empty is a 404 and the write never
// happens. Create has no row to read, which is the one hole that leaves, and
// this is it closed: a token addresses one person's machine, and registering one
// against somebody else's account would put their notifications on your screen.
//
// A rule rather than a hook that overwrites the value, because a request asking
// for something it may not have should be told so rather than quietly given
// something else.
func (s *rules) validateAccountID(ctx context.Context, _ *model.RigNotificationDeviceValidatorContext, value uuid.UUID) error {
	claims, err := tenancy.FromContext(ctx)
	if err != nil {
		return err
	}
	if value != claims.AccountID {
		return rigerr.NewFieldError(rigerr.FieldCodeInvalidValue,
			"a device is yours, so account_id has to be your own")
	}
	return nil
}

// validateToken refuses an empty registration.
//
// The token is opaque to rig — it is whatever the provider gave the browser, a
// Web Push endpoint and its keys for a real one — so there is nothing here to
// check about its shape. What is worth refusing is the blank one, which is a
// device row that can never be delivered to and will sit in somebody's list
// looking like it works.
func (s *rules) validateToken(_ context.Context, _ *model.RigNotificationDeviceValidatorContext, value string) error {
	if strings.TrimSpace(value) == "" {
		return rigerr.NewFieldError(rigerr.FieldCodeCannotBeEmpty,
			"a device needs the token its provider gave it")
	}
	return nil
}
