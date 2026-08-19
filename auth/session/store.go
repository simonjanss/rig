package session

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Kind is what a token is for.
type Kind string

const (
	// KindRefresh is exchanged for a new pair and never sent to an API.
	KindRefresh Kind = "Refresh"
	// KindAccess authenticates a request and is short-lived.
	KindAccess Kind = "Access"
)

// Client is what kind of thing holds the session.
type Client string

// The clients. The set is closed because the column is a Postgres enum: a value
// that is not one of these is rejected by the database rather than stored, which
// is why [Manager.Issue] defaults an unstated client instead of passing it
// through. What a session is allowed to do does not depend on this — it is what
// a person revoking a session from a list is reading to recognise it by.
const (
	ClientWeb     Client = "Web"
	ClientMobile  Client = "Mobile"
	ClientMachine Client = "Machine"
)

// Token is one row of the token table.
//
// The family root is the session. There is no separate session table because
// there is nothing a session would hold that its root token does not already:
// who, when, from where, and whether it is still alive.
type Token struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	AccountID uuid.UUID
	Kind      Kind

	// RootTokenID is the family. The first token of a session is its own root,
	// which is what lets "revoke this session" be one indexed UPDATE.
	RootTokenID uuid.UUID
	// ParentTokenID is the token this one was minted from, nil for a root.
	ParentTokenID *uuid.UUID

	SecretHash []byte

	CreatedAt time.Time
	ExpiresAt time.Time
	// RotatedAt is when this token was first exchanged. It is a soft consume:
	// the row stays, because a replay of a consumed token is the signal that
	// matters and a deleted row cannot signal anything.
	RotatedAt *time.Time
	// RevokedAt is a hard kill.
	RevokedAt *time.Time

	IPAddress string
	UserAgent string
	Client    Client

	ImpersonatedByAccountID *uuid.UUID
	APIKeyID                *uuid.UUID

	// Payload is whatever the application wants to remember about this session.
	//
	// Opaque here on purpose: this package stores and copies it and never looks
	// inside. It survives every rotation, so a fact recorded at sign-in is still
	// there twelve hours later — and is only as fresh as the last refresh, which
	// is why nothing deciding what somebody may do belongs in it. Grants are
	// resolved per request for exactly that reason.
	Payload json.RawMessage
}

// Live reports whether a token may still be used at the given time.
func (t *Token) Live(now time.Time) bool {
	return t.RevokedAt == nil && now.Before(t.ExpiresAt)
}

// Family is one session, summarized for a "where am I signed in" screen.
type Family struct {
	// Root is the token the session started from.
	Root *Token
	// LastUsedAt is the newest descendant's creation time, which is when the
	// session last refreshed. Deriving it beats maintaining a column that
	// every request would have to write.
	LastUsedAt time.Time
	// Tokens is how many tokens the family has produced.
	Tokens int
}

// Store is the persistence the manager needs.
//
// It is small on purpose. Everything interesting — rotation, reuse detection,
// expiry — is decided here rather than in SQL, so the rules can be read in one
// place and tested without a database.
type Store interface {
	// Insert writes a token.
	Insert(ctx context.Context, t *Token) error

	// Find reads a token by identifier, or returns nil when there is none.
	//
	// A missing token is not an error: it is the ordinary answer to a
	// presented value that was made up.
	Find(ctx context.Context, id uuid.UUID) (*Token, error)

	// Lock reads a token for update inside the caller's transaction.
	//
	// Rotation reads, decides, and writes, and two requests doing that at once
	// would both see an unconsumed token. The lock is what makes the second
	// one see the first one's work.
	Lock(ctx context.Context, id uuid.UUID) (*Token, error)

	// MarkRotated records the first exchange of a token.
	//
	// Implementations must leave an existing value alone. If a replay could
	// push the timestamp forward, an attacker replaying every twenty seconds
	// would keep a thirty-second leeway open indefinitely.
	MarkRotated(ctx context.Context, id uuid.UUID, at time.Time) error

	// RevokeFamily kills every live token descending from a root and reports
	// how many it killed.
	RevokeFamily(ctx context.Context, rootID uuid.UUID, at time.Time) (int, error)

	// Families lists an account's sessions, newest first.
	Families(ctx context.Context, tenantID, accountID uuid.UUID) ([]Family, error)

	// InTx runs fn inside one transaction, joining one already in progress.
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}
