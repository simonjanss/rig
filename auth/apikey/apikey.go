// Package apikey mints and verifies machine-to-machine credentials.
//
// A key is two halves: a public identifier that is stored in clear and used to
// find the row, and a secret that is stored only as a sha256. The secret is
// shown once, at creation, and never again.
//
// sha256 rather than argon2id, and the difference matters. A password is short,
// human-chosen, and worth grinding for, so it gets a deliberately slow function.
// A key secret is 256 bits from the system's random source: no amount of
// guessing will find it, and running a memory-hard function on every machine
// request would be a denial of service aimed at ourselves.
package apikey

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/authlog"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// Prefix makes a leaked key identifiable on sight — by a scanner watching a
// public repository, and by whoever has to decide what it is.
const Prefix = "rig_sk_"

const (
	keyIDBytes  = 10
	secretBytes = 32
)

var encoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// ErrInvalidKey covers everything a caller could present that does not resolve
// to a usable key: malformed, unknown, wrong secret, revoked, expired, or
// coming from an address the key is not allowed to use.
//
// One error for all of them, because there is nothing a legitimate client does
// differently in any of those cases, and telling an attacker which half of the
// guess was right is a free hint.
var ErrInvalidKey = rigerr.Unauthorized("the API key is not valid")

// Key is one row.
type Key struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	AccountID uuid.UUID

	// Kind is whose key it is: an integration's, with a service account of its
	// own, or a person's.
	Kind Kind

	Name string

	// KeyID is the public half. It is what a person sees in a list, what a log
	// line names, and what a rate limit counts against — including for keys
	// that do not exist, which is the case that matters.
	KeyID      string
	SecretHash []byte

	// Scopes are permission keys, the same ones RBAC uses. Sharing the
	// vocabulary is what keeps a key from ever being able to express more than
	// a role could.
	Scopes []string
	// CIDRAllowList restricts where the key may be used from. Empty means
	// anywhere.
	CIDRAllowList []netip.Prefix

	CreatedAt          time.Time
	CreatedByAccountID *uuid.UUID
	ExpiresAt          *time.Time
	LastUsedAt         *time.Time
	RevokedAt          *time.Time
}

// Live reports whether the key may be used at the given time.
func (k *Key) Live(now time.Time) bool {
	if k.RevokedAt != nil {
		return false
	}
	return k.ExpiresAt == nil || now.Before(*k.ExpiresAt)
}

