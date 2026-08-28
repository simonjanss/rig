package notifymodel

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Something worth telling somebody about, and when it is due. Carries no
// recipients: the audience is computed when it is sent.
//
// It is the one definition of a Notification: the repository scans into it and
// the API returns it, so there is no conversion between two shapes of the same
// thing and no field that can go missing from one of them.
type Notification struct {
	// Unique identifier for this row.
	ID uuid.UUID `json:"id"`
	// Tenant this row belongs to. Every query is scoped by it.
	TenantID uuid.UUID `json:"tenantId"`
	// When this row was created. Set automatically and never changes.
	CreatedAt time.Time `json:"createdAt"`
	// Account that created this row, taken from the request's claims.
	CreatedByAccountID *uuid.UUID `json:"createdByAccountId,omitempty"`
	// API key this row was created through, when it was an integration rather than
	// a person.
	CreatedByAPIKeyID *uuid.UUID `json:"createdByApiKeyId,omitempty"`
	// When this row was last updated. Set automatically on every update.
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
	// What happened, as the application names it. Narrow it to an enum of your own
	// to get a switch the compiler can see.
	Kind string `json:"kind"`
	// Resolved means the audience was determined and the inbox lines exist. It
	// does not mean anything was sent.
	State NotificationState `json:"state"`
	// When this is due. now() is the ordinary case; a scheduled notification is
	// the same row with a later time, which is the only difference between the
	// two.
	DeliverAt time.Time `json:"deliverAt"`
	// When the audience was computed. Null while the notification is still pending
	// or was cancelled.
	ResolvedAt *time.Time `json:"resolvedAt,omitempty"`
	// What a template needs beyond the linked row. Give it a Go type with the
	// go_type key if it has a shape.
	Payload json.RawMessage `json:"payload"`
	// What collapses several of these into one inbox line, decided when the
	// announcement was written and copied onto the line when the audience is
	// resolved.
	GroupKey *string `json:"groupKey,omitempty"`
	// A recipient list captured at write time, and the one exception to computing
	// the audience late. Null is the ordinary case; a list here skips the question
	// entirely, for an audience that genuinely cannot be re-derived.
	AccountIds []uuid.UUID `json:"accountIds,omitempty"`
	// When a dispatcher took this to resolve. Past notifications.claim_ttl another
	// may, which is what makes a crashed process recoverable.
	ClaimedAt *time.Time `json:"claimedAt,omitempty"`
	// Which process holds the lease, so a stuck one traces to a pod rather than to
	// a mystery.
	ClaimedBy *uuid.UUID `json:"claimedBy,omitempty"`
	// How many times resolving this has been attempted.
	Attempts int `json:"attempts"`
}

// TableNotification is the table this entity is stored in.
const TableNotification = "rig_notification"

// Column names for rig_notification, so nothing has to spell one out.
const (
	ColumnNotificationID                 = "id"
	ColumnNotificationTenantID           = "tenant_id"
	ColumnNotificationCreatedAt          = "created_at"
	ColumnNotificationCreatedByAccountID = "created_by_account_id"
	ColumnNotificationCreatedByAPIKeyID  = "created_by_api_key_id"
	ColumnNotificationUpdatedAt          = "updated_at"
	ColumnNotificationKind               = "kind"
	ColumnNotificationState              = "state"
	ColumnNotificationDeliverAt          = "deliver_at"
	ColumnNotificationResolvedAt         = "resolved_at"
	ColumnNotificationPayload            = "payload"
	ColumnNotificationGroupKey           = "group_key"
	ColumnNotificationAccountIds         = "account_ids"
	ColumnNotificationClaimedAt          = "claimed_at"
	ColumnNotificationClaimedBy          = "claimed_by"
	ColumnNotificationAttempts           = "attempts"
)

// NotificationColumns is every column, in the order the row is scanned.
var NotificationColumns = []string{"id", "tenant_id", "created_at", "created_by_account_id", "created_by_api_key_id", "updated_at", "kind", "state", "deliver_at", "resolved_at", "payload", "group_key", "account_ids", "claimed_at", "claimed_by", "attempts"}
