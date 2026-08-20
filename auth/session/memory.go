package session

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemoryStore is an in-process Store.
//
// It is for tests. Rotation and reuse detection are decided in Go, not in SQL,
// which is what makes them testable without a database.
//
// Two things it cannot show, both of which have hidden a real bug: it takes no
// locks, so a concurrent rotation here proves nothing; and its InTx is a
// passthrough, so a write that a real transaction would roll back appears to
// stick. Those cases belong in the Docker suite, and there are tests for both
// of them there.
type MemoryStore struct {
	mu     sync.Mutex
	tokens map[uuid.UUID]*Token
	// identities are the tenant-less sessions, in their own map for the same
	// reason they are in their own table: one cannot be found as the other.
	identities map[uuid.UUID]*Identity
}

// NewMemoryStore builds an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		tokens:     make(map[uuid.UUID]*Token),
		identities: make(map[uuid.UUID]*Identity),
	}
}

// It satisfies both halves, so a test needs one store rather than two.
var (
	_ Store         = (*MemoryStore)(nil)
	_ IdentityStore = (*MemoryStore)(nil)
)

// Insert implements [Store].
func (s *MemoryStore) Insert(_ context.Context, t *Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	copied := *t
	s.tokens[t.ID] = &copied
	return nil
}

// Find implements [Store].
func (s *MemoryStore) Find(_ context.Context, id uuid.UUID) (*Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tokens[id]
	if !ok {
		return nil, nil
	}
	copied := *t
	return &copied, nil
}

// Lock implements [Store]. There is no lock to take in one process.
func (s *MemoryStore) Lock(ctx context.Context, id uuid.UUID) (*Token, error) {
	return s.Find(ctx, id)
}

// MarkRotated implements [Store].
func (s *MemoryStore) MarkRotated(_ context.Context, id uuid.UUID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tokens[id]
	if !ok || t.RotatedAt != nil {
		// An existing timestamp stands. A replay that pushed it forward would
		// hold the leeway open for as long as the replaying went on.
		return nil
	}
	when := at
	t.RotatedAt = &when
	return nil
}

// RevokeFamily implements [Store].
func (s *MemoryStore) RevokeFamily(_ context.Context, rootID uuid.UUID, at time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := 0
	for _, t := range s.tokens {
		if t.RootTokenID != rootID || t.RevokedAt != nil {
			continue
		}
		when := at
		t.RevokedAt = &when
		n++
	}
	return n, nil
}

// Families implements [Store].
func (s *MemoryStore) Families(_ context.Context, tenantID, accountID uuid.UUID) ([]Family, error) {
	return s.families(tenantID, &accountID), nil
}

// TenantFamilies implements [Store].
func (s *MemoryStore) TenantFamilies(_ context.Context, tenantID uuid.UUID) ([]Family, error) {
	return s.families(tenantID, nil), nil
}

// families groups a tenant's tokens into sessions, optionally for one account.
func (s *MemoryStore) families(tenantID uuid.UUID, accountID *uuid.UUID) []Family {
	s.mu.Lock()
	defer s.mu.Unlock()

	byRoot := map[uuid.UUID]*Family{}
	for _, t := range s.tokens {
		if t.TenantID != tenantID {
			continue
		}
		if accountID != nil && t.AccountID != *accountID {
			continue
		}
		f, ok := byRoot[t.RootTokenID]
		if !ok {
			f = &Family{}
			byRoot[t.RootTokenID] = f
		}
		f.Tokens++
		if t.CreatedAt.After(f.LastUsedAt) {
			f.LastUsedAt = t.CreatedAt
		}
		if t.ID == t.RootTokenID {
			copied := *t
			f.Root = &copied
		}
	}

	out := make([]Family, 0, len(byRoot))
	for _, f := range byRoot {
		if f.Root == nil {
			continue
		}
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Root.CreatedAt.After(out[j].Root.CreatedAt)
	})
	return out
}

// InTx implements [Store]. There is no transaction to open in one map.
func (s *MemoryStore) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return fn(ctx)
}

// The identity-session half of the store. Separate maps because they are
// separate credentials: an identity session is not a weaker rig_account_token, and a
// double that let one be found as the other would hide the mistake this design
// exists to make impossible.

// InsertIdentitySession implements [IdentityStore].
func (s *MemoryStore) InsertIdentitySession(_ context.Context, in *Identity) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.identities == nil {
		s.identities = map[uuid.UUID]*Identity{}
	}
	copied := *in
	s.identities[in.ID] = &copied
	return nil
}

// FindIdentitySession implements [IdentityStore].
func (s *MemoryStore) FindIdentitySession(_ context.Context, id uuid.UUID) (*Identity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	found, ok := s.identities[id]
	if !ok {
		return nil, nil
	}
	copied := *found
	return &copied, nil
}

// RevokeIdentitySession implements [IdentityStore].
func (s *MemoryStore) RevokeIdentitySession(_ context.Context, id uuid.UUID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if found, ok := s.identities[id]; ok && found.RevokedAt == nil {
		when := at
		found.RevokedAt = &when
	}
	return nil
}

// RevokeIdentitySessionsFor implements [IdentityStore].
func (s *MemoryStore) RevokeIdentitySessionsFor(_ context.Context, identityID uuid.UUID, at time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var n int
	for _, found := range s.identities {
		if found.IdentityID != identityID || found.RevokedAt != nil {
			continue
		}
		when := at
		found.RevokedAt = &when
		n++
	}
	return n, nil
}
