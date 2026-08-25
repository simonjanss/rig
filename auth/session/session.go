// Package session issues, verifies, and rotates opaque session tokens.
//
// A session is a family of tokens. The first refresh token is the family root
// and is the session's identity; every later token points back at it. That is
// what makes "sign me out of this device" one indexed UPDATE, and what gives
// reuse detection something to revoke.
//
// Rotation is mandatory. Every refresh consumes the token it was given and
// mints a new pair, so a stolen refresh token is useful exactly until the real
// client refreshes — at which point the theft becomes visible rather than
// permanent.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/authlog"
	"github.com/simonjanss/rig/runtime/dbx"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/throttle"
)

// ErrInvalidToken is returned for anything a caller presented that does not
// resolve to a live token: malformed, unknown, expired, revoked, or replayed.
//
// One error for all of them is deliberate. Telling a caller that a token exists
// but has expired, as opposed to never existing, is a small oracle, and there
// is nothing a legitimate client does differently in the two cases.
var ErrInvalidToken = rigerr.Unauthorized("the session is not valid")

// Default lifetimes.
const (
	DefaultAccessTTL      = 10 * time.Minute
	DefaultRefreshTTL     = 12 * time.Hour
	DefaultRememberTTL    = 30 * 24 * time.Hour
	DefaultRotationLeeway = 30 * time.Second
)

// Config builds a manager.
type Config struct {
	Store Store
	Log   authlog.Log

	// AccessTTL is short because an access token is the one that travels. Ten
	// minutes bounds the damage of a leaked one without making refresh a
	// constant background hum.
	AccessTTL time.Duration
	// RefreshTTL is how long an ordinary session lasts.
	RefreshTTL time.Duration
	// RememberTTL is how long a session lasts when the person asked to stay
	// signed in.
	RememberTTL time.Duration

	// RotationLeeway is how long a refresh token stays usable after it has
	// been exchanged.
	//
	// Without one, a dropped response is a logout: the client never received
	// the new pair, retries with the old one, and gets its whole family
	// revoked for a network blip. Thirty seconds covers the retry and leaves a
	// real replay — which turns up minutes or hours later — firmly outside.
	RotationLeeway time.Duration

	// Limiter and RefreshLimit bound how hard one session may refresh.
	//
	// Both or neither. Nil is a manager nobody wired a limiter into, which has
	// to go on working: this package is constructible without the rest of auth,
	// and a refresh limit is a backstop rather than a security boundary — what
	// it catches is a client looping, not somebody with a stolen token, whose
	// second use is caught by reuse detection instead.
	//
	// The limit counts TokenRefreshed against the token family, so it is one
	// session's budget and not one person's: signing in on a phone and a laptop
	// does not make either of them closer to it.
	Limiter      *throttle.Limiter
	RefreshLimit throttle.Limit

	// Cache keeps verified access tokens in memory, and is told to stop.
	//
	// Nil, the default, means every request reads the row. A cache built by
	// [NewTokenCache] means most of them do not, and that a revocation reaches
	// every replica on the transaction that performed it — so the lifetime in
	// [TokenCacheConfig] is the backstop rather than the guarantee.
	//
	// There is no plain time-to-live here any more, and its absence is the
	// decision: a cache over authentication with nothing to withdraw an entry
	// is a revoked session that keeps working, which is not a trade this
	// package offers.
	Cache *TokenCache

	// OnRotate replaces a session's payload when it is refreshed.
	//
	// Nil — the default — carries the previous payload forward unchanged, which is
	// what an application that never sets one wants and what an application that
	// set one at sign-in usually wants too.
	//
	// It is handed the token being exchanged, so the current payload is
	// prev.Payload and everything else about the session is there to decide from.
	// Returning nil clears the payload; returning an error fails the refresh,
	// which means a session cannot be refreshed while this is broken — deliberate,
	// since the alternative is quietly serving a payload the application refused
	// to produce.
	//
	// It runs inside the rotation's transaction, once per refresh, and both new
	// tokens get the result. Keep it quick: it is on the path every client takes
	// when its access token expires.
	OnRotate func(ctx context.Context, prev *Token) (json.RawMessage, error)

	// Now is swappable for tests.
	Now func() time.Time
}

