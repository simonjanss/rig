package apikey

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemoryStore is an in-process Store, for tests.
type MemoryStore struct {
	mu   sync.Mutex
	keys map[uuid.UUID]*Key
}

// NewMemoryStore builds an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{keys: make(map[uuid.UUID]*Key)}
}

// Insert implements [Store].
func (s *MemoryStore) Insert(_ context.Context, k *Key) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	copied := *k
	s.keys[k.ID] = &copied
	return nil
}

// ByKeyID implements [Store].
func (s *MemoryStore) ByKeyID(_ context.Context, keyID string) (*Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, k := range s.keys {
		if k.KeyID == keyID {
			copied := *k
			return &copied, nil
		}
	}
	return nil, nil
}

// Find implements [Store].
func (s *MemoryStore) Find(_ context.Context, tenantID, id uuid.UUID) (*Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	k, ok := s.keys[id]
	if !ok || k.TenantID != tenantID {
		return nil, nil
	}
	copied := *k
	return &copied, nil
}

// TouchLastUsed implements [Store].
func (s *MemoryStore) TouchLastUsed(_ context.Context, id uuid.UUID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if k, ok := s.keys[id]; ok {
		when := at
		k.LastUsedAt = &when
	}
	return nil
}

// Revoke implements [Store].
func (s *MemoryStore) Revoke(_ context.Context, tenantID, id uuid.UUID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if k, ok := s.keys[id]; ok && k.TenantID == tenantID && k.RevokedAt == nil {
		when := at
		k.RevokedAt = &when
	}
	return nil
}

// SetExpiry implements [Store].
func (s *MemoryStore) SetExpiry(_ context.Context, tenantID, id uuid.UUID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if k, ok := s.keys[id]; ok && k.TenantID == tenantID {
		when := at
		k.ExpiresAt = &when
	}
	return nil
}

// List implements [Store].
func (s *MemoryStore) List(_ context.Context, tenantID uuid.UUID) ([]*Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*Key
	for _, k := range s.keys {
		if k.TenantID != tenantID {
			continue
		}
		copied := *k
		out = append(out, &copied)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// InTx implements [Store]. There is no transaction to open in one map.
func (s *MemoryStore) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return fn(ctx)
}
