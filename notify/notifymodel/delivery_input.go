package notifymodel

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/simonjanss/rig/runtime/patch"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// NotificationDeliveryCreateInput is what creating a NotificationDelivery
// takes.
//
// The identifier, the tenant, and the audit columns are absent: those are
// stamped by the repository from the request's claims.
type NotificationDeliveryCreateInput struct {
	// The inbox line this is a copy of. The line is the truth and this is one way
	// it was repeated.
	RecipientID uuid.UUID `json:"recipientId"`
	// Who it is for, denormalized off the line so a claim can group by it without
	// a join.
	AccountID uuid.UUID `json:"accountId"`
	// Where it is going. One row per line per channel, which is what the unique
	// index below enforces.
	Channel NotificationChannel `json:"channel"`
	// Copied from the notification, so the settings resolution needs no join
	// either.
	Kind string `json:"kind"`
	// What the setting said when this row was written. Immediate rows are sent on
	// their own; the rest are batched per account and channel.
	Digest NotificationDigest `json:"digest"`
	// Skipped means a setting refused it, which is different from Failed and worth
	// telling apart in a report.
	State NotificationDeliveryState `json:"state"`
	// When this is due. A delivery held outside somebody hours moves to the next
	// opening; a digested one moves to its window close.
	DeliverAt time.Time `json:"deliverAt"`
	// When a channel accepted it, which is not the same as it arriving.
	SentAt *time.Time `json:"sentAt"`
	// What the channel said last time. Kept on a retry as well as on a failure, so
	// a pattern is visible before the cap is reached.
	FailedReason *string `json:"failedReason"`
	// How many times this has been claimed. Past notifications.max_attempts it is
	// Failed and stops being claimed.
	Attempts int `json:"attempts"`
	// When a dispatcher took it. Past notifications.claim_ttl another one may,
	// which is what makes a crashed process recoverable.
	ClaimedAt *time.Time `json:"claimedAt"`
	// Which process holds the lease. A uuid generated once per process, with the
	// hostname beside it in the log line, so a stuck lease traces to a pod rather
	// than to a mystery.
	ClaimedBy *uuid.UUID `json:"claimedBy"`
}

// Normalize tidies what was given before anything checks it.
//
// It runs first so that validation sees the value that will actually be
// stored: a title with a trailing space and one without are the same title,
// and rejecting the second for a length rule the first passes would be
// indefensible.
func (i *NotificationDeliveryCreateInput) Normalize() {
	if v, ok := ParseNotificationChannel(string(i.Channel)); ok {
		i.Channel = v
	}
	i.Kind = strings.TrimSpace(i.Kind)
	if v, ok := ParseNotificationDigest(string(i.Digest)); ok {
		i.Digest = v
	}
	if i.Digest == "" {
		i.Digest = NotificationDigestImmediate
	}
	if v, ok := ParseNotificationDeliveryState(string(i.State)); ok {
		i.State = v
	}
	if i.State == "" {
		i.State = NotificationDeliveryStatePending
	}
	if i.FailedReason != nil {
		*i.FailedReason = strings.TrimSpace(*i.FailedReason)
	}
}

// NotificationDeliveryCreateInputError says what was wrong with each field of
// a NotificationDeliveryCreateInput.
//
// Its shape is the input's shape, so a client can attach every message to the
// field it is about without matching on strings. A member is nil when that
// field was fine, and the whole value is nil when the input was. It is what
// the 422 carries.
type NotificationDeliveryCreateInputError struct {
	// The inbox line this is a copy of. The line is the truth and this is one way
	// it was repeated.
	RecipientID *rigerr.FieldError `json:"recipientId,omitempty"`
	// Who it is for, denormalized off the line so a claim can group by it without
	// a join.
	AccountID *rigerr.FieldError `json:"accountId,omitempty"`
	// Where it is going. One row per line per channel, which is what the unique
	// index below enforces.
	Channel *rigerr.FieldError `json:"channel,omitempty"`
	// Copied from the notification, so the settings resolution needs no join
	// either.
	Kind *rigerr.FieldError `json:"kind,omitempty"`
	// What the setting said when this row was written. Immediate rows are sent on
	// their own; the rest are batched per account and channel.
	Digest *rigerr.FieldError `json:"digest,omitempty"`
	// Skipped means a setting refused it, which is different from Failed and worth
	// telling apart in a report.
	State *rigerr.FieldError `json:"state,omitempty"`
	// When this is due. A delivery held outside somebody hours moves to the next
	// opening; a digested one moves to its window close.
	DeliverAt *rigerr.FieldError `json:"deliverAt,omitempty"`
	// When a channel accepted it, which is not the same as it arriving.
	SentAt *rigerr.FieldError `json:"sentAt,omitempty"`
	// What the channel said last time. Kept on a retry as well as on a failure, so
	// a pattern is visible before the cap is reached.
	FailedReason *rigerr.FieldError `json:"failedReason,omitempty"`
	// How many times this has been claimed. Past notifications.max_attempts it is
	// Failed and stops being claimed.
	Attempts *rigerr.FieldError `json:"attempts,omitempty"`
	// When a dispatcher took it. Past notifications.claim_ttl another one may,
	// which is what makes a crashed process recoverable.
	ClaimedAt *rigerr.FieldError `json:"claimedAt,omitempty"`
	// Which process holds the lease. A uuid generated once per process, with the
	// hostname beside it in the log line, so a stuck lease traces to a pod rather
	// than to a mystery.
	ClaimedBy *rigerr.FieldError `json:"claimedBy,omitempty"`

	// Entity is a problem with the row as a whole rather than with one field: what
	// the Entity rule said.
	Entity *rigerr.FieldError `json:"entity,omitempty"`
}