// Manager issues and verifies sessions.
type Manager struct {
	store    Store
	log      authlog.Log
	now      func() time.Time
	ttl      ttls
	leeway   time.Duration
	cache    *TokenCache
	onRotate func(ctx context.Context, prev *Token) (json.RawMessage, error)

	limiter      *throttle.Limiter
	refreshLimit throttle.Limit
}

type ttls struct{ access, refresh, remember time.Duration }

// New builds a manager. Zero-valued durations take their defaults.
func New(cfg Config) (*Manager, error) {
	if cfg.Store == nil {
		return nil, errors.New("session: a Store is required")
	}
	if cfg.Log == nil {
		cfg.Log = authlog.Noop{}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.AccessTTL == 0 {
		cfg.AccessTTL = DefaultAccessTTL
	}
	if cfg.RefreshTTL == 0 {
		cfg.RefreshTTL = DefaultRefreshTTL
	}
	if cfg.RememberTTL == 0 {
		cfg.RememberTTL = DefaultRememberTTL
	}
	if cfg.RotationLeeway == 0 {
		cfg.RotationLeeway = DefaultRotationLeeway
	}

	m := &Manager{
		store: cfg.Store,
		log:   cfg.Log,
		// UTC at the source. A pair this manager builds is answered to a client
		// without a round trip, so a token's expiresAt never passes the scan that
		// normalizes one read back — and would otherwise carry the host's offset.
		now:      func() time.Time { return cfg.Now().UTC() },
		ttl:      ttls{cfg.AccessTTL, cfg.RefreshTTL, cfg.RememberTTL},
		leeway:   cfg.RotationLeeway,
		onRotate: cfg.OnRotate,

		limiter:      cfg.Limiter,
		refreshLimit: cfg.RefreshLimit,
		cache:        cfg.Cache,
	}
	return m, nil
}

// Issued is one token, at the only moment its secret exists.
type Issued struct {
	// Token is the value to hand the client. It cannot be recovered later:
	// only its hash is stored.
	Token     string
	TokenID   uuid.UUID
	ExpiresAt time.Time
}

// Pair is what every issue and every rotation returns.
type Pair struct {
	Access  Issued
	Refresh Issued
	// RootTokenID identifies the session the pair belongs to. It is what a
	// client shows on a "signed in devices" screen and what Revoke takes.
	RootTokenID uuid.UUID
}

// IssueInput starts a session.
type IssueInput struct {
	TenantID  uuid.UUID
	AccountID uuid.UUID
	Client    Client

	IPAddress string
	UserAgent string

	// Remember asks for the long lifetime.
	Remember bool

	// ImpersonatedByAccountID marks a session an administrator opened as
	// somebody else. It propagates through every rotation, so a session that
	// began as impersonation cannot quietly become an ordinary one.
	ImpersonatedByAccountID *uuid.UUID
	// APIKeyID marks a session minted for a machine.
	APIKeyID *uuid.UUID

	// Payload is the application's own context for this session — which device,
	// which sign-in method, when step-up authentication was last satisfied. It is
	// carried forward by every rotation and reaches a handler as
	// [tenancy.Claims.Extra].
	//
	// Never authorization. It is written here and again only when the session is
	// refreshed, so a permission kept in it stays true for as long as the refresh
	// lifetime — twelve hours, or thirty days for "remember me" — after somebody
	// took it away.
	Payload json.RawMessage
}

// Issue starts a new session and returns its first pair.
func (m *Manager) Issue(ctx context.Context, in IssueInput) (Pair, error) {
	if in.TenantID == uuid.Nil || in.AccountID == uuid.Nil {
		return Pair{}, errors.New("session: Issue needs both a tenant and an account")
	}
	if in.Client == "" {
		// A browser, because that is what an unstated client almost always is.
		// Defaulted here rather than left empty because the column is an enum:
		// passing it through would turn a caller who did not care into an
		// invalid-input error from Postgres, which is nobody's idea of a clue.
		in.Client = ClientWeb
	}

	now := m.now()
	refreshTTL := m.ttl.refresh
	if in.Remember {
		refreshTTL = m.ttl.remember
	}

	var pair Pair
	err := m.store.InTx(ctx, func(ctx context.Context) error {
		refresh, refreshToken, err := m.build(tokenSpec{
			kind:      KindRefresh,
			ttl:       refreshTTL,
			now:       now,
			tenantID:  in.TenantID,
			accountID: in.AccountID,
			client:    in.Client,
			ip:        in.IPAddress,
			userAgent: in.UserAgent,
			asBy:      in.ImpersonatedByAccountID,
			apiKeyID:  in.APIKeyID,
			payload:   in.Payload,
		})
		if err != nil {
			return err
		}
		// The first refresh token is its own root: a session with no family
		// yet still needs an identity, and inventing a separate one would mean
		// a second table that says nothing new.
		refresh.RootTokenID = refresh.ID
		if err := m.store.Insert(ctx, refresh); err != nil {
			return err
		}

		access, accessToken, err := m.child(refresh, KindAccess, now)
		if err != nil {
			return err
		}
		if err := m.store.Insert(ctx, access); err != nil {
			return err
		}

		pair = Pair{
			Access:      Issued{accessToken, access.ID, access.ExpiresAt},
			Refresh:     Issued{refreshToken, refresh.ID, refresh.ExpiresAt},
			RootTokenID: refresh.RootTokenID,
		}
		return nil
	})
	if err != nil {
		return Pair{}, err
	}
	// Nothing is logged here. Issue is reached from a password login, from an
	// OAuth callback, and from an administrator starting an impersonation, and
	// those are three different events — only the caller knows which.
	return pair, nil
}

// Verify resolves a presented access token.
//
// It refuses a refresh token outright. The two are interchangeable strings and
// a client that sends the wrong one should be told so by failing, not by being
// let in with a credential that was never meant to leave its keychain.
func (m *Manager) Verify(ctx context.Context, presented string) (*Token, error) {
	kind, id, s, err := parse(presented)
	if err != nil {
		return nil, err
	}
	if kind != KindAccess {
		return nil, ErrInvalidToken
	}

	// Every check below runs on the answer whether it came from the cache or
	// from the row, which is the property that makes the cache safe to reason
	// about: caching changes where the token was read, and nothing else. The
	// secret is compared here rather than being part of the key — the key is the
	// identifier a caller supplied, and a secret is never one.
	t, err := m.cache.load(id, func() (*Token, error) {
		return m.store.Find(ctx, id)
	})
	switch {
	case errors.Is(err, errNoToken):
		return nil, ErrInvalidToken
	case err != nil:
		return nil, err
	}

	now := m.now()
	if t.Kind != KindAccess || !matches(t.SecretHash, s) || !t.Live(now) {
		return nil, ErrInvalidToken
	}
	return t, nil
}

// Rotate exchanges a refresh token for a new pair.
//
// The whole exchange runs in one transaction under a row lock, because the
// decision it makes — is this the first use, a retry, or a replay — is only
// correct if nobody else can be making it at the same time.
func (m *Manager) Rotate(ctx context.Context, presented string) (Pair, error) {
	kind, id, s, err := parse(presented)
	if err != nil {
		return Pair{}, err
	}
	if kind != KindRefresh {
		return Pair{}, ErrInvalidToken
	}

	now := m.now()

	var (
		pair    Pair
		reuse   *Token
		rotated *Token
	)
	err = m.store.InTx(ctx, func(ctx context.Context) error {
		prev, err := m.store.Lock(ctx, id)
		if err != nil {
			return err
		}
		// The hash is checked before anything else is believed about the row.
		// Knowing an identifier is not knowing the token.
		if prev == nil || prev.Kind != KindRefresh || !matches(prev.SecretHash, s) {
			return ErrInvalidToken
		}
		if !prev.Live(now) {
			return ErrInvalidToken
		}

		if prev.RotatedAt != nil {
			if now.Sub(*prev.RotatedAt) > m.leeway {
				// Somebody is using a token that was consumed a while ago.
				// Either it was stolen, or the legitimate holder's copy was —
				// and there is no way to tell which from here, so the only safe
				// move is to end the session for both.
				//
				// The revocation happens after this transaction rather than
				// inside it. Returning an error from here rolls the transaction
				// back, and a revocation that is rolled back is a session the
				// thief keeps.
				reuse = prev
				return ErrInvalidToken
			}
			// Inside the leeway. A dropped response is not an attack, and
			// revoking the family for one would make every flaky connection a
			// logout.
		}

		// Once the token is known good and before anything is written. Later
		// and a refused refresh has already consumed the parent, which would
		// turn a limit into a logout; earlier and it would count against a
		// family named by whoever asked, so anybody could exhaust a stranger's
		// budget by presenting their session id with the wrong secret.
		if err := m.checkRefreshLimit(ctx, prev); err != nil {
			return err
		}

		// Before the parent is consumed, and once, so that both new tokens agree
		// about what this session now knows.
		//
		// The order matters. A hook that fails must leave the refresh token
		// usable — the client will try again, and an outage in whatever the hook
		// talks to should not sign anybody out. Consuming first and rolling back
		// would get there too on a real database, but this does not depend on the
		// store having transactions to be correct.
		payload := prev.Payload
		if m.onRotate != nil {
			payload, err = m.onRotate(ctx, prev)
			if err != nil {
				return fmt.Errorf("session: OnRotate: %w", err)
			}
		}

		if err := m.store.MarkRotated(ctx, prev.ID, now); err != nil {
			return err
		}

		refresh, refreshToken, err := m.child(prev, KindRefresh, now)
		if err != nil {
			return err
		}
		refresh.Payload = payload
		// The session does not get longer by being refreshed. Inheriting the
		// parent's expiry rather than starting a new one is what stops a
		// twelve-hour session becoming immortal for anyone who leaves a tab
		// open.
		refresh.ExpiresAt = prev.ExpiresAt
		if err := m.store.Insert(ctx, refresh); err != nil {
			return err
		}

		access, accessToken, err := m.child(prev, KindAccess, now)
		if err != nil {
			return err
		}
		access.Payload = payload
		if err := m.store.Insert(ctx, access); err != nil {
			return err
		}

		pair, rotated = Pair{
			Access:      Issued{accessToken, access.ID, access.ExpiresAt},
			Refresh:     Issued{refreshToken, refresh.ID, refresh.ExpiresAt},
			RootTokenID: prev.RootTokenID,
		}, prev
		return nil
	})

	if reuse != nil {
		// A transaction of its own, because the one above was rolled back on
		// purpose — see the comment there. The revocation and the invalidation
		// share it, so no replica is ever told to forget a session that is in
		// fact still alive.
		revokeErr := m.store.InTx(ctx, func(ctx context.Context) error {
			killed, err := m.store.RevokeFamily(ctx, reuse.RootTokenID, now)
			if err != nil {
				return err
			}
			return m.forget(ctx, killed)
		})
		if revokeErr != nil {
			return Pair{}, revokeErr
		}
		m.recordReuse(ctx, now, reuse)
		return Pair{}, ErrInvalidToken
	}
	if err != nil {
		return Pair{}, err
	}

	m.record(ctx, authlog.Entry{
		At:          now,
		Event:       authlog.EventTokenRefreshed,
		Outcome:     authlog.Succeeded,
		TenantID:    &rotated.TenantID,
		AccountID:   &rotated.AccountID,
		IPAddress:   rotated.IPAddress,
		UserAgent:   rotated.UserAgent,
		TokenRootID: &pair.RootTokenID,
	})
	return pair, nil
}

// checkRefreshLimit bounds how hard one session may rotate.
//
// A client refreshing sixty times a minute is looping, not working. The limit is
// counted out of the auth log the same way the login limits are, so it holds
// across replicas and survives a restart.
func (m *Manager) checkRefreshLimit(ctx context.Context, prev *Token) error {
	if m.limiter == nil || m.refreshLimit.Max <= 0 {
		return nil
	}

	d, err := m.limiter.Allow(ctx, throttle.Check{
		Limit: m.refreshLimit,
		Key:   throttle.TokenFamily(prev.RootTokenID.String()),
	})
	if err != nil {
		return err
	}
	return d.Err()
}

// Revoke ends one session.
func (m *Manager) Revoke(ctx context.Context, rootID uuid.UUID) error {
	return m.RevokeBy(ctx, rootID, uuid.Nil)
}

// RevokeBy ends one session and records who ended it.
//
// The administrative half of [Manager.Revoke], for a session somebody else is
// holding: a lost phone, or a departure. `by` is the account that asked, and
// uuid.Nil means the holder ended their own — which is what Revoke passes and
// what a logout is.
//
// It reads the root token before revoking, and not only for the identifier. The
// entry needs the tenant and the account, and an entry without a tenant is
// invisible to every reader of the trail: `rig_auth_log.tenant_id` is nullable
// for the attempts that resolved to nobody, and the readers filter on it. A
// logout stamped with no tenant is a logout the tenant's own audit screen can
// never show, which is how this went unnoticed for as long as nothing read the
// table.
func (m *Manager) RevokeBy(ctx context.Context, rootID, by uuid.UUID) error {
	now := m.now()

	// One transaction over the read, the revocation and the invalidation. The
	// last of those is why: a notification is delivered when the transaction
	// issuing it commits and discarded when it rolls back, so publishing here
	// is what makes "this session is over" and "stop believing this session"
	// one event rather than two that can disagree.
	var root *Token
	if err := m.store.InTx(ctx, func(ctx context.Context) error {
		// A read on a path that is not hot, in exchange for an entry that can
		// be found. A session nobody has is still revoked — the statement
		// affects nothing — and the entry is written with what little is known.
		found, err := m.store.Find(ctx, rootID)
		if err != nil {
			return err
		}
		root = found

		killed, err := m.store.RevokeFamily(ctx, rootID, now)
		if err != nil {
			return err
		}
		return m.forget(ctx, killed)
	}); err != nil {
		return err
	}

	entry := authlog.Entry{
		At:          now,
		Event:       authlog.EventLogout,
		Outcome:     authlog.Succeeded,
		TokenRootID: &rootID,
	}
	if root != nil {
		entry.TenantID = &root.TenantID
		entry.AccountID = &root.AccountID
		entry.IPAddress = root.IPAddress
		entry.UserAgent = root.UserAgent
	}
	if by != uuid.Nil && (root == nil || by != root.AccountID) {
		// Only when somebody else did it. "revokedBy: me" on every logout would
		// be a field a reader has to compare before it means anything.
		entry.Detail = map[string]any{"revokedBy": by.String()}
	}
	m.record(ctx, entry)
	return nil
}

// FindSession returns the root token of a live session in a tenant, or nil when
// there is none.
//
// The tenant is an argument rather than something the caller checks afterwards,
// because "does this session exist" and "does this session exist *here*" are
// questions with different answers and only the second one is ever safe to
// answer. A session that has been revoked or has expired is nil too: it is not
// there to be acted on, and saying otherwise would let an old identifier be
// told apart from an invented one.
func (m *Manager) FindSession(ctx context.Context, tenantID, rootID uuid.UUID) (*Token, error) {
	found, err := m.store.Find(ctx, rootID)
	if err != nil {
		return nil, err
	}
	if found == nil || found.TenantID != tenantID || found.ID != found.RootTokenID {
		return nil, nil
	}
	if !found.Live(m.now()) {
		return nil, nil
	}
	return found, nil
}

// RevokeAll ends every session an account has.
//
// A password change and a password reset both call it. Changing a password
// while the person who stole it stays signed in elsewhere is not a recovery.
func (m *Manager) RevokeAll(ctx context.Context, tenantID, accountID uuid.UUID) error {
	families, err := m.store.Families(ctx, tenantID, accountID)
	if err != nil {
		return err
	}

	now := m.now()
	return m.store.InTx(ctx, func(ctx context.Context) error {
		for _, f := range families {
			killed, err := m.store.RevokeFamily(ctx, f.Root.ID, now)
			if err != nil {
				return err
			}
			if err := m.forget(ctx, killed); err != nil {
				return err
			}
		}
		return nil
	})
}

// forget tells every replica to stop believing in the tokens just revoked.
//
// Always called inside the transaction that revoked them, which is what makes
// the invalidation atomic with the revocation: Postgres delivers a notification
// when the issuing transaction commits and throws it away when that transaction
// rolls back.
//
// A store with no transaction on the context is one that is not Postgres — a
// [MemoryStore] in a test — so there is no channel to publish on and no other
// replica to reach. Forgetting in this process is the whole of what is
// available and the whole of what is needed.
func (m *Manager) forget(ctx context.Context, ids []uuid.UUID) error {
	if m.cache == nil || len(ids) == 0 {
		return nil
	}
	if tx, ok := dbx.Tx(ctx); ok {
		return m.cache.forget(ctx, tx, ids...)
	}
	m.cache.drop(ids...)
	return nil
}

// List returns an account's live sessions, newest first.
func (m *Manager) List(ctx context.Context, tenantID, accountID uuid.UUID) ([]Family, error) {
	return m.store.Families(ctx, tenantID, accountID)
}

// ListTenant returns every live session in a tenant, newest first.
//
// This is the wide answer, and holding it is a permission somebody was granted:
// it says who is signed in across the whole tenant, which is what makes ending
// the session of somebody who left possible at all.
func (m *Manager) ListTenant(ctx context.Context, tenantID uuid.UUID) ([]Family, error) {
	return m.store.TenantFamilies(ctx, tenantID)
}

// tokenSpec is what every token needs at birth.
type tokenSpec struct {
	kind      Kind
	ttl       time.Duration
	now       time.Time
	tenantID  uuid.UUID
	accountID uuid.UUID
	client    Client
	ip        string
	userAgent string
	asBy      *uuid.UUID
	apiKeyID  *uuid.UUID
	payload   json.RawMessage
}

func (m *Manager) build(spec tokenSpec) (*Token, string, error) {
	id, s, err := mint()
	if err != nil {
		return nil, "", err
	}
	return &Token{
		ID:                      id,
		TenantID:                spec.tenantID,
		AccountID:               spec.accountID,
		Kind:                    spec.kind,
		SecretHash:              hashSecret(s),
		CreatedAt:               spec.now,
		ExpiresAt:               spec.now.Add(spec.ttl),
		IPAddress:               spec.ip,
		UserAgent:               spec.userAgent,
		Client:                  spec.client,
		ImpersonatedByAccountID: spec.asBy,
		APIKeyID:                spec.apiKeyID,
		Payload:                 spec.payload,
	}, format(spec.kind, id, s), nil
}

// child mints a token descending from another.
//
// Everything about who the session belongs to is copied rather than looked up
// again, including the impersonation marker: a session that began as an
// administrator acting as somebody else must not be able to shed that fact by
// refreshing.
//
// The application's payload is copied for the same reason in reverse: a fact
// recorded when somebody signed in should still be there after a refresh, rather
// than quietly emptying every ten minutes. Replacing it is what OnRotate is for.
func (m *Manager) child(parent *Token, kind Kind, now time.Time) (*Token, string, error) {
	ttl := m.ttl.access
	if kind == KindRefresh {
		ttl = m.ttl.refresh
	}

	t, presented, err := m.build(tokenSpec{
		kind:      kind,
		ttl:       ttl,
		now:       now,
		tenantID:  parent.TenantID,
		accountID: parent.AccountID,
		client:    parent.Client,
		ip:        parent.IPAddress,
		userAgent: parent.UserAgent,
		asBy:      parent.ImpersonatedByAccountID,
		apiKeyID:  parent.APIKeyID,
		payload:   parent.Payload,
	})
	if err != nil {
		return nil, "", err
	}

	root := parent.RootTokenID
	if root == uuid.Nil {
		root = parent.ID
	}
	t.RootTokenID = root
	parentID := parent.ID
	t.ParentTokenID = &parentID
	return t, presented, nil
}

func (m *Manager) record(ctx context.Context, e authlog.Entry) {
	m.log.Write(ctx, e)
}

// recordReuse writes the entry an investigation actually needs.
//
// "A token was replayed" is not actionable. "A token issued to a phone in
// Stockholm was replayed from a datacentre in Frankfurt" is.
func (m *Manager) recordReuse(ctx context.Context, now time.Time, prev *Token) {
	m.log.Write(ctx, authlog.Entry{
		At:          now,
		Event:       authlog.EventTokenReuseDetected,
		Outcome:     authlog.Failed,
		TenantID:    &prev.TenantID,
		AccountID:   &prev.AccountID,
		TokenRootID: &prev.RootTokenID,
		Detail: map[string]any{
			"consumed_at":         prev.RotatedAt,
			"original_ip":         prev.IPAddress,
			"original_user_agent": prev.UserAgent,
			"token_id":            prev.ID.String(),
		},
	})
}
