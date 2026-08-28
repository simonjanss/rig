package notifymodel

import (
	"strings"
)

// What happened to one copy of an inbox line on its way to a channel.
type NotificationDeliveryState string

// The values of NotificationDeliveryState.
const (
	// Owed and not yet claimed, or claimed by a dispatcher that has not marked it.
	NotificationDeliveryStatePending NotificationDeliveryState = "Pending"
	// A channel accepted it, which is not the same as it arriving — rig does not
	// pretend to know the difference.
	NotificationDeliveryStateSent NotificationDeliveryState = "Sent"
	// Past notifications.max_attempts, and no longer claimed. Without the cap a
	// permanently broken address would consume a lease forever.
	NotificationDeliveryStateFailed NotificationDeliveryState = "Failed"
	// A setting refused it, or the row it was a copy of was retired before it
	// went. Worth telling apart from Failed in a report: it is 'we decided against
	// this' rather than 'this did not work'.
	NotificationDeliveryStateSkipped NotificationDeliveryState = "Skipped"
)

// AllNotificationDeliveryState is every value, in declaration order.
var AllNotificationDeliveryState = []NotificationDeliveryState{NotificationDeliveryStatePending, NotificationDeliveryStateSent, NotificationDeliveryStateFailed, NotificationDeliveryStateSkipped}

// Valid reports whether the value is one the database will accept.
func (v NotificationDeliveryState) Valid() bool {
	switch v {
	case NotificationDeliveryStatePending, NotificationDeliveryStateSent, NotificationDeliveryStateFailed, NotificationDeliveryStateSkipped:
		return true
	default:
		return false
	}
}

// ParseNotificationDeliveryState reads a value, accepting any casing and
// surrounding space.
//
// Normalization uses it, so "IN_PROGRESS" from one client and "in_progress"
// from another mean the same thing rather than one of them being a validation
// failure nobody can explain. The spelling still has to be the label’s: a
// value is a name the database knows, not a phrase.
func ParseNotificationDeliveryState(s string) (NotificationDeliveryState, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "pending":
		return NotificationDeliveryStatePending, true
	case "sent":
		return NotificationDeliveryStateSent, true
	case "failed":
		return NotificationDeliveryStateFailed, true
	case "skipped":
		return NotificationDeliveryStateSkipped, true
	default:
		return "", false
	}
}

// String implements fmt.Stringer.
func (v NotificationDeliveryState) String() string { return string(v) }