// Empty reports whether anything went wrong. A validator that found nothing
// returns nil rather than one of these.
func (e *NotificationDeliveryCreateInputError) Empty() bool {
	if e == nil {
		return true
	}

	return e.RecipientID == nil && e.AccountID == nil && e.Channel == nil && e.Kind == nil && e.Digest == nil && e.State == nil && e.DeliverAt == nil && e.SentAt == nil && e.FailedReason == nil && e.Attempts == nil && e.ClaimedAt == nil && e.ClaimedBy == nil && e.Entity == nil
}

// Error implements error. The sentence is for logs and for a person; the
// structure above is what a client acts on.
func (e *NotificationDeliveryCreateInputError) Error() string {
	var parts []string
	if e.RecipientID != nil {
		parts = append(parts, "recipientId "+e.RecipientID.Error())
	}
	if e.AccountID != nil {
		parts = append(parts, "accountId "+e.AccountID.Error())
	}
	if e.Channel != nil {
		parts = append(parts, "channel "+e.Channel.Error())
	}
	if e.Kind != nil {
		parts = append(parts, "kind "+e.Kind.Error())
	}
	if e.Digest != nil {
		parts = append(parts, "digest "+e.Digest.Error())
	}
	if e.State != nil {
		parts = append(parts, "state "+e.State.Error())
	}
	if e.DeliverAt != nil {
		parts = append(parts, "deliverAt "+e.DeliverAt.Error())
	}
	if e.SentAt != nil {
		parts = append(parts, "sentAt "+e.SentAt.Error())
	}
	if e.FailedReason != nil {
		parts = append(parts, "failedReason "+e.FailedReason.Error())
	}
	if e.Attempts != nil {
		parts = append(parts, "attempts "+e.Attempts.Error())
	}
	if e.ClaimedAt != nil {
		parts = append(parts, "claimedAt "+e.ClaimedAt.Error())
	}
	if e.ClaimedBy != nil {
		parts = append(parts, "claimedBy "+e.ClaimedBy.Error())
	}
	if e.Entity != nil {
		parts = append(parts, e.Entity.Error())
	}

	return "rig_notification_delivery is not valid: " + strings.Join(parts, "; ")
}

// ErrorCode implements [rigerr.Coder]: the request was understood and its
// content is what is wrong, which is 422 and not 400.
func (e *NotificationDeliveryCreateInputError) ErrorCode() rigerr.Code {
	return rigerr.CodeUnprocessableEntity
}

// ErrorFields implements [rigerr.FieldReporter], which is how the HTTP layer
// finds this and answers with it rather than with prose.
func (e *NotificationDeliveryCreateInputError) ErrorFields() any { return e }

// Validate checks what the schema can decide on its own.
//
// Everything a column declares — NOT NULL, a length, an enumeration's values
// — is checked here, so a service only writes the rules that are actually
// about the business. Every field is checked before returning, because a form
// that reports one problem per round trip is a form people give up on.
//
// What comes back is a *NotificationDeliveryCreateInputError, shaped like the
// input itself.
func (i *NotificationDeliveryCreateInput) Validate() error {
	var failed NotificationDeliveryCreateInputError

	if !i.Channel.Valid() {
		failed.Channel = rigerr.NewFieldError(rigerr.FieldCodeInvalidValue, "%q is not one of the allowed values", i.Channel)
	}
	if strings.TrimSpace(i.Kind) == "" {
		failed.Kind = rigerr.NewFieldError(rigerr.FieldCodeCannotBeEmpty, "cannot be empty")
	}
	if !i.Digest.Valid() {
		failed.Digest = rigerr.NewFieldError(rigerr.FieldCodeInvalidValue, "%q is not one of the allowed values", i.Digest)
	}
	if !i.State.Valid() {
		failed.State = rigerr.NewFieldError(rigerr.FieldCodeInvalidValue, "%q is not one of the allowed values", i.State)
	}

	if failed.Empty() {
		return nil
	}
	return &failed
}

// NotificationDeliveryUpdateInput is what changing a NotificationDelivery
// takes.
//
// A field left out is untouched. A nullable field set to null is cleared —
// which is why the two wrappers differ: a column that cannot hold null has no
// way to be given one, so clearing it is a compile error rather than a
// rejection at runtime. Immutable fields are not here at all.
type NotificationDeliveryUpdateInput struct {
	// The inbox line this is a copy of. The line is the truth and this is one way
	// it was repeated.
	RecipientID patch.Optional[uuid.UUID] `json:"recipientId"`
	// Who it is for, denormalized off the line so a claim can group by it without
	// a join.
	AccountID patch.Optional[uuid.UUID] `json:"accountId"`
	// Where it is going. One row per line per channel, which is what the unique
	// index below enforces.
	Channel patch.Optional[NotificationChannel] `json:"channel"`
	// Copied from the notification, so the settings resolution needs no join
	// either.
	Kind patch.Optional[string] `json:"kind"`
	// What the setting said when this row was written. Immediate rows are sent on
	// their own; the rest are batched per account and channel.
	Digest patch.Optional[NotificationDigest] `json:"digest"`
	// Skipped means a setting refused it, which is different from Failed and worth
	// telling apart in a report.
	State patch.Optional[NotificationDeliveryState] `json:"state"`
	// When this is due. A delivery held outside somebody hours moves to the next
	// opening; a digested one moves to its window close.
	DeliverAt patch.Optional[time.Time] `json:"deliverAt"`
	// When a channel accepted it, which is not the same as it arriving.
	SentAt patch.Nullable[time.Time] `json:"sentAt"`
	// What the channel said last time. Kept on a retry as well as on a failure, so
	// a pattern is visible before the cap is reached.
	FailedReason patch.Nullable[string] `json:"failedReason"`
	// How many times this has been claimed. Past notifications.max_attempts it is
	// Failed and stops being claimed.
	Attempts patch.Optional[int] `json:"attempts"`
	// When a dispatcher took it. Past notifications.claim_ttl another one may,
	// which is what makes a crashed process recoverable.
	ClaimedAt patch.Nullable[time.Time] `json:"claimedAt"`
	// Which process holds the lease. A uuid generated once per process, with the
	// hostname beside it in the log line, so a stuck lease traces to a pod rather
	// than to a mystery.
	ClaimedBy patch.Nullable[uuid.UUID] `json:"claimedBy"`
}

