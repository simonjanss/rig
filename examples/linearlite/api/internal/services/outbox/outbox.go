// Package outbox is the mail this example would have sent.
//
// Two interfaces meet here, and they are the two halves of "somebody was told
// something".
//
// A [github.com/simonjanss/rig/auth/account.Notifier] delivers the single-use
// links the auth package mints: invitations, password resets, address
// confirmations. A [github.com/simonjanss/rig/notify.Sender] delivers a copy of
// an inbox line to a channel. rig ships no transport for either — no SMTP, no
// APNs, no dependency for one — because what rig knows is who is owed what and
// when, and every provider decision after that is one it would get wrong.
//
// This box keeps the last few in memory so the front end can show them. That is
// exactly what a real implementation must never do, and the reason it is
// acceptable here is the reason it is interesting: a live invitation is a
// credential for as long as it lives, and putting one on a screen is putting a
// credential on a screen. It is here so that the reset flow and the invitation
// flow can be walked end to end without a mail server, and the page that shows
// them says so at the top.
package outbox

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/notify"
)

// Kind is what a message is.
type Kind string

// The kinds. The first three are links the account service minted; the last is
// a copy of an inbox line, which is a different thing arriving by the same road.
const (
	KindInvitation   Kind = "Invitation"
	KindReset        Kind = "PasswordReset"
	KindVerification Kind = "EmailVerification"
	KindNotification Kind = "Notification"
)

// Message is one thing that would have been sent.
// The tags are the wire's, matching what every other route here answers with:
// this is read by the front end over /_demo/outbox, and one endpoint spelling
// its keys differently from the rest is one thing for somebody to trip over.
type Message struct {
	Kind Kind `json:"kind"`
	// Channel is which channel carried this, and empty for the auth package's
	// links — those are not a channel, they are a single-use URL that had to
	// reach one address.
	Channel string `json:"channel,omitempty"`
	// To is the address, which for a link is the identity's and for a
	// notification is whatever the account service resolved.
	To          string `json:"to"`
	DisplayName string `json:"displayName"`
	// Token is the single-use secret in the link, and empty for a
	// notification. It is what makes this screen a demonstration rather than a
	// feature.
	Token string `json:"token"`
	// Subject is the one line a real template would have rendered from what the
	// channel was handed.
	Subject string `json:"subject"`
	// Devices are where a push would have gone, labelled as the person labelled
	// them. Empty for email, whose address is on the account — which is the
	// whole difference between the two kinds of channel, and the reason the
	// device table exists at all.
	Devices []string `json:"devices,omitempty"`
	// DeliveryIDs are what a real transport is obliged to hand the provider as
	// its own idempotency key — Message-ID, apns-id, whatever the SDK calls it.
	// rig cannot enforce that, so showing them is the next best thing: this is
	// the value that makes a redelivery after a crash one message instead of
	// two.
	DeliveryIDs []uuid.UUID `json:"deliveryIds"`
	TenantID    *uuid.UUID  `json:"tenantId"`
	At          time.Time   `json:"at"`
}

// Box collects messages, newest first.
//
// The cap is small on purpose: this is a demonstration, not a mailbox, and a
// process that accumulated every link ever minted would be a process holding
// every credential ever minted.
type Box struct {
	mu       sync.Mutex
	messages []Message
	limit    int
}

// New builds a box keeping the last n messages.
func New(n int) *Box {
	if n <= 0 {
		n = 20
	}
	return &Box{limit: n}
}

var _ account.Notifier = (*Box)(nil)

// SendInvitation implements [account.Notifier].
func (b *Box) SendInvitation(_ context.Context, i *account.Identity, a *account.Account, token string) error {
	tenantID := a.TenantID
	b.add(Message{
		Kind: KindInvitation, To: i.EmailAddress, DisplayName: i.DisplayName,
		Subject: "You have been invited to a workspace",
		Token:   token, TenantID: &tenantID,
	})
	return nil
}

// SendPasswordReset implements [account.Notifier].
func (b *Box) SendPasswordReset(_ context.Context, i *account.Identity, token string) error {
	b.add(Message{
		Kind: KindReset, To: i.EmailAddress, DisplayName: i.DisplayName,
		Subject: "Choose a new password",
		Token:   token,
	})
	return nil
}

// SendEmailVerification implements [account.Notifier].
func (b *Box) SendEmailVerification(_ context.Context, i *account.Identity, token string) error {
	b.add(Message{
		Kind: KindVerification, To: i.EmailAddress, DisplayName: i.DisplayName,
		Subject: "Confirm your address",
		Token:   token,
	})
	return nil
}

