package account

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemoryStore is an in-process Store, for tests.
type MemoryStore struct {
	mu            sync.Mutex
	identities    map[uuid.UUID]*Identity
	accounts      map[uuid.UUID]*Account
	credentials   map[uuid.UUID]*Credential // by identity
	verifications map[uuid.UUID]*Verification

	// accountOrder is the order accounts were added, which stands in for
	// created_at.
	//
	// The Postgres store orders by that column and [Account] does not carry it,
	// so there is nothing here to sort by — and ranging the map instead would
	// make "the tenant they joined first" a coin flip, which is exactly the
	// thing [Service.accountFor] promises. A double that disagrees with the real
	// store about the property under test is worse than no double.
	accountOrder []uuid.UUID

	// Domains are the allowed email domains per tenant, for the tests that care
	// about provisioning. Absent means no restriction, which is what an ordinary
	// tenant has.
	Domains map[uuid.UUID][]string
	// TenantNames name the tenants a tenant list reports.
	TenantNames map[uuid.UUID]string
}

// Insert implements [Store].
func (s *MemoryStore) Insert(ctx context.Context, a *Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.record(a)
	return nil
}

// record stores an account and remembers where it came in the sequence. Callers
// hold the lock.
func (s *MemoryStore) record(a *Account) {
	if _, seen := s.accounts[a.ID]; !seen {
		s.accountOrder = append(s.accountOrder, a.ID)
	}
	copied := *a
	s.accounts[a.ID] = &copied
}

// eachAccount visits every account in the order it was added.
func (s *MemoryStore) eachAccount(fn func(*Account)) {
	for _, id := range s.accountOrder {
		if a, ok := s.accounts[id]; ok {
			fn(a)
		}
	}
}

// InsertTenant implements [Store].
func (s *MemoryStore) InsertTenant(_ context.Context, t *Tenant) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.TenantNames == nil {
		s.TenantNames = map[uuid.UUID]string{}
	}
	// The double keeps only what it is asked about. A tenant here is a name — the
	// rest of the row is columns nothing in this package reads back.
	s.TenantNames[t.ID] = t.Name
	if len(t.AllowedEmailDomains) > 0 {
		if s.Domains == nil {
			s.Domains = map[uuid.UUID][]string{}
		}
		s.Domains[t.ID] = t.AllowedEmailDomains
	}
	return nil
}

// TenantDomains implements [Store].
func (s *MemoryStore) TenantDomains(ctx context.Context, tenantID uuid.UUID) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.Domains[tenantID], nil
}

// NewMemoryStore builds an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		identities:    make(map[uuid.UUID]*Identity),
		accounts:      make(map[uuid.UUID]*Account),
		credentials:   make(map[uuid.UUID]*Credential),
		verifications: make(map[uuid.UUID]*Verification),
	}
}

// Put adds an account.
func (s *MemoryStore) Put(a *Account) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.record(a)
}

// PutIdentity adds a person.
func (s *MemoryStore) PutIdentity(i *Identity) {
	s.mu.Lock()
	defer s.mu.Unlock()

	copied := *i
	s.identities[i.ID] = &copied
}

// PutPerson adds a person and their account in one call, which is what almost
// every test wants: an identity with nobody in it, or an account behind nobody,
// is a state the flows are entitled to refuse.
func (s *MemoryStore) PutPerson(i *Identity, a *Account) {
	a.IdentityID = &i.ID
	if a.EmailAddress == "" {
		a.EmailAddress = i.EmailAddress
	}
	s.PutIdentity(i)
	s.Put(a)
}

// InsertIdentity implements [Store].
func (s *MemoryStore) InsertIdentity(_ context.Context, i *Identity) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	copied := *i
	s.identities[i.ID] = &copied
	return nil
}

// FindIdentityByEmail implements [Store].
func (s *MemoryStore) FindIdentityByEmail(_ context.Context, lowercased string) (*Identity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, i := range s.identities {
		if normalizeEmail(i.EmailAddress) == lowercased {
			copied := *i
			return &copied, nil
		}
	}
	return nil, nil
}

// FindIdentityByID implements [Store].
func (s *MemoryStore) FindIdentityByID(_ context.Context, id uuid.UUID) (*Identity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	i, ok := s.identities[id]
	if !ok {
		return nil, nil
	}
	copied := *i
	return &copied, nil
}

// FindByID implements [Store].
func (s *MemoryStore) FindByID(_ context.Context, tenantID, id uuid.UUID) (*Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	a, ok := s.accounts[id]
	if !ok || a.TenantID != tenantID {
		return nil, nil
	}
	copied := *a
	return &copied, nil
}

