package notifymodel

import (
	"strings"
)

// How often somebody wants to be told, on one channel.
type NotificationDigest string

// The values of NotificationDigest.
const (
	// Each notification on its own, as soon as it is due.
	NotificationDigestImmediate NotificationDigest = "Immediate"
	// Whatever accumulated in an hour, as one message.
	NotificationDigestHourly NotificationDigest = "Hourly"
	// The same, a day at a time.
	NotificationDigestDaily NotificationDigest = "Daily"
	// The widest window, and the one notifications.retention has to outlive — a
	// weekly digest is assembled from rows a shorter retention would have pruned.
	NotificationDigestWeekly NotificationDigest = "Weekly"
	// Nothing on this channel, and the inbox line is written anyway: this is
	// somebody preferring to look rather than be told. is_enabled false is the
	// other thing, and says the channel is not available to them at all.
	NotificationDigestOff NotificationDigest = "Off"
)

// AllNotificationDigest is every value, in declaration order.
var AllNotificationDigest = []NotificationDigest{NotificationDigestImmediate, NotificationDigestHourly, NotificationDigestDaily, NotificationDigestWeekly, NotificationDigestOff}

// Valid reports whether the value is one the database will accept.
func (v NotificationDigest) Valid() bool {
	switch v {
	case NotificationDigestImmediate, NotificationDigestHourly, NotificationDigestDaily, NotificationDigestWeekly, NotificationDigestOff:
		return true
	default:
		return false
	}
}

// ParseNotificationDigest reads a value, accepting any casing and surrounding
// space.
//
// Normalization uses it, so "IN_PROGRESS" from one client and "in_progress"
// from another mean the same thing rather than one of them being a validation
// failure nobody can explain. The spelling still has to be the label’s: a
// value is a name the database knows, not a phrase.
func ParseNotificationDigest(s string) (NotificationDigest, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "immediate":
		return NotificationDigestImmediate, true
	case "hourly":
		return NotificationDigestHourly, true
	case "daily":
		return NotificationDigestDaily, true
	case "weekly":
		return NotificationDigestWeekly, true
	case "off":
		return NotificationDigestOff, true
	default:
		return "", false
	}
}

// String implements fmt.Stringer.
func (v NotificationDigest) String() string { return string(v) }
