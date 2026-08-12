// Package outbox is the mail this example would have sent.
//
// A [github.com/simonjanss/rig/auth/account.Notifier] delivers the single-use
// links the auth package mints: invitations, password resets, address
// confirmations. Sending mail is deliberately not rig's business — it does not
// know your templates, your sender, your locale, or whether you use a queue — so
// what rig provides is the moment a link exists and what it says.
//
// This one keeps the last few in memory so the interface can show them. That is
// exactly what a real notifier must never do, and the reason it is acceptable
// here is the reason it is interesting: a live invitation is a credential for as
// long as it lives, and putting one on a screen is putting a credential on a
// screen. It is here so that the invitation flow can be demonstrated end to end
// without a mail server, and the interface says so where it shows them.
package outbox

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/account"
)

// Kind is what a delivered link is for.
type Kind string

// The kinds, which mirror what the account service mints.
const (
	KindInvitation    Kind = "Invitation"
	KindPasswordReset Kind = "PasswordReset"
	KindVerification  Kind = "EmailVerification"
)

// Message is one link that would have been mailed.
type Message struct {
	Kind        Kind
	To          string
	DisplayName string
	Token       string
	At          time.Time
	TenantID    *uuid.UUID
	TenantRef   string
}

// Box collects messages.
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
		Token: token, TenantID: &tenantID, TenantRef: a.TenantID.String(),
	})
	return nil
}

// SendPasswordReset implements [account.Notifier].
func (b *Box) SendPasswordReset(_ context.Context, i *account.Identity, token string) error {
	b.add(Message{
		Kind: KindPasswordReset, To: i.EmailAddress,
		DisplayName: i.DisplayName, Token: token,
	})
	return nil
}

// SendEmailVerification implements [account.Notifier].
func (b *Box) SendEmailVerification(_ context.Context, i *account.Identity, token string) error {
	b.add(Message{
		Kind: KindVerification, To: i.EmailAddress,
		DisplayName: i.DisplayName, Token: token,
	})
	return nil
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

// Pending returns the invitations that have not been redeemed as far as this box
// knows, newest first.
//
// "As far as this box knows" is doing some work: consuming a link happens in the
// database and nothing tells the notifier about it, so an accepted invitation
// stays in the list until it is pushed out. The interface tries one and finds out.
func (b *Box) Pending() []Message {
	var out []Message
	for _, m := range b.Messages() {
		if m.Kind == KindInvitation {
			out = append(out, m)
		}
	}
	return out
}