// AccountForIdentity implements [Store].
func (s *MemoryStore) AccountForIdentity(_ context.Context, tenantID, identityID uuid.UUID) (*Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, a := range s.accounts {
		if a.TenantID == tenantID && a.IdentityID != nil && *a.IdentityID == identityID {
			copied := *a
			return &copied, nil
		}
	}
	return nil, nil
}

// AccountsForIdentity implements [Store].
//
// Oldest first, which is not decoration: signing in without naming a tenant puts
// somebody in the one they joined first, so the order *is* the contract.
func (s *MemoryStore) AccountsForIdentity(_ context.Context, identityID uuid.UUID) ([]*Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*Account
	s.eachAccount(func(a *Account) {
		if a.IdentityID != nil && *a.IdentityID == identityID {
			copied := *a
			out = append(out, &copied)
		}
	})
	return out, nil
}

// TenantsForIdentity implements [Store].
//
// TenantNames is how a test gives a tenant a name; an absent one is reported as
// its identifier, which is enough for the flows that only care about the set.
func (s *MemoryStore) TenantsForIdentity(_ context.Context, identityID uuid.UUID) ([]Membership, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []Membership
	s.eachAccount(func(a *Account) {
		if a.IdentityID == nil || *a.IdentityID != identityID {
			return
		}
		name, ok := s.TenantNames[a.TenantID]
		if !ok {
			name = a.TenantID.String()
		}
		out = append(out, Membership{
			TenantID: a.TenantID, TenantName: name, TenantSlug: name,
			AccountID: a.ID, Role: a.Role, IsActive: a.IsActive,
		})
	})
	return out, nil
}

// MarkIdentityVerified implements [Store].
func (s *MemoryStore) MarkIdentityVerified(_ context.Context, identityID uuid.UUID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if i, ok := s.identities[identityID]; ok {
		when := at
		i.EmailVerifiedAt = &when
	}
	return nil
}

// Credential implements [Store].
func (s *MemoryStore) Credential(_ context.Context, identityID uuid.UUID) (*Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.credentials[identityID]
	if !ok {
		return nil, nil
	}
	copied := *c
	return &copied, nil
}

// SaveCredential implements [Store].
func (s *MemoryStore) SaveCredential(_ context.Context, c *Credential) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	copied := *c
	s.credentials[c.IdentityID] = &copied
	return nil
}

// CreateVerification implements [Store].
func (s *MemoryStore) CreateVerification(_ context.Context, v *Verification) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	copied := *v
	s.verifications[v.ID] = &copied
	return nil
}

// PendingInvitations implements [Store].
func (s *MemoryStore) PendingInvitations(_ context.Context, tenantID uuid.UUID) ([]Invitation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []Invitation
	for _, v := range s.verifications {
		if v.Kind != KindInvitation || v.InvitedToTenantID == nil || *v.InvitedToTenantID != tenantID {
			continue
		}
		if v.ConsumedAt != nil || v.RevokedAt != nil {
			continue
		}
		ident, ok := s.identities[v.IdentityID]
		if !ok {
			continue
		}
		for _, a := range s.accounts {
			if a.TenantID != tenantID || a.IdentityID == nil || *a.IdentityID != v.IdentityID {
				continue
			}
			out = append(out, s.invitationOf(v, ident, a))
		}
	}
	return out, nil
}

// InvitationsForIdentity implements [Store].
func (s *MemoryStore) InvitationsForIdentity(_ context.Context, identityID uuid.UUID) ([]Invitation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ident, ok := s.identities[identityID]
	if !ok {
		return nil, nil
	}

	var out []Invitation
	for _, v := range s.verifications {
		if v.Kind != KindInvitation || v.IdentityID != identityID {
			continue
		}
		if v.ConsumedAt != nil || v.RevokedAt != nil || v.InvitedToTenantID == nil {
			continue
		}
		s.eachAccount(func(a *Account) {
			if a.TenantID != *v.InvitedToTenantID || a.IdentityID == nil || *a.IdentityID != identityID {
				return
			}
			out = append(out, s.invitationOf(v, ident, a))
		})
	}
	return out, nil
}

// invitationOf assembles the view both invitation queries answer with. Callers
// hold the lock.
func (s *MemoryStore) invitationOf(v *Verification, ident *Identity, a *Account) Invitation {
	name, ok := s.TenantNames[a.TenantID]
	if !ok {
		name = a.TenantID.String()
	}
	return Invitation{
		ID: v.ID, IdentityID: v.IdentityID, AccountID: a.ID,
		TenantID: a.TenantID, TenantName: name,
		EmailAddress: ident.EmailAddress, DisplayName: ident.DisplayName,
		Role: a.Role, CreatedAt: v.CreatedAt, ExpiresAt: v.ExpiresAt,
	}
}

