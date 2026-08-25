package notify

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Channel is the three-way switch of a delivery, and every one of them is a
// *copy* of an inbox line sent somewhere else.
//
// The inbox is deliberately not one. It is the table this module writes, it is
// always on, and a switch that turned it off would produce a notification nobody
// can ever find: the badge would be wrong, the count would be wrong, and the row
// would sit there unread forever. Everything here can be refused because
// everything here is a copy.
type Channel string

const (
	// ChannelDesktop and ChannelMobile are separate rather than one push
	// channel with a platform column on the device, and that is the whole reason
	// to name them this way: they are separately *answerable*. "Not on my phone
	// during dinner, yes on my laptop while I am working" is the setting people
	// reach for, and a platform on a device row cannot express it — the platform
	// says where a token points, and the question is what a person wants.
	ChannelDesktop Channel = "Desktop"
	// ChannelMobile is the other half of that pair.
	ChannelMobile Channel = "Mobile"
	// ChannelEmail has no device rows. The address is on the account and on the
	// identity already, and a third copy of it is a third thing that can
	// disagree.
	ChannelEmail Channel = "Email"
)

// Channels are all three, in the order a report lists them.
func Channels() []Channel { return []Channel{ChannelDesktop, ChannelMobile, ChannelEmail} }

// Digest is how often somebody wants to hear from a channel.
type Digest string

const (
	// DigestImmediate sends each delivery on its own, as soon as it is due.
	DigestImmediate Digest = "Immediate"
	// DigestHourly waits an hour and sends whatever accumulated as one message.
	DigestHourly Digest = "Hourly"
	// DigestDaily is the same, a day at a time.
	DigestDaily Digest = "Daily"
	// DigestWeekly is the widest window, and the one retention has to outlive:
	// a weekly digest under a daily retention is assembled from rows that were
	// pruned before it ran.
	DigestWeekly Digest = "Weekly"
	// DigestOff sends nothing on this channel and still writes the inbox line —
	// the person will see it when they look.
	//
	// It is not the same as `is_enabled: false`, and the difference is worth a
	// sentence because somebody will set the wrong one. Both mean "no mail"
	// today. `Off` is about this person preferring to look rather than be told;
	// `is_enabled: false` is about the channel being unavailable to them, and it
	// would stop meaning the same thing if a future channel ever needed to
	// refuse the inbox line too.
	DigestOff Digest = "Off"
)

// Window is how long a digest waits before it goes out as one message.
//
// Immediate and Off have none: one sends now and the other never sends.
func (d Digest) Window() (time.Duration, bool) {
	switch d {
	case DigestHourly:
		return time.Hour, true
	case DigestDaily:
		return 24 * time.Hour, true
	case DigestWeekly:
		return 7 * 24 * time.Hour, true
	default:
		return 0, false
	}
}

// DeliveryState is where one copy is in its life.
type DeliveryState string

const (
	// DeliveryPending is owed and not yet claimed, or claimed by a process that
	// has not marked it.
	DeliveryPending DeliveryState = "Pending"
	// DeliverySent means a channel accepted it, which is not the same as it
	// arriving — and rig does not pretend to know the difference.
	DeliverySent DeliveryState = "Sent"
	// DeliveryFailed is past max_attempts and stops being claimed. Without the
	// cap a permanently broken address consumes a lease and a log line forever.
	DeliveryFailed DeliveryState = "Failed"
	// DeliverySkipped is a setting that refused it, which is a different answer
	// from Failed and worth telling apart in a report.
	DeliverySkipped DeliveryState = "Skipped"
)

// Delivery is one copy of an inbox line, on its way.
type Delivery struct {
	// ID is stable across retries, and it is the whole of what rig can do about
	// exactly-once.
	//
	// Hand it to the provider as its own idempotency key — Message-ID, apns-id,
	// whatever the SDK calls it. rig cannot enforce that, and saying so is
	// better than implying exactly-once and letting somebody find out: the send
	// and the bookkeeping are two systems, no transaction spans both, and a
	// process that handed a message over and died will hand it over again when
	// its lease expires.
	ID uuid.UUID

	TenantID    uuid.UUID
	AccountID   uuid.UUID
	RecipientID uuid.UUID

	Channel Channel
	Kind    string
	// Digest is what the setting said when this row was written. Immediate rows
	// go out on their own; the rest are batched per account and channel.
	Digest Digest

	// Attempts is how many times this has been claimed, including this one.
	Attempts  int
	DeliverAt time.Time
}