// Normalize tidies the fields this request actually carries.
//
// It does not fill in the ones it does not: the repository writes exactly the
// columns that were sent, and filling them here would turn every update into a
// write of every column — so two requests changing different fields of one
// row would start overwriting each other instead of composing.
func (i *NotificationDeliveryUpdateInput) Normalize() {
	if v, ok := i.Channel.Get(); ok {
		if parsed, ok := ParseNotificationChannel(string(v)); ok {
			v = parsed
		}
		i.Channel = patch.NewOptional(v)
	}
	if v, ok := i.Kind.Get(); ok {
		v = strings.TrimSpace(v)
		i.Kind = patch.NewOptional(v)
	}
	if v, ok := i.Digest.Get(); ok {
		if parsed, ok := ParseNotificationDigest(string(v)); ok {
			v = parsed
		}
		i.Digest = patch.NewOptional(v)
	}
	if v, ok := i.State.Get(); ok {
		if parsed, ok := ParseNotificationDeliveryState(string(v)); ok {
			v = parsed
		}
		i.State = patch.NewOptional(v)
	}
	if v, ok := i.FailedReason.Get(); ok {
		v = strings.TrimSpace(v)
		i.FailedReason = patch.NewNullable(v)
	}
}

// Merged is the row as it will be once this update is applied.
//
// It is what validation runs against, and the reason it exists: a rule
// spanning two fields cannot be checked from a partial request. "Ends after
// starts" is unanswerable when only one of them was sent.
//
// It returns a copy. The input keeps its patches, so the repository still
// writes only the columns that were actually given.
func (i NotificationDeliveryUpdateInput) Merged(prev *NotificationDelivery) NotificationDelivery {
	out := *prev

	if v, ok := i.RecipientID.Get(); ok {
		out.RecipientID = v
	}
	if v, ok := i.AccountID.Get(); ok {
		out.AccountID = v
	}
	if v, ok := i.Channel.Get(); ok {
		out.Channel = v
	}
	if v, ok := i.Kind.Get(); ok {
		out.Kind = v
	}
	if v, ok := i.Digest.Get(); ok {
		out.Digest = v
	}
	if v, ok := i.State.Get(); ok {
		out.State = v
	}
	if v, ok := i.DeliverAt.Get(); ok {
		out.DeliverAt = v
	}
	if i.SentAt.Touched() {
		out.SentAt = i.SentAt.Ptr()
	}
	if i.FailedReason.Touched() {
		out.FailedReason = i.FailedReason.Ptr()
	}
	if v, ok := i.Attempts.Get(); ok {
		out.Attempts = v
	}
	if i.ClaimedAt.Touched() {
		out.ClaimedAt = i.ClaimedAt.Ptr()
	}
	if i.ClaimedBy.Touched() {
		out.ClaimedBy = i.ClaimedBy.Ptr()
	}

	return out
}

// NotificationDeliveryUpdateInputError says what was wrong with each field of
// a NotificationDeliveryUpdateInput.
//
// Its shape is the input's shape, so a client can attach every message to the
// field it is about without matching on strings. A member is nil when that
// field was fine, and the whole value is nil when the input was. It is what
// the 422 carries.
type NotificationDeliveryUpdateInputError struct {
	// The inbox line this is a copy of. The line is the truth and this is one way
	// it was repeated.
	RecipientID *rigerr.FieldError `json:"recipientId,omitempty"`
	// Who it is for, denormalized off the line so a claim can group by it without
	// a join.
	AccountID *rigerr.FieldError `json:"accountId,omitempty"`
	// Where it is going. One row per line per channel, which is what the unique
	// index below enforces.
	Channel *rigerr.FieldError `json:"channel,omitempty"`
	// Copied from the notification, so the settings resolution needs no join
	// either.
	Kind *rigerr.FieldError `json:"kind,omitempty"`
	// What the setting said when this row was written. Immediate rows are sent on
	// their own; the rest are batched per account and channel.
	Digest *rigerr.FieldError `json:"digest,omitempty"`
	// Skipped means a setting refused it, which is different from Failed and worth
	// telling apart in a report.
	State *rigerr.FieldError `json:"state,omitempty"`
	// When this is due. A delivery held outside somebody hours moves to the next
	// opening; a digested one moves to its window close.
	DeliverAt *rigerr.FieldError `json:"deliverAt,omitempty"`
	// When a channel accepted it, which is not the same as it arriving.
	SentAt *rigerr.FieldError `json:"sentAt,omitempty"`
	// What the channel said last time. Kept on a retry as well as on a failure, so
	// a pattern is visible before the cap is reached.
	FailedReason *rigerr.FieldError `json:"failedReason,omitempty"`
	// How many times this has been claimed. Past notifications.max_attempts it is
	// Failed and stops being claimed.
	Attempts *rigerr.FieldError `json:"attempts,omitempty"`
	// When a dispatcher took it. Past notifications.claim_ttl another one may,
	// which is what makes a crashed process recoverable.
	ClaimedAt *rigerr.FieldError `json:"claimedAt,omitempty"`
	// Which process holds the lease. A uuid generated once per process, with the
	// hostname beside it in the log line, so a stuck lease traces to a pod rather
	// than to a mystery.
	ClaimedBy *rigerr.FieldError `json:"claimedBy,omitempty"`

	// Entity is a problem with the row as a whole rather than with one field: what
	// the Entity rule said.
	Entity *rigerr.FieldError `json:"entity,omitempty"`
}