// RevokeVerification implements [Store].
func (s *MemoryStore) RevokeVerification(_ context.Context, id uuid.UUID, at time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	v, ok := s.verifications[id]
	if !ok || v.ConsumedAt != nil || v.RevokedAt != nil {
		return false, nil
	}
	when := at
	v.RevokedAt = &when
	return true, nil
}

// SoftDeleteAccount implements [Store].
func (s *MemoryStore) SoftDeleteAccount(_ context.Context, in DeleteAccountInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Deleting for real, because the memory store has no lifecycle columns and
	// what every caller checks is whether the account is still found.
	if a, ok := s.accounts[in.AccountID]; ok && a.TenantID == in.TenantID {
		delete(s.accounts, in.AccountID)
	}
	return nil
}

// VerificationByHash implements [Store].
func (s *MemoryStore) VerificationByHash(_ context.Context, hash []byte) (*Verification, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, v := range s.verifications {
		if bytes.Equal(v.TokenHash, hash) {
			copied := *v
			return &copied, nil
		}
	}
	return nil, nil
}

// VerificationByID implements [Store].
func (s *MemoryStore) VerificationByID(_ context.Context, id uuid.UUID) (*Verification, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	v, ok := s.verifications[id]
	if !ok {
		return nil, nil
	}
	copied := *v
	return &copied, nil
}

// ConsumeVerification implements [Store].
func (s *MemoryStore) ConsumeVerification(_ context.Context, id uuid.UUID, at time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	v, ok := s.verifications[id]
	if !ok || v.ConsumedAt != nil {
		return false, nil
	}
	when := at
	v.ConsumedAt = &when
	return true, nil
}

// InTx implements [Store].
func (s *MemoryStore) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return fn(ctx)
}

// MemoryOutbox is an [Outbox] over a map, for tests and for development.
//
// It is the same kind of double [MemoryStore] is, with the same rule: a double
// that disagrees with the real store about the property under test is worse than
// no double. So the claim charges an attempt before anything is sent, the lease
// is honoured by wall-clock comparison rather than ignored, and RotateToken
// refuses a link that is already settled — those three are what the queue's
// correctness rests on, and a version that skipped them would prove nothing.
//
// What it does not have is concurrency across processes. One mutex is not
// SKIP LOCKED, and the tests that care about two dispatchers racing are the
// Docker ones.
type MemoryOutbox struct {
	mu         sync.Mutex
	deliveries map[uuid.UUID]*Delivery
	// order is insertion order, standing in for the claim query's ORDER BY
	// deliver_at — ranging the map instead would make which row a bounded pass
	// takes a coin flip.
	order []uuid.UUID
	// claims stands in for the claimed_at and claimed_by columns. Honoured
	// rather than ignored, because a lease that is never checked is not a lease
	// and the tests here would prove nothing about one.
	claims map[uuid.UUID]mailClaim

	// FailedReason is what the notifier last said about each delivery, exposed
	// so a test can assert on the sentence rather than only on the state.
	FailedReason map[uuid.UUID]string

	// store is the one MemoryStore this outbox belongs to, needed because
	// rotating a token writes to the verification the delivery owns.
	store *MemoryStore
}

// mailClaim is one lease: who holds it and since when.
type mailClaim struct {
	by uuid.UUID
	at time.Time
}

// NewMemoryOutbox builds one over a store, which it needs because rotating a
// token writes to the verification the delivery owns.
func NewMemoryOutbox(store *MemoryStore) *MemoryOutbox {
	return &MemoryOutbox{
		deliveries:   map[uuid.UUID]*Delivery{},
		claims:       map[uuid.UUID]mailClaim{},
		FailedReason: map[uuid.UUID]string{},
		store:        store,
	}
}

// The claim bookkeeping, both callers of which already hold the mutex.

func (o *MemoryOutbox) setClaim(id, by uuid.UUID, at time.Time) {
	o.claims[id] = mailClaim{by: by, at: at}
}

func (o *MemoryOutbox) dropClaim(id uuid.UUID) { delete(o.claims, id) }

// Enqueue implements [Outbox].
func (o *MemoryOutbox) Enqueue(_ context.Context, d *Delivery) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	// The unique index on verification_id, which is what makes the enqueue
	// idempotent and makes "the row this delivery owns" a phrase with one
	// referent.
	for _, existing := range o.deliveries {
		if existing.VerificationID == d.VerificationID {
			return fmt.Errorf("account: a delivery for %s already exists", d.VerificationID)
		}
	}

	copied := *d
	o.deliveries[d.ID] = &copied
	o.order = append(o.order, d.ID)
	return nil
}