// Device is somewhere a push can land.
type Device struct {
	ID        uuid.UUID
	AccountID uuid.UUID
	Channel   Channel
	// Token is what the provider was given. Opaque to rig.
	Token string
	// Label is what a person sees in a list of their devices, and the field that
	// makes revoking one possible for somebody who has four.
	Label      string
	LastSeenAt *time.Time
}

// Message is what a channel is handed.
//
// A slice rather than one delivery, because a digest is the same call with more
// in it: an Hourly account with nine pending deliveries gets one message
// containing nine, and an Immediate account beside it gets nine messages of one.
// The channel decides what to say with what it is given — "you have 2 unread
// notifications" and a link to the inbox is the obvious rendering, and rig does
// not write it, for the reason it writes no other template.
type Message struct {
	Channel   Channel
	AccountID uuid.UUID
	TenantID  uuid.UUID

	// Deliveries are what this message stands for, oldest first. One for an
	// immediate send; several for a digest.
	Deliveries []Delivery

	// Devices are where to send, for a push channel. Empty for Email, whose
	// address is on the account.
	Devices []Device
	// EmailAddress is where to send for Email, and empty otherwise.
	EmailAddress string
}

// Sender is what an application implements to actually send something.
//
// rig ships no transport: no SMTP, no APNs, no FCM, no web-push, and no
// dependency for any of them. That is the bargain the mail notifier already
// makes, repeated without alteration — what rig knows is who is owed what and
// when, and every provider decision after that is one rig would get wrong.
//
// Returning an error retries with backoff until max_attempts. Returning nil
// means the provider accepted it, which is not the same as it arriving, and rig
// does not pretend to know the difference.
//
// A bare error gets the ordinary schedule, which is the right default: "try
// again" is the safe answer when nobody said otherwise. Two things are worth
// saying instead of it, and both are optional — [Permanent] for a provider
// refusing the recipient rather than the request, which fails the delivery on
// this attempt instead of spending eight hours on an address that does not
// exist, and [RetryAfter] for a provider that named the time to come back. A
// Sender written before those existed is a correct Sender.
//
// **The context carries a deadline — notifications.send_timeout, thirty seconds
// by default — and honouring it is this method's job.** rig cannot enforce it,
// for the same reason it cannot enforce that [Delivery.ID] reaches the provider:
// what happens after this call is application code. Pass ctx to the request, and
// do not hand a provider SDK a client with no timeout of its own.
//
// What a Sender that ignores it costs is worth stating, because the shape hides
// it. A pass is one goroutine and it resolves before it dispatches, so a call
// that never returns stops this replica writing inbox lines and sending on
// *every* channel — not just this one — until the process is restarted. The
// deadline exists to make that a failed delivery instead. It is the only
// protection there is, and it is cooperative.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// SenderFunc adapts a function to [Sender].
type SenderFunc func(ctx context.Context, m Message) error

// Send implements Sender.
func (f SenderFunc) Send(ctx context.Context, m Message) error { return f(ctx, m) }

// NoSender refuses to send anything, and says so.
//
// It is the default, and it fails rather than discarding, because a production
// deployment whose notifications all silently succeeded into nothing is worse
// than one that refused to start. A project that genuinely wants no delivery
// leaves the channel out of the engine's map, which writes no delivery rows at
// all — and the inbox still works, because the inbox is not a channel.
type NoSender struct{ Channel Channel }

// Send implements Sender by refusing.
func (n NoSender) Send(context.Context, Message) error {
	return &noSenderError{channel: n.Channel}
}

type noSenderError struct{ channel Channel }

func (e *noSenderError) Error() string {
	return "notify: no sender is configured for the " + string(e.channel) +
		" channel, so nothing was sent; register one or leave the channel out entirely"
}