// Empty reports whether anything went wrong. A validator that found nothing
// returns nil rather than one of these.
func (e *NotificationDeliveryUpdateInputError) Empty() bool {
	if e == nil {
		return true
	}

	return e.RecipientID == nil && e.AccountID == nil && e.Channel == nil && e.Kind == nil && e.Digest == nil && e.State == nil && e.DeliverAt == nil && e.SentAt == nil && e.FailedReason == nil && e.Attempts == nil && e.ClaimedAt == nil && e.ClaimedBy == nil && e.Entity == nil
}

// Error implements error. The sentence is for logs and for a person; the
// structure above is what a client acts on.
func (e *NotificationDeliveryUpdateInputError) Error() string {
	var parts []string
	if e.RecipientID != nil {
		parts = append(parts, "recipientId "+e.RecipientID.Error())
	}
	if e.AccountID != nil {
		parts = append(parts, "accountId "+e.AccountID.Error())
	}
	if e.Channel != nil {
		parts = append(parts, "channel "+e.Channel.Error())
	}
	if e.Kind != nil {
		parts = append(parts, "kind "+e.Kind.Error())
	}
	if e.Digest != nil {
		parts = append(parts, "digest "+e.Digest.Error())
	}
	if e.State != nil {
		parts = append(parts, "state "+e.State.Error())
	}
	if e.DeliverAt != nil {
		parts = append(parts, "deliverAt "+e.DeliverAt.Error())
	}
	if e.SentAt != nil {
		parts = append(parts, "sentAt "+e.SentAt.Error())
	}
	if e.FailedReason != nil {
		parts = append(parts, "failedReason "+e.FailedReason.Error())
	}
	if e.Attempts != nil {
		parts = append(parts, "attempts "+e.Attempts.Error())
	}
	if e.ClaimedAt != nil {
		parts = append(parts, "claimedAt "+e.ClaimedAt.Error())
	}
	if e.ClaimedBy != nil {
		parts = append(parts, "claimedBy "+e.ClaimedBy.Error())
	}
	if e.Entity != nil {
		parts = append(parts, e.Entity.Error())
	}

	return "rig_notification_delivery is not valid: " + strings.Join(parts, "; ")
}

// ErrorCode implements [rigerr.Coder]: the request was understood and its
// content is what is wrong, which is 422 and not 400.
func (e *NotificationDeliveryUpdateInputError) ErrorCode() rigerr.Code {
	return rigerr.CodeUnprocessableEntity
}

// ErrorFields implements [rigerr.FieldReporter], which is how the HTTP layer
// finds this and answers with it rather than with prose.
func (e *NotificationDeliveryUpdateInputError) ErrorFields() any { return e }

// Validate checks the row this update would produce.
//
// Against the merged state, not the request: a length rule on a field nobody
// sent still has to hold, and a rule about two fields needs both.
//
// What comes back is a *NotificationDeliveryUpdateInputError, shaped like the
// input itself.
func (i *NotificationDeliveryUpdateInput) Validate(prev *NotificationDelivery) error {
	var failed NotificationDeliveryUpdateInputError

	merged := i.Merged(prev)

	if !merged.Channel.Valid() {
		failed.Channel = rigerr.NewFieldError(rigerr.FieldCodeInvalidValue, "%q is not one of the allowed values", merged.Channel)
	}
	if strings.TrimSpace(merged.Kind) == "" {
		failed.Kind = rigerr.NewFieldError(rigerr.FieldCodeCannotBeEmpty, "cannot be empty")
	}
	if !merged.Digest.Valid() {
		failed.Digest = rigerr.NewFieldError(rigerr.FieldCodeInvalidValue, "%q is not one of the allowed values", merged.Digest)
	}
	if !merged.State.Valid() {
		failed.State = rigerr.NewFieldError(rigerr.FieldCodeInvalidValue, "%q is not one of the allowed values", merged.State)
	}

	if failed.Empty() {
		return nil
	}
	return &failed
}

// NotificationDeliveryDeleteInput is what deleting a NotificationDelivery
// takes.
type NotificationDeliveryDeleteInput struct {
	// ID is the row to delete.
	ID uuid.UUID `json:"id"`
}

// NotificationDeliveryValidatorContext is what a rule sees.
//
// Values is the row as it will be if this goes through — merged from the
// previous state on an update, so every field is set whether or not the
// request mentioned it. That is the point: "ends after starts" cannot be
// answered from a request that only carried one of them.
type NotificationDeliveryValidatorContext struct {
	// Values is the intended end state.
	Values NotificationDelivery

	// Claims are who is asking. They are a value rather than something to fetch
	// from the context because a rule that has to look them up is a rule that can
	// forget to, and because there is no case where they are absent: a write
	// without a caller is refused by the repository before any rule runs.
	Claims tenancy.Claims

	// previous is the row before this change, and is the zero value on a
	// create — there was nothing before.
	previous NotificationDelivery
	isUpdate bool
	changed  map[string]bool
}

// IsUpdate reports whether there was a row before this.
func (c *NotificationDeliveryValidatorContext) IsUpdate() bool { return c.isUpdate }