// Claim implements [Outbox].
func (o *MemoryOutbox) Claim(_ context.Context, by uuid.UUID, now time.Time, ttl time.Duration, limit int) ([]Delivery, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	var out []Delivery
	for _, id := range o.order {
		if len(out) >= limit {
			break
		}
		d := o.deliveries[id]
		if d == nil || d.State != DeliveryPending || d.DeliverAt.After(now) {
			continue
		}
		if held, ok := o.claims[id]; ok && held.at.After(now.Add(-ttl)) {
			// Somebody else's lease, and it has not expired.
			continue
		}

		// The attempt is charged by the claim, before anything is sent, which is
		// what makes Abandon's job — giving one back — a thing that has to exist.
		d.Attempts++
		o.setClaim(id, by, now)
		out = append(out, *d)
	}
	return out, nil
}

// RotateToken implements [Outbox].
func (o *MemoryOutbox) RotateToken(_ context.Context, verificationID uuid.UUID, hash []byte, expiresAt time.Time) (bool, error) {
	o.store.mu.Lock()
	defer o.store.mu.Unlock()

	v := o.store.verifications[verificationID]
	// The check and the write are one step, because two passes racing over one
	// link must not both come away believing they hold the live token.
	if v == nil || v.ConsumedAt != nil || v.RevokedAt != nil {
		return false, nil
	}
	v.TokenHash = hash
	v.ExpiresAt = expiresAt
	return true, nil
}

// MarkSent implements [Outbox].
func (o *MemoryOutbox) MarkSent(_ context.Context, id uuid.UUID, _ time.Time) error {
	return o.settle(id, DeliverySent, "")
}

// MarkFailed implements [Outbox].
func (o *MemoryOutbox) MarkFailed(_ context.Context, id uuid.UUID, reason string, _ time.Time) error {
	return o.settle(id, DeliveryFailed, reason)
}

// MarkSkipped implements [Outbox].
func (o *MemoryOutbox) MarkSkipped(_ context.Context, id uuid.UUID, reason string, _ time.Time) error {
	return o.settle(id, DeliverySkipped, reason)
}

func (o *MemoryOutbox) settle(id uuid.UUID, state DeliveryState, reason string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	d := o.deliveries[id]
	if d == nil {
		return nil
	}
	d.State = state
	o.FailedReason[id] = reason
	o.dropClaim(id)
	return nil
}

// Retry implements [Outbox].
func (o *MemoryOutbox) Retry(_ context.Context, id uuid.UUID, at time.Time, reason string, _ time.Time) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	d := o.deliveries[id]
	if d == nil {
		return nil
	}
	d.DeliverAt = at
	o.FailedReason[id] = reason
	o.dropClaim(id)
	return nil
}

// Abandon implements [Outbox].
//
// The attempt goes back with the claim. The claim charged every row in the batch
// before anything was sent, so a row released without a send would have paid for
// one it never got — and MaxAttempts of those would fail a delivery no notifier
// had ever been asked about.
func (o *MemoryOutbox) Abandon(_ context.Context, ids []uuid.UUID, by uuid.UUID, _ time.Time) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, id := range ids {
		d := o.deliveries[id]
		// by is checked for the reason the real statement checks claimed_by: past
		// a lease that expired anyway, the row may be somebody else's now, and
		// the attempt to give back would be theirs.
		if d == nil || d.State != DeliveryPending || o.claims[id].by != by {
			continue
		}
		d.Attempts = max(d.Attempts-1, 0)
		o.dropClaim(id)
	}
	return nil
}

// ReleaseClaims implements [Outbox].
//
// The attempts stay, which is the difference from Abandon: the sends this pass
// made were made.
func (o *MemoryOutbox) ReleaseClaims(_ context.Context, by uuid.UUID, _ time.Time) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var n int
	for id, held := range o.claims {
		if held.by != by {
			continue
		}
		o.dropClaim(id)
		n++
	}
	return n, nil
}

// Deliveries is every row, for a test that wants to assert on the queue itself.
func (o *MemoryOutbox) Deliveries() []Delivery {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]Delivery, 0, len(o.order))
	for _, id := range o.order {
		if d := o.deliveries[id]; d != nil {
			out = append(out, *d)
		}
	}
	return out
}

// CountVerifications is how many link rows exist, for the test that says a retry
// writes none.
func (s *MemoryStore) CountVerifications() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.verifications)
}

// DeactivateIdentity switches somebody off, for the test that says a deactivated
// person is not mailed a working link.
func (s *MemoryStore) DeactivateIdentity(id uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := s.identities[id]; i != nil {
		i.IsActive = false
	}
}