// Allows reports whether the key may be used from an address.
func (k *Key) Allows(addr netip.Addr) bool {
	if len(k.CIDRAllowList) == 0 {
		return true
	}
	if !addr.IsValid() {
		// A key restricted to a set of networks must not be usable by a caller
		// whose address could not be determined. Failing open here would make
		// the restriction advisory.
		return false
	}
	for _, p := range k.CIDRAllowList {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// Store is the persistence a manager needs.
type Store interface {
	Insert(ctx context.Context, k *Key) error
	// ByKeyID reads a key by its public half, or nil when there is none.
	ByKeyID(ctx context.Context, keyID string) (*Key, error)
	// Find reads a key by identifier within a tenant.
	Find(ctx context.Context, tenantID, id uuid.UUID) (*Key, error)
	// TouchLastUsed records that a key was used.
	TouchLastUsed(ctx context.Context, id uuid.UUID, at time.Time) error
	// Revoke kills a key.
	Revoke(ctx context.Context, tenantID, id uuid.UUID, at time.Time) error
	// SetExpiry ends a key at a chosen time, for a rotation's overlap window.
	SetExpiry(ctx context.Context, tenantID, id uuid.UUID, at time.Time) error
	// List returns a tenant's keys, newest first.
	List(ctx context.Context, tenantID uuid.UUID) ([]*Key, error)
}

// Config builds a manager.
type Config struct {
	Store Store
	Log   authlog.Log

	// TouchInterval bounds how often a key's last-used time is written.
	//
	// Recording it on every request would turn a read into a write on the
	// hottest path a machine integration has, for a field nobody reads more
	// precisely than "today". Five minutes is close enough to answer "is this
	// key still in use" and cheap enough to be free.
	TouchInterval time.Duration

	// DefaultTTL bounds a key with no explicit expiry. Zero means keys can be
	// minted that never expire, which is sometimes what a deployment wants and
	// should always be a decision rather than a default.
	DefaultTTL time.Duration

	Now func() time.Time
}

// DefaultTouchInterval is how often a key's last use is recorded.
const DefaultTouchInterval = 5 * time.Minute

// Manager mints and verifies keys.
type Manager struct {
	store Store
	log   authlog.Log
	now   func() time.Time
	touch time.Duration
	ttl   time.Duration
}

// New builds a manager.
func New(cfg Config) (*Manager, error) {
	if cfg.Store == nil {
		return nil, errors.New("apikey: a Store is required")
	}
	if cfg.Log == nil {
		cfg.Log = authlog.Noop{}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.TouchInterval == 0 {
		cfg.TouchInterval = DefaultTouchInterval
	}
	return &Manager{
		store: cfg.Store,
		log:   cfg.Log,
		now:   utcClock(cfg.Now),
		touch: cfg.TouchInterval,
		ttl:   cfg.DefaultTTL,
	}, nil
}

// MintInput describes a new key.
type MintInput struct {
	TenantID uuid.UUID
	// AccountID is the account the key acts as. Every write the key makes is
	// stamped with it, so a machine's changes are attributable to something
	// rather than to nobody.
	//
	// For an integration this is a service account of its own, which is what
	// makes "the integration did it" a thing the audit trail can say. For a
	// personal key it is the person, and must equal CreatedByAccountID.
	AccountID uuid.UUID

	// Kind is whose key this is. The zero value is an integration key, because
	// that is the one with the sharper failure mode if it is wrong: a personal
	// key mistaken for an integration acts as a person who can be deactivated,
	// which fails safely, while the reverse would silently widen what the key
	// can reach.
	Kind Kind

	Name string

	Scopes        []string
	CIDRAllowList []netip.Prefix
	ExpiresAt     *time.Time

	// CreatedByAccountID is the person who minted it.
	CreatedByAccountID *uuid.UUID
}

// Kind is whose key it is.
type Kind string

const (
	// KindIntegration acts as a service account. Revoking the person who set it
	// up does not stop it, which is the point: an integration outlives the
	// employee who connected it.
	KindIntegration Kind = "Integration"

	// KindPersonal acts as its owner, for somebody automating their own work.
	// What it can reach is exactly what they can reach, so it needs no separate
	// permission story — and when they leave, it leaves with them.
	KindPersonal Kind = "Personal"
)

// Minted is a new key and its secret, at the only moment the secret exists.
type Minted struct {
	Key *Key
	// Secret is the whole value to hand over. It is not recoverable: showing
	// it again would mean storing something worth stealing.
	Secret string
}

// utcClock is the clock every constructed instant comes from.
//
// A timestamptz stores an instant and no zone, and a scanned one is normalized
// on the way out of the database — but a row this package builds in memory and
// answers with never went through a scan. A minted key's createdAt and a fresh
// session's expiresAt are both like that, and both reached a client carrying the
// host's offset. Wrapping the clock fixes it at the source: what is written and
// what is answered are the same UTC instant.
func utcClock(now func() time.Time) func() time.Time {
	if now == nil {
		now = time.Now
	}
	return func() time.Time { return now().UTC() }
}

// Mint creates a key.
func (m *Manager) Mint(ctx context.Context, in MintInput) (Minted, error) {
	if in.TenantID == uuid.Nil || in.AccountID == uuid.Nil {
		return Minted{}, errors.New("apikey: Mint needs both a tenant and an account")
	}
	if strings.TrimSpace(in.Name) == "" {
		// A list of keys called "" is a list nobody can safely revoke from.
		return Minted{}, rigerr.Invalid("an API key needs a name")
	}

	kind := in.Kind
	if kind == "" {
		kind = KindIntegration
	}
	switch kind {
	case KindIntegration, KindPersonal:
	default:
		return Minted{}, rigerr.Invalid("%q is not a kind of API key", string(kind))
	}

	// A personal key acts as the person who made it. The database says so too —
	// the foundation carries a CHECK for it — and saying it here as well is what
	// turns a constraint violation nobody can read into an answer that names the
	// problem.
	if kind == KindPersonal {
		if in.CreatedByAccountID == nil || *in.CreatedByAccountID != in.AccountID {
			return Minted{}, rigerr.Invalid(
				"a personal API key acts as the account that created it")
		}
	}

	id, err := uuid.NewV7()
	if err != nil {
		return Minted{}, fmt.Errorf("apikey: generate id: %w", err)
	}

	keyID, err := randomString(keyIDBytes)
	if err != nil {
		return Minted{}, err
	}
	secretRaw := make([]byte, secretBytes)
	if _, err := rand.Read(secretRaw); err != nil {
		return Minted{}, fmt.Errorf("apikey: generate secret: %w", err)
	}

	now := m.now()
	expires := in.ExpiresAt
	if expires == nil && m.ttl > 0 {
		at := now.Add(m.ttl)
		expires = &at
	}

	sum := sha256.Sum256(secretRaw)
	k := &Key{
		ID:                 id,
		TenantID:           in.TenantID,
		AccountID:          in.AccountID,
		Kind:               kind,
		Name:               strings.TrimSpace(in.Name),
		KeyID:              keyID,
		SecretHash:         sum[:],
		Scopes:             slices.Clone(in.Scopes),
		CIDRAllowList:      slices.Clone(in.CIDRAllowList),
		CreatedAt:          now,
		CreatedByAccountID: in.CreatedByAccountID,
		ExpiresAt:          expires,
	}
	if err := m.store.Insert(ctx, k); err != nil {
		return Minted{}, err
	}

	return Minted{Key: k, Secret: Prefix + keyID + "_" + encoding.EncodeToString(secretRaw)}, nil
}

// Verify resolves a presented key into claims.
//
// The address is checked against the key's allow list, so pass the one the
// request actually came from — not one read out of a header a client controls,
// unless a proxy you trust set it.
func (m *Manager) Verify(ctx context.Context, presented string, from netip.Addr) (tenancy.Claims, *Key, error) {
	keyID, secret, err := split(presented)
	if err != nil {
		// Nothing to count this against: there is no key identifier to name.
		m.recordFailure(ctx, "", from, "malformed")
		return tenancy.Claims{}, nil, ErrInvalidKey
	}

	k, err := m.store.ByKeyID(ctx, keyID)
	if err != nil {
		return tenancy.Claims{}, nil, err
	}

	now := m.now()
	switch {
	case k == nil:
		m.recordFailure(ctx, keyID, from, "unknown")
		return tenancy.Claims{}, nil, ErrInvalidKey
	case subtle.ConstantTimeCompare(k.SecretHash, hash(secret)) != 1:
		m.recordFailure(ctx, keyID, from, "wrong secret")
		return tenancy.Claims{}, nil, ErrInvalidKey
	case !k.Live(now):
		m.recordFailure(ctx, keyID, from, "revoked or expired")
		return tenancy.Claims{}, nil, ErrInvalidKey
	case !k.Allows(from):
		m.recordFailure(ctx, keyID, from, "address not allowed")
		return tenancy.Claims{}, nil, ErrInvalidKey
	}

	if m.shouldTouch(k, now) {
		if err := m.store.TouchLastUsed(ctx, k.ID, now); err != nil {
			return tenancy.Claims{}, nil, err
		}
	}

	m.log.Write(ctx, authlog.Entry{
		At:        now,
		Event:     authlog.EventAPIKeyAuthSucceeded,
		Outcome:   authlog.Succeeded,
		TenantID:  &k.TenantID,
		AccountID: &k.AccountID,
		APIKeyID:  &k.ID,
		APIKeyRef: k.KeyID,
		IPAddress: addrString(from),
	})

	// The key travels in the claims, which is what lets a write record the
	// credential it came through and not only the account it acted as. One
	// service account can have several keys, and "which key did this" is the
	// question worth being able to answer afterwards.
	actingKey := k.ID

	return tenancy.Claims{
		TenantID:  k.TenantID,
		AccountID: k.AccountID,
		APIKeyID:  &actingKey,
		Subject:   tenancy.SubjectAPIKey,
		// A key's scopes are its permissions, full stop. There is no role
		// lookup here: a machine credential that inherited a person's roles
		// would grow new powers every time somebody edited a role. A personal
		// key is enriched by the caller — see authhttp — because that one is a
		// person automating themselves.
		Permissions: slices.Clone(k.Scopes),
	}, k, nil
}

// Rotate mints a replacement and gives the old key an overlap to die in.
//
// Revoking immediately would break whatever is deployed with the old value the
// moment the new one is created, which is why nobody rotates keys. An overlap
// makes it a two-step the operator can actually complete.
func (m *Manager) Rotate(ctx context.Context, tenantID, id uuid.UUID, overlap time.Duration) (Minted, error) {
	old, err := m.store.Find(ctx, tenantID, id)
	if err != nil {
		return Minted{}, err
	}
	if old == nil {
		return Minted{}, rigerr.NotFound("no API key with that identifier")
	}

	now := m.now()
	minted, err := m.Mint(ctx, MintInput{
		TenantID:           old.TenantID,
		AccountID:          old.AccountID,
		Name:               old.Name,
		Scopes:             old.Scopes,
		CIDRAllowList:      old.CIDRAllowList,
		CreatedByAccountID: old.CreatedByAccountID,
	})
	if err != nil {
		return Minted{}, err
	}

	// An overlap of zero means the old key stops working now, which is a
	// legitimate choice when the old value is known to have leaked.
	end := now.Add(overlap)
	if overlap <= 0 {
		if err := m.store.Revoke(ctx, tenantID, old.ID, now); err != nil {
			return Minted{}, err
		}
		return minted, nil
	}
	// Never extend a key that was already going to expire sooner.
	if old.ExpiresAt != nil && old.ExpiresAt.Before(end) {
		return minted, nil
	}
	if err := m.store.SetExpiry(ctx, tenantID, old.ID, end); err != nil {
		return Minted{}, err
	}
	return minted, nil
}

// Revoke kills a key immediately.
func (m *Manager) Revoke(ctx context.Context, tenantID, id uuid.UUID) error {
	return m.store.Revoke(ctx, tenantID, id, m.now())
}

// List returns a tenant's keys. The secrets are not in them; there is nothing
// stored that could produce one.
func (m *Manager) List(ctx context.Context, tenantID uuid.UUID) ([]*Key, error) {
	return m.store.List(ctx, tenantID)
}

func (m *Manager) shouldTouch(k *Key, now time.Time) bool {
	return k.LastUsedAt == nil || now.Sub(*k.LastUsedAt) >= m.touch
}

// recordFailure writes the entry a rate limit will count.
//
// The reason goes in the detail rather than the response: an operator debugging
// their integration needs it, and an attacker probing keys does not get it.
func (m *Manager) recordFailure(ctx context.Context, keyID string, from netip.Addr, reason string) {
	m.log.Write(ctx, authlog.Entry{
		At:        m.now(),
		Event:     authlog.EventAPIKeyAuthFailed,
		Outcome:   authlog.Failed,
		APIKeyRef: keyID,
		IPAddress: addrString(from),
		Detail:    map[string]any{"reason": reason},
	})
}

func hash(secret []byte) []byte {
	sum := sha256.Sum256(secret)
	return sum[:]
}

// split parses a presented key.
func split(presented string) (keyID string, secret []byte, err error) {
	body, ok := strings.CutPrefix(presented, Prefix)
	if !ok {
		return "", nil, ErrInvalidKey
	}
	keyID, rawSecret, found := strings.Cut(body, "_")
	if !found || keyID == "" {
		return "", nil, ErrInvalidKey
	}

	secret, err = encoding.DecodeString(strings.ToUpper(rawSecret))
	if err != nil || len(secret) != secretBytes {
		return "", nil, ErrInvalidKey
	}
	return keyID, secret, nil
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("apikey: generate identifier: %w", err)
	}
	return encoding.EncodeToString(b), nil
}

func addrString(a netip.Addr) string {
	if !a.IsValid() {
		return ""
	}
	return a.String()
}