// Previous is the row as it was, and the zero value on a create. Check
// IsUpdate before reading it.
func (c *NotificationDeliveryValidatorContext) Previous() NotificationDelivery { return c.previous }

// Changed reports whether this request carried a new value for a column.
//
// It is what keeps an expensive rule from running on every update: a check
// that reaches another service to confirm a reference only needs to run when
// the reference actually moved. On a create everything is changed, because
// everything is new.
func (c *NotificationDeliveryValidatorContext) Changed(column string) bool { return c.changed[column] }

// RecipientIDChanged reports whether this request set recipient_id.
func (c *NotificationDeliveryValidatorContext) RecipientIDChanged() bool {
	return c.changed[ColumnNotificationDeliveryRecipientID]
}

// AccountIDChanged reports whether this request set account_id.
func (c *NotificationDeliveryValidatorContext) AccountIDChanged() bool {
	return c.changed[ColumnNotificationDeliveryAccountID]
}

// ChannelChanged reports whether this request set channel.
func (c *NotificationDeliveryValidatorContext) ChannelChanged() bool {
	return c.changed[ColumnNotificationDeliveryChannel]
}

// KindChanged reports whether this request set kind.
func (c *NotificationDeliveryValidatorContext) KindChanged() bool {
	return c.changed[ColumnNotificationDeliveryKind]
}

// DigestChanged reports whether this request set digest.
func (c *NotificationDeliveryValidatorContext) DigestChanged() bool {
	return c.changed[ColumnNotificationDeliveryDigest]
}

// StateChanged reports whether this request set state.
func (c *NotificationDeliveryValidatorContext) StateChanged() bool {
	return c.changed[ColumnNotificationDeliveryState]
}

// DeliverAtChanged reports whether this request set deliver_at.
func (c *NotificationDeliveryValidatorContext) DeliverAtChanged() bool {
	return c.changed[ColumnNotificationDeliveryDeliverAt]
}

// SentAtChanged reports whether this request set sent_at.
func (c *NotificationDeliveryValidatorContext) SentAtChanged() bool {
	return c.changed[ColumnNotificationDeliverySentAt]
}

// FailedReasonChanged reports whether this request set failed_reason.
func (c *NotificationDeliveryValidatorContext) FailedReasonChanged() bool {
	return c.changed[ColumnNotificationDeliveryFailedReason]
}

// AttemptsChanged reports whether this request set attempts.
func (c *NotificationDeliveryValidatorContext) AttemptsChanged() bool {
	return c.changed[ColumnNotificationDeliveryAttempts]
}

// ClaimedAtChanged reports whether this request set claimed_at.
func (c *NotificationDeliveryValidatorContext) ClaimedAtChanged() bool {
	return c.changed[ColumnNotificationDeliveryClaimedAt]
}

// ClaimedByChanged reports whether this request set claimed_by.
func (c *NotificationDeliveryValidatorContext) ClaimedByChanged() bool {
	return c.changed[ColumnNotificationDeliveryClaimedBy]
}

// NotificationDeliveryCreateValidator is the rules for bringing a
// NotificationDelivery into existence: what the schema cannot express.
//
// One optional function per field this operation can set, so the set of fields
// is the set of rules that could apply — a column an update cannot touch has
// no hook here to write by mistake. A nil one is skipped. Every configured
// hook runs, because a rule that fails should not hide the next one, so a
// request reports everything wrong with it.
//
// A hook returns a FieldError to attach the message to a specific field, or
// any other error to fail the request outright.
type NotificationDeliveryCreateValidator struct {
	// The inbox line this is a copy of. The line is the truth and this is one way
	// it was repeated.
	RecipientID func(ctx context.Context, c *NotificationDeliveryValidatorContext, value uuid.UUID) error
	// Who it is for, denormalized off the line so a claim can group by it without
	// a join.
	AccountID func(ctx context.Context, c *NotificationDeliveryValidatorContext, value uuid.UUID) error
	// Where it is going. One row per line per channel, which is what the unique
	// index below enforces.
	Channel func(ctx context.Context, c *NotificationDeliveryValidatorContext, value NotificationChannel) error
	// Copied from the notification, so the settings resolution needs no join
	// either.
	Kind func(ctx context.Context, c *NotificationDeliveryValidatorContext, value string) error
	// What the setting said when this row was written. Immediate rows are sent on
	// their own; the rest are batched per account and channel.
	Digest func(ctx context.Context, c *NotificationDeliveryValidatorContext, value NotificationDigest) error
	// Skipped means a setting refused it, which is different from Failed and worth
	// telling apart in a report.
	State func(ctx context.Context, c *NotificationDeliveryValidatorContext, value NotificationDeliveryState) error
	// When this is due. A delivery held outside somebody hours moves to the next
	// opening; a digested one moves to its window close.
	DeliverAt func(ctx context.Context, c *NotificationDeliveryValidatorContext, value time.Time) error
	// When a channel accepted it, which is not the same as it arriving.
	SentAt func(ctx context.Context, c *NotificationDeliveryValidatorContext, value *time.Time) error
	// What the channel said last time. Kept on a retry as well as on a failure, so
	// a pattern is visible before the cap is reached.
	FailedReason func(ctx context.Context, c *NotificationDeliveryValidatorContext, value *string) error
	// How many times this has been claimed. Past notifications.max_attempts it is
	// Failed and stops being claimed.
	Attempts func(ctx context.Context, c *NotificationDeliveryValidatorContext, value int) error
	// When a dispatcher took it. Past notifications.claim_ttl another one may,
	// which is what makes a crashed process recoverable.
	ClaimedAt func(ctx context.Context, c *NotificationDeliveryValidatorContext, value *time.Time) error
	// Which process holds the lease. A uuid generated once per process, with the
	// hostname beside it in the log line, so a stuck lease traces to a pod rather
	// than to a mystery.
	ClaimedBy func(ctx context.Context, c *NotificationDeliveryValidatorContext, value *uuid.UUID) error

	// Entity runs after the per-field hooks, for a rule that is about the row
	// rather than about one column.
	Entity func(ctx context.Context, c *NotificationDeliveryValidatorContext) error
}

