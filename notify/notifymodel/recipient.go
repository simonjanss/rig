package notifymodel

import (
	"time"

	"github.com/google/uuid"
)

// One inbox line: a notification, an account, and whether it has been read.
//
// It is the one definition of a NotificationRecipient: the repository scans
// into it and the API returns it, so there is no conversion between two shapes
// of the same thing and no field that can go missing from one of them.
type NotificationRecipient struct {
	// Unique identifier for this row.
	ID uuid.UUID `json:"id"`
	// Tenant this row belongs to. Every query is scoped by it.
	TenantID uuid.UUID `json:"tenantId"`
	// What happened. The line is separate from it so that one person clearing
	// their inbox changes nothing anybody else sees.
	NotificationID uuid.UUID `json:"notificationId"`
	// Who this line is for. An account rather than an identity: an identity has no
	// tenant, so a row addressed to one would fall outside every generated query.
	AccountID uuid.UUID `json:"accountId"`
	// When this row was created. Set automatically and never changes.
	CreatedAt time.Time `json:"createdAt"`
	// When this row was last updated. Set automatically on every update.
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
	// When this row was soft-deleted. Null while the row is live.
	DeletedAt *time.Time `json:"deletedAt,omitempty"`
	// Account that soft-deleted this row, taken from the request's claims.
	DeletedByAccountID *uuid.UUID `json:"deletedByAccountId,omitempty"`
	// Copied from the notification, so the inbox and its live-sync shape never
	// touch a table holding rows for people who are not recipients.
	Kind string `json:"kind"`
	// What collapses several events into one line. Null opts out and every event
	// is its own row.
	GroupKey *string `json:"groupKey,omitempty"`
	// How many events this line stands for. One unless a group key collapsed them.
	EventCount int `json:"eventCount"`
	// When the person read it. Null is unread, which is what the badge counts.
	ReadAt *time.Time `json:"readAt,omitempty"`
}

// TableNotificationRecipient is the table this entity is stored in.
const TableNotificationRecipient = "rig_notification_recipient"

// Column names for rig_notification_recipient, so nothing has to spell one
// out.
const (
	ColumnNotificationRecipientID                 = "id"
	ColumnNotificationRecipientTenantID           = "tenant_id"
	ColumnNotificationRecipientNotificationID     = "notification_id"
	ColumnNotificationRecipientAccountID          = "account_id"
	ColumnNotificationRecipientCreatedAt          = "created_at"
	ColumnNotificationRecipientUpdatedAt          = "updated_at"
	ColumnNotificationRecipientDeletedAt          = "deleted_at"
	ColumnNotificationRecipientDeletedByAccountID = "deleted_by_account_id"
	ColumnNotificationRecipientKind               = "kind"
	ColumnNotificationRecipientGroupKey           = "group_key"
	ColumnNotificationRecipientEventCount         = "event_count"
	ColumnNotificationRecipientReadAt             = "read_at"
)

// NotificationRecipientColumns is every column, in the order the row is
// scanned.
var NotificationRecipientColumns = []string{"id", "tenant_id", "notification_id", "account_id", "created_at", "updated_at", "deleted_at", "deleted_by_account_id", "kind", "group_key", "event_count", "read_at"}
