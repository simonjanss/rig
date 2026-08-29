package session

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/runtime/rigerr"
)

// PrefixIdentity marks a token that has proved who somebody is and nothing more.
//
// A third prefix rather than a third [Kind]: Kind is the account_token_kind
// column, and an identity session is not a row in that table. Keeping them apart
// on the wire means a credential presented to the wrong door is refused by shape
// rather than by a check somebody has to remember to write.
const PrefixIdentity = "rig_it_"

// DefaultIdentityTTL is how long a session with no tenant lasts.
//
// Short, because of what it is for: somebody signs in, looks at the tenants
// they belong to and the invitations waiting for them, and picks one. Half an
// hour covers reading and deciding. It is not a session to work in — there is no
// tenant, so there is nothing to work on.
const DefaultIdentityTTL = 30 * time.Minute

// Identity is a person who has signed in but has not chosen a tenant.
//
// Deliberately not a [Token], and deliberately not convertible to
// [tenancy.Claims]. Claims carry a tenant because every generated query scopes by
// one; claims without a tenant would be claims that cannot scope a read, which is
// why the type refuses to exist. This is the other state a person can be in, and
// it is a different credential rather than a weaker version of the same one.
//
// What it opens is small and fixed: the tenants this person belongs to, the
// invitations addressed to them, accepting one, and creating a tenant where the
// application allows it. No application data, because there is no tenant to read
// it in.
type Identity struct {
	ID         uuid.UUID
	IdentityID uuid.UUID

	SecretHash []byte

	CreatedAt time.Time
	ExpiresAt time.Time
	// RevokedAt is set by signing out, and by picking a tenant: once there is
	// a real session there is no reason to leave a second credential alive.
	RevokedAt *time.Time

	IPAddress string
	UserAgent string
}

// Live reports whether the session may still be used.
func (i *Identity) Live(now time.Time) bool {
	return i != nil && i.RevokedAt == nil && now.Before(i.ExpiresAt)
}

// IdentityStore persists identity sessions.
type IdentityStore interface {
	InsertIdentitySession(ctx context.Context, s *Identity) error
	FindIdentitySession(ctx context.Context, id uuid.UUID) (*Identity, error)
	RevokeIdentitySession(ctx context.Context, id uuid.UUID, at time.Time) error
	// RevokeIdentitySessionsFor ends every one a person holds, which is what a
	// password change means.
	RevokeIdentitySessionsFor(ctx context.Context, identityID uuid.UUID, at time.Time) (int, error)
}

// IdentityConfig builds an [IdentityManager].
type IdentityConfig struct {
	Store IdentityStore
	// TTL defaults to [DefaultIdentityTTL].
	TTL time.Duration
	// Now is swappable for tests.
	Now func() time.Time
}

// IdentityManager issues and verifies identity sessions.
//
// Much smaller than [Manager], and the difference is the point. There is no
// rotation, no family, and no reuse detection: those exist because a refresh
// token is long-lived and travels, and this is neither. It is exchanged for a
// real session within minutes and protects nothing but a list of tenant names.
type IdentityManager struct {
	store IdentityStore
	ttl   time.Duration
	now   func() time.Time
}

// NewIdentity builds a manager.
func NewIdentity(cfg IdentityConfig) (*IdentityManager, error) {
	if cfg.Store == nil {
		return nil, errors.New("session: an IdentityStore is required")
	}
	if cfg.TTL == 0 {
		cfg.TTL = DefaultIdentityTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &IdentityManager{
		store: cfg.Store,
		ttl:   cfg.TTL,
		// UTC at the source, the same rule every other instant follows: an
		// expiry answered to a client never round-trips through a scan, so
		// without this it would carry the host's offset.
		now: func() time.Time { return cfg.Now().UTC() },
	}, nil
}

// IdentityIssueInput starts one.
type IdentityIssueInput struct {
	IdentityID uuid.UUID
	IPAddress  string
	UserAgent  string
}

// Issue starts an identity session and returns the token for it.
func (m *IdentityManager) Issue(ctx context.Context, in IdentityIssueInput) (Issued, error) {
	if in.IdentityID == uuid.Nil {
		return Issued{}, errors.New("session: Issue needs an identity")
	}

	id, s, err := mint()
	if err != nil {
		return Issued{}, err
	}

	now := m.now()
	row := &Identity{
		ID:         id,
		IdentityID: in.IdentityID,
		SecretHash: hashSecret(s),
		CreatedAt:  now,
		ExpiresAt:  now.Add(m.ttl),
		IPAddress:  in.IPAddress,
		UserAgent:  in.UserAgent,
	}
	if err := m.store.InsertIdentitySession(ctx, row); err != nil {
		return Issued{}, err
	}

	return Issued{
		Token:     PrefixIdentity + encoding.EncodeToString(id[:]) + "." + encoding.EncodeToString(s),
		TokenID:   id,
		ExpiresAt: row.ExpiresAt,
	}, nil
}

// Verify resolves a presented token to the person behind it.
//
// The error is always the same one, whether the prefix was wrong, the identifier
// unknown, the secret incorrect, or the session expired. Telling a caller which
// half of their guess was right is telling them how to guess better.
func (m *IdentityManager) Verify(ctx context.Context, presented string) (*Identity, error) {
	id, s, err := parseIdentity(presented)
	if err != nil {
		return nil, err
	}

	row, err := m.store.FindIdentitySession(ctx, id)
	if err != nil {
		return nil, err
	}
	// The hash before anything else is believed about the row: knowing an
	// identifier is not knowing the token.
	if row == nil || !matches(row.SecretHash, s) {
		return nil, ErrInvalidToken
	}
	if !row.Live(m.now()) {
		return nil, ErrInvalidToken
	}
	return row, nil
}

// Revoke ends one, by the token that holds it.
//
// Verifying first rather than deleting by identifier: an identifier is half a
// token and half a token should not be able to end somebody's session.
func (m *IdentityManager) Revoke(ctx context.Context, presented string) error {
	row, err := m.Verify(ctx, presented)
	if err != nil {
		return err
	}
	return m.store.RevokeIdentitySession(ctx, row.ID, m.now())
}

// RevokeAll ends every identity session a person holds.
func (m *IdentityManager) RevokeAll(ctx context.Context, identityID uuid.UUID) (int, error) {
	return m.store.RevokeIdentitySessionsFor(ctx, identityID, m.now())
}

// parseIdentity splits a presented identity token.
func parseIdentity(presented string) (uuid.UUID, secret, error) {
	body, found := strings.CutPrefix(presented, PrefixIdentity)
	if !found {
		return uuid.Nil, nil, ErrInvalidToken
	}

	rawID, rawSecret, found := strings.Cut(body, ".")
	if !found {
		return uuid.Nil, nil, ErrInvalidToken
	}

	idBytes, err := encoding.DecodeString(strings.ToUpper(rawID))
	if err != nil || len(idBytes) != 16 {
		return uuid.Nil, nil, ErrInvalidToken
	}
	s, err := encoding.DecodeString(strings.ToUpper(rawSecret))
	if err != nil || len(s) != secretBytes {
		return uuid.Nil, nil, ErrInvalidToken
	}

	return uuid.UUID(idBytes), s, nil
}

// ErrNoIdentitySession is what a handler answers when a request that needs one
// arrived without it.
var ErrNoIdentitySession = rigerr.Unauthorized("this request needs a sign-in")