// RunCreate implements [dbhook.CreateValidator]: it runs the service's rules
// against the row this input would produce.
//
// The generated checks are not repeated here. The repository runs Normalize
// and Validate first, so by the time a hook sees the input it is tidy and the
// schema is satisfied — which is what lets a hook be about the business
// rather than about NOT NULL.
func (v NotificationDeliveryCreateValidator) RunCreate(ctx context.Context, claims tenancy.Claims, i *NotificationDeliveryCreateInput) error {
	// Everything is new, so everything counts as changed.
	c := &NotificationDeliveryValidatorContext{Claims: claims, changed: map[string]bool{}}
	c.Values.RecipientID = i.RecipientID
	c.changed[ColumnNotificationDeliveryRecipientID] = true
	c.Values.AccountID = i.AccountID
	c.changed[ColumnNotificationDeliveryAccountID] = true
	c.Values.Channel = i.Channel
	c.changed[ColumnNotificationDeliveryChannel] = true
	c.Values.Kind = i.Kind
	c.changed[ColumnNotificationDeliveryKind] = true
	c.Values.Digest = i.Digest
	c.changed[ColumnNotificationDeliveryDigest] = true
	c.Values.State = i.State
	c.changed[ColumnNotificationDeliveryState] = true
	c.Values.DeliverAt = i.DeliverAt
	c.changed[ColumnNotificationDeliveryDeliverAt] = true
	c.Values.SentAt = i.SentAt
	c.changed[ColumnNotificationDeliverySentAt] = true
	c.Values.FailedReason = i.FailedReason
	c.changed[ColumnNotificationDeliveryFailedReason] = true
	c.Values.Attempts = i.Attempts
	c.changed[ColumnNotificationDeliveryAttempts] = true
	c.Values.ClaimedAt = i.ClaimedAt
	c.changed[ColumnNotificationDeliveryClaimedAt] = true
	c.Values.ClaimedBy = i.ClaimedBy
	c.changed[ColumnNotificationDeliveryClaimedBy] = true

	failed, err := v.run(ctx, c)
	if err != nil {
		return err
	}
	if failed != nil {
		return failed
	}
	return nil
}

// run calls every configured hook and puts what each one said under the field
// it was about.
//
// Two kinds of answer. A [rigerr.FieldError] is about the input: it lands on
// the field and the others still run, so one request reports everything wrong
// with it. Anything else is the rule itself failing — a lookup that could
// not reach another service — and there is nothing to tell the caller about
// their input, so it comes back wrapped with the rule that could not be run,
// keeping whatever code it carried and becoming Internal if it carried none.
func (v NotificationDeliveryCreateValidator) run(ctx context.Context, c *NotificationDeliveryValidatorContext) (*NotificationDeliveryCreateInputError, error) {
	var failed NotificationDeliveryCreateInputError

	if v.RecipientID != nil {
		if err := v.RecipientID(ctx, c, c.Values.RecipientID); err != nil {
			field, ok := rigerr.AsFieldError(err)
			if !ok {
				return nil, rigerr.Wrap(err, "validate recipient_id")
			}
			failed.RecipientID = field
		}
	}
	if v.AccountID != nil {
		if err := v.AccountID(ctx, c, c.Values.AccountID); err != nil {
			field, ok := rigerr.AsFieldError(err)
			if !ok {
				return nil, rigerr.Wrap(err, "validate account_id")
			}
			failed.AccountID = field
		}
	}
	if v.Channel != nil {
		if err := v.Channel(ctx, c, c.Values.Channel); err != nil {
			field, ok := rigerr.AsFieldError(err)
			if !ok {
				return nil, rigerr.Wrap(err, "validate channel")
			}
			failed.Channel = field
		}
	}
	if v.Kind != nil {
		if err := v.Kind(ctx, c, c.Values.Kind); err != nil {
			field, ok := rigerr.AsFieldError(err)
			if !ok {
				return nil, rigerr.Wrap(err, "validate kind")
			}
			failed.Kind = field
		}
	}
	if v.Digest != nil {
		if err := v.Digest(ctx, c, c.Values.Digest); err != nil {
			field, ok := rigerr.AsFieldError(err)
			if !ok {
				return nil, rigerr.Wrap(err, "validate digest")
			}
			failed.Digest = field
		}
	}
	if v.State != nil {
		if err := v.State(ctx, c, c.Values.State); err != nil {
			field, ok := rigerr.AsFieldError(err)
			if !ok {
				return nil, rigerr.Wrap(err, "validate state")
			}
			failed.State = field
		}
	}
	if v.DeliverAt != nil {
		if err := v.DeliverAt(ctx, c, c.Values.DeliverAt); err != nil {
			field, ok := rigerr.AsFieldError(err)
			if !ok {
				return nil, rigerr.Wrap(err, "validate deliver_at")
			}
			failed.DeliverAt = field
		}
	}
	if v.SentAt != nil {
		if err := v.SentAt(ctx, c, c.Values.SentAt); err != nil {
			field, ok := rigerr.AsFieldError(err)
			if !ok {
				return nil, rigerr.Wrap(err, "validate sent_at")
			}
			failed.SentAt = field
		}
	}
	if v.FailedReason != nil {
		if err := v.FailedReason(ctx, c, c.Values.FailedReason); err != nil {
			field, ok := rigerr.AsFieldError(err)
			if !ok {
				return nil, rigerr.Wrap(err, "validate failed_reason")
			}
			failed.FailedReason = field
		}
	}
	if v.Attempts != nil {
		if err := v.Attempts(ctx, c, c.Values.Attempts); err != nil {
			field, ok := rigerr.AsFieldError(err)
			if !ok {
				return nil, rigerr.Wrap(err, "validate attempts")
			}
			failed.Attempts = field
		}
	}
	if v.ClaimedAt != nil {
		if err := v.ClaimedAt(ctx, c, c.Values.ClaimedAt); err != nil {
			field, ok := rigerr.AsFieldError(err)
			if !ok {
				return nil, rigerr.Wrap(err, "validate claimed_at")
			}
			failed.ClaimedAt = field
		}
	}
	if v.ClaimedBy != nil {
		if err := v.ClaimedBy(ctx, c, c.Values.ClaimedBy); err != nil {
			field, ok := rigerr.AsFieldError(err)
			if !ok {
				return nil, rigerr.Wrap(err, "validate claimed_by")
			}
			failed.ClaimedBy = field
		}
	}

	if v.Entity != nil {
		if err := v.Entity(ctx, c); err != nil {
			field, ok := rigerr.AsFieldError(err)
			if !ok {
				return nil, rigerr.Wrap(err, "validate rig_notification_delivery")
			}
			failed.Entity = field
		}
	}

	if failed.Empty() {
		return nil, nil
	}
	return &failed, nil
}