// NotificationSender is the email channel, and the shape a real one has with
// none of the substance.
//
// One call arrives per account per channel, carrying one delivery for an
// account set to Immediate and several for one set to a digest — so the number
// of deliveries below is the number of things this person is being told at
// once, and deciding what to say with them is the channel's job. rig writes no
// template, here or anywhere.
//
// What a real one adds is a transport and one obligation: hand
// [notify.Delivery.ID] to the provider as its own idempotency key. The send and
// the bookkeeping are two systems and no transaction spans both, so a channel
// that ignores it sends a duplicate every time a process dies mid-send, and
// nothing will tell you. The ids are recorded here so the page can show what a
// real one would have passed along.
//
// It also honours the deadline it is handed, by doing nothing slow. A sender
// that ignores notifications.send_timeout hangs its dispatcher, and the
// dispatcher is one goroutine for every channel.
func (b *Box) NotificationSender() notify.Sender {
	return notify.SenderFunc(func(ctx context.Context, m notify.Message) error {
		if m.EmailAddress == "" && len(m.Devices) == 0 {
			// Nowhere to send is the channel's answer to give, not rig's: rig
			// knows who is owed what, and where somebody can be reached is the
			// channel's own question.
			return errors.New("outbox: nowhere to deliver this notification")
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		kinds := make([]string, 0, len(m.Deliveries))
		ids := make([]uuid.UUID, 0, len(m.Deliveries))
		for _, d := range m.Deliveries {
			kinds = append(kinds, d.Kind)
			ids = append(ids, d.ID)
		}

		tenantID := m.TenantID
		b.add(Message{
			Kind:        KindNotification,
			Channel:     string(notify.ChannelEmail),
			To:          m.EmailAddress,
			Subject:     strings.Join(kinds, ", "),
			DeliveryIDs: ids,
			TenantID:    &tenantID,
		})
		return nil
	})
}

// PushSender is a channel that reaches devices rather than an address, and the
// second half of what rig knows how to be told.
//
// It is the same interface as the email one and a different question answered.
// Email has an address on the account, so a channel for it needs nothing
// registered; a push has to be told where, which is what rig_notification_device
// is, and this is handed those rows — one call per account per channel, with
// every live device of theirs on it. Fan-out across a person's four machines is
// the channel's job, and so is what happens when one of them is gone: a real one
// deletes the device row when the provider says the subscription has expired,
// because a token nobody removes is a delivery that fails forever.
//
// What a real one would have here is a transport — Web Push with a VAPID key
// pair for Desktop, APNs or FCM for Mobile — and rig ships none of them, for the
// reason it ships no SMTP. So this records what it was handed: the tokens, and
// the deliveries whose ids a real one owes the provider as its idempotency key.
// Which is the honest version of a demonstration: the browser cannot be shown a
// notification it was never sent, and pretending otherwise would teach the wrong
// shape.
func (b *Box) PushSender(channel notify.Channel) notify.Sender {
	return notify.SenderFunc(func(ctx context.Context, m notify.Message) error {
		if len(m.Devices) == 0 {
			// Nothing registered for this channel. rig wrote the delivery row
			// because the account's setting says it wants this channel; where it
			// goes is the channel's question, and the answer here is nowhere.
			return errors.New("outbox: nothing registered for " + string(channel))
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		kinds := make([]string, 0, len(m.Deliveries))
		ids := make([]uuid.UUID, 0, len(m.Deliveries))
		for _, d := range m.Deliveries {
			kinds = append(kinds, d.Kind)
			ids = append(ids, d.ID)
		}

		devices := make([]string, 0, len(m.Devices))
		for _, d := range m.Devices {
			// The label, and enough of the token to recognise it by. Never the
			// whole token: it is a credential for sending to somebody's machine,
			// and this page is on a screen.
			label := d.Label
			if label == "" {
				label = "unlabelled"
			}
			devices = append(devices, label+" ("+short(d.Token)+")")
		}

		tenantID := m.TenantID
		b.add(Message{
			Kind:        KindNotification,
			Channel:     string(channel),
			Subject:     strings.Join(kinds, ", "),
			Devices:     devices,
			DeliveryIDs: ids,
			TenantID:    &tenantID,
		})
		return nil
	})
}

// short is the first few characters of a token, for recognising one in a list
// without putting the whole thing on a screen.
func short(token string) string {
	if len(token) <= 12 {
		return token
	}
	return token[:12] + "…"
}

func (b *Box) add(m Message) {
	b.mu.Lock()
	defer b.mu.Unlock()

	m.At = time.Now().UTC()
	b.messages = append([]Message{m}, b.messages...)
	if len(b.messages) > b.limit {
		b.messages = b.messages[:b.limit]
	}
}

// Messages returns what has been delivered, newest first.
func (b *Box) Messages() []Message {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]Message, len(b.messages))
	copy(out, b.messages)
	return out
}

// For returns the messages a caller in this tenant may see, newest first.
//
// A link minted before anybody knew which tenant it was for — a password reset
// is one, because an address can belong to accounts in several — carries no
// tenant and is shown to everybody. That is a demonstration's compromise and
// not a rule to copy: the honest version of this screen is no screen.
func (b *Box) For(tenantID uuid.UUID) []Message {
	var out []Message
	for _, m := range b.Messages() {
		if m.TenantID == nil || *m.TenantID == tenantID {
			out = append(out, m)
		}
	}
	return out
}