// NotificationDeliveryUpdateValidator is the rules for changing one that
// already exists: what the schema cannot express.
//
// One optional function per field this operation can set, so the set of fields
// is the set of rules that could apply — a column an update cannot touch has
// no hook here to write by mistake. A nil one is skipped. Every configured
// hook runs, because a rule that fails should not hide the next one, so a
// request reports everything wrong with it.
//
// A hook returns a FieldError to attach the message to a specific field, or
// any other error to fail the request outright.
type NotificationDeliveryUpdateValidator struct {
	// The inbox line this is a copy of. The line is the truth and this is one way
	// it was repeated.
	RecipientID func(ctx context.Context, c *NotificationDeliveryValidatorContext, value uuid.UUID) error
	// Who it is for, denormalized off the line so a claim can group by it without
	// a join.
	AccountID func(ctx context.Context, c *NotificationDeliveryValidatorContext, value uuid.UUID) error
	// Where it is going. One row per line per channel, which is what the unique
	// index below enforces.
	Channel func(ctx context.Context, c *NotificationDeliveryValidatorContext, value NotificationChannel) error
	// Copied from the notification, so the settings resolution needs no join
	// either.
	Kind func(ctx context.Context, c *NotificationDeliveryValidatorContext, value string) error
	// What the setting said when this row was written. Immediate rows are sent on
	// their own; the rest are batched per account and channel.
	Digest func(ctx context.Context, c *NotificationDeliveryValidatorContext, value NotificationDigest) error
	// Skipped means a setting refused it, which is different from Failed and worth
	// telling apart in a report.
	State func(ctx context.Context, c *NotificationDeliveryValidatorContext, value NotificationDeliveryState) error
	// When this is due. A delivery held outside somebody hours moves to the next
	// opening; a digested one moves to its window close.
	DeliverAt func(ctx context.Context, c *NotificationDeliveryValidatorContext, value time.Time) error
	// When a channel accepted it, which is not the same as it arriving.
	SentAt func(ctx context.Context, c *NotificationDeliveryValidatorContext, value *time.Time) error
	// What the channel said last time. Kept on a retry as well as on a failure, so
	// a pattern is visible before the cap is reached.
	FailedReason func(ctx context.Context, c *NotificationDeliveryValidatorContext, value *string) error
	// How many times this has been claimed. Past notifications.max_attempts it is
	// Failed and stops being claimed.
	Attempts func(ctx context.Context, c *NotificationDeliveryValidatorContext, value int) error
	// When a dispatcher took it. Past notifications.claim_ttl another one may,
	// which is what makes a crashed process recoverable.
	ClaimedAt func(ctx context.Context, c *NotificationDeliveryValidatorContext, value *time.Time) error
	// Which process holds the lease. A uuid generated once per process, with the
	// hostname beside it in the log line, so a stuck lease traces to a pod rather
	// than to a mystery.
	ClaimedBy func(ctx context.Context, c *NotificationDeliveryValidatorContext, value *uuid.UUID) error

	// Entity runs after the per-field hooks, for a rule that is about the row
	// rather than about one column.
	Entity func(ctx context.Context, c *NotificationDeliveryValidatorContext) error
}

// RunUpdate implements [dbhook.UpdateValidator]: it runs the service's rules
// against the row this update would produce, with the row as it was available
// for a rule about the change itself.
func (v NotificationDeliveryUpdateValidator) RunUpdate(ctx context.Context, claims tenancy.Claims, i *NotificationDeliveryUpdateInput, prev *NotificationDelivery) error {
	c := &NotificationDeliveryValidatorContext{
		Values:   i.Merged(prev),
		Claims:   claims,
		previous: *prev,
		isUpdate: true,
		changed:  map[string]bool{},
	}
	c.changed[ColumnNotificationDeliveryRecipientID] = i.RecipientID.IsSet()
	c.changed[ColumnNotificationDeliveryAccountID] = i.AccountID.IsSet()
	c.changed[ColumnNotificationDeliveryChannel] = i.Channel.IsSet()
	c.changed[ColumnNotificationDeliveryKind] = i.Kind.IsSet()
	c.changed[ColumnNotificationDeliveryDigest] = i.Digest.IsSet()
	c.changed[ColumnNotificationDeliveryState] = i.State.IsSet()
	c.changed[ColumnNotificationDeliveryDeliverAt] = i.DeliverAt.IsSet()
	c.changed[ColumnNotificationDeliverySentAt] = i.SentAt.Touched()
	c.changed[ColumnNotificationDeliveryFailedReason] = i.FailedReason.Touched()
	c.changed[ColumnNotificationDeliveryAttempts] = i.Attempts.IsSet()
	c.changed[ColumnNotificationDeliveryClaimedAt] = i.ClaimedAt.Touched()
	c.changed[ColumnNotificationDeliveryClaimedBy] = i.ClaimedBy.Touched()

	failed, err := v.run(ctx, c)
	if err != nil {
		return err
	}
	if failed != nil {
		return failed
	}
	return nil
}

// run calls every configured hook and puts what each one said under the field
// it was about.
//
// Two kinds of answer. A [rigerr.FieldError] is about the input: it lands on
// the field and the others still run, so one request reports everything wrong
// with it. Anything else is the rule itself failing — a lookup that could
// not reach another service — and there is nothing to tell the caller about
// their input, so it comes back wrapped with the rule that could not be run,
// keeping whatever code it carried and becoming Internal if it carried none.
func (v NotificationDeliveryUpdateValidator) run(ctx context.Context, c *NotificationDeliveryValidatorContext) (*NotificationDeliveryUpdateInputError, error) {
	var failed NotificationDeliveryUpdateInputError

	if v.RecipientID != nil {
		if err := v.RecipientID(ctx, c, c.Values.RecipientID); err != nil {
			field, ok := rigerr.AsFieldError(err)
			if !ok {
				return nil, rigerr.Wrap(err, "validate recipient_id")
			}
			failed.RecipientID = field
		}
	}
	if v.AccountID != nil {
		if err := v.AccountID(ctx, c, c.Values.AccountID); err != nil {
			field, ok := rigerr.AsFieldError(err)
			if !ok {
				return nil, rigerr.Wrap(err, "validate account_id")
			}
			failed.AccountID = field
		}
	}
	if v.Channel != nil {
		if err := v.Channel(ctx, c, c.Values.Channel); err != nil {
			field, ok := rigerr.AsFieldError(err)
			if !ok {
				return nil, rigerr.Wrap(err, "validate channel")
			}
			failed.Channel = field
		}
	}
	if v.Kind != nil {
		if err := v.Kind(ctx, c, c.Values.Kind); err != nil {
			field, ok := rigerr.AsFieldError(err)
			if !ok {
				return nil, rigerr.Wrap(err, "validate kind")
			}
			failed.Kind = field
		}
	}
	if v.Digest != nil {
		if err := v.Digest(ctx, c, c.Values.Digest); err != nil {
			field, ok := rigerr.AsFieldError(err)
			if !ok {
				return nil, rigerr.Wrap(err, "validate digest")
			}
			failed.Digest = field
		}
	}
	if v.State != nil {
		if err := v.State(ctx, c, c.Values.State); err != nil {
			field, ok := rigerr.AsFieldError(err)
			if !ok {
				return nil, rigerr.Wrap(err, "validate state")
			}
			failed.State = field
		}
	}
	if v.DeliverAt != nil {
		if err := v.DeliverAt(ctx, c, c.Values.DeliverAt); err != nil {
			field, ok := rigerr.AsFieldError(err)
			if !ok {
				return nil, rigerr.Wrap(err, "validate deliver_at")
			}
			failed.DeliverAt = field
		}
	}
	if v.SentAt != nil {
		if err := v.SentAt(ctx, c, c.Values.SentAt); err != nil {
			field, ok := rigerr.AsFieldError(err)
			if !ok {
				return nil, rigerr.Wrap(err, "validate sent_at")
			}
			failed.SentAt = field
		}
	}
	if v.FailedReason != nil {
		if err := v.FailedReason(ctx, c, c.Values.FailedReason); err != nil {
			field, ok := rigerr.AsFieldError(err)
			if !ok {
				return nil, rigerr.Wrap(err, "validate failed_reason")
			}
			failed.FailedReason = field
		}
	}
	if v.Attempts != nil {
		if err := v.Attempts(ctx, c, c.Values.Attempts); err != nil {
			field, ok := rigerr.AsFieldError(err)
			if !ok {
				return nil, rigerr.Wrap(err, "validate attempts")
			}
			failed.Attempts = field
		}
	}
	if v.ClaimedAt != nil {
		if err := v.ClaimedAt(ctx, c, c.Values.ClaimedAt); err != nil {
			field, ok := rigerr.AsFieldError(err)
			if !ok {
				return nil, rigerr.Wrap(err, "validate claimed_at")
			}
			failed.ClaimedAt = field
		}
	}
	if v.ClaimedBy != nil {
		if err := v.ClaimedBy(ctx, c, c.Values.ClaimedBy); err != nil {
			field, ok := rigerr.AsFieldError(err)
			if !ok {
				return nil, rigerr.Wrap(err, "validate claimed_by")
			}
			failed.ClaimedBy = field
		}
	}

	if v.Entity != nil {
		if err := v.Entity(ctx, c); err != nil {
			field, ok := rigerr.AsFieldError(err)
			if !ok {
				return nil, rigerr.Wrap(err, "validate rig_notification_delivery")
			}
			failed.Entity = field
		}
	}

	if failed.Empty() {
		return nil, nil
	}
	return &failed, nil
}
