package authpg

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/apikey"
	"github.com/simonjanss/rig/runtime/dbx"
)

// APIKeyStore keeps machine credentials in rig_api_key.
type APIKeyStore struct {
	db dbx.Conn
	tx dbx.Beginner
}

var _ apikey.Store = (*APIKeyStore)(nil)

const keyColumns = `id, tenant_id, account_id, kind, name, key_id, secret_hash, scopes,
	cidr_allow_list, created_at, created_by_account_id, expires_at, last_used_at, revoked_at`

// Insert implements [apikey.Store].
func (s *APIKeyStore) Insert(ctx context.Context, k *apikey.Key) error {
	_, err := conn(ctx, s.db).Exec(ctx, `
		INSERT INTO rig_api_key (`+keyColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		k.ID, k.TenantID, k.AccountID, k.Kind, k.Name, k.KeyID, k.SecretHash,
		emptyStrings(k.Scopes), prefixStrings(k.CIDRAllowList),
		k.CreatedAt, k.CreatedByAccountID, k.ExpiresAt, k.LastUsedAt, k.RevokedAt)
	if err != nil {
		return fmt.Errorf("authpg: insert api key: %w", err)
	}
	return nil
}

// ByKeyID implements [apikey.Store].
func (s *APIKeyStore) ByKeyID(ctx context.Context, keyID string) (*apikey.Key, error) {
	return s.one(ctx, `SELECT `+keyColumns+` FROM rig_api_key WHERE key_id = $1`, keyID)
}

// Find implements [apikey.Store].
func (s *APIKeyStore) Find(ctx context.Context, tenantID, id uuid.UUID) (*apikey.Key, error) {
	return s.one(ctx, `SELECT `+keyColumns+` FROM rig_api_key WHERE tenant_id = $1 AND id = $2`,
		tenantID, id)
}

func (s *APIKeyStore) one(ctx context.Context, sql string, args ...any) (*apikey.Key, error) {
	rows, err := conn(ctx, s.db).Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("authpg: read api key: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		return nil, rows.Err()
	}
	return scanKey(rows)
}

// TouchLastUsed implements [apikey.Store].
func (s *APIKeyStore) TouchLastUsed(ctx context.Context, id uuid.UUID, at time.Time) error {
	_, err := conn(ctx, s.db).Exec(ctx, `UPDATE rig_api_key SET last_used_at = $2 WHERE id = $1`, id, at)
	if err != nil {
		return fmt.Errorf("authpg: touch api key: %w", err)
	}
	return nil
}

// Revoke implements [apikey.Store].
func (s *APIKeyStore) Revoke(ctx context.Context, tenantID, id uuid.UUID, at time.Time) error {
	_, err := conn(ctx, s.db).Exec(ctx, `
		UPDATE rig_api_key SET revoked_at = $3
		WHERE tenant_id = $1 AND id = $2 AND revoked_at IS NULL`, tenantID, id, at)
	if err != nil {
		return fmt.Errorf("authpg: revoke api key: %w", err)
	}
	return nil
}

// SetExpiry implements [apikey.Store].
func (s *APIKeyStore) SetExpiry(ctx context.Context, tenantID, id uuid.UUID, at time.Time) error {
	_, err := conn(ctx, s.db).Exec(ctx, `
		UPDATE rig_api_key SET expires_at = $3 WHERE tenant_id = $1 AND id = $2`, tenantID, id, at)
	if err != nil {
		return fmt.Errorf("authpg: set api key expiry: %w", err)
	}
	return nil
}

// InTx implements [apikey.Store].
func (s *APIKeyStore) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return dbx.InTx(ctx, s.tx, func(ctx context.Context, _ dbx.Conn) error { return fn(ctx) })
}

// List implements [apikey.Store].
func (s *APIKeyStore) List(ctx context.Context, tenantID uuid.UUID) ([]*apikey.Key, error) {
	rows, err := conn(ctx, s.db).Query(ctx,
		`SELECT `+keyColumns+` FROM rig_api_key WHERE tenant_id = $1 ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("authpg: list api keys: %w", err)
	}
	defer rows.Close()

	var out []*apikey.Key
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func scanKey(row scanner) (*apikey.Key, error) {
	var (
		k     apikey.Key
		allow []netip.Prefix
	)
	if err := row.Scan(&k.ID, &k.TenantID, &k.AccountID, &k.Kind, &k.Name, &k.KeyID, &k.SecretHash,
		&k.Scopes, &allow, &k.CreatedAt, &k.CreatedByAccountID,
		&k.ExpiresAt, &k.LastUsedAt, &k.RevokedAt); err != nil {
		return nil, fmt.Errorf("authpg: scan api key: %w", err)
	}
	k.CIDRAllowList = allow

	// UTC on the way out, the same rule everywhere else follows. These reach an
	// endpoint's JSON, and a key's expiry rendered against the host's zone is a
	// field two replicas answer differently.
	k.CreatedAt = dbx.UTC(k.CreatedAt)
	k.ExpiresAt = dbx.UTCPtr(k.ExpiresAt)
	k.LastUsedAt = dbx.UTCPtr(k.LastUsedAt)
	k.RevokedAt = dbx.UTCPtr(k.RevokedAt)
	return &k, nil
}

// nullable turns an empty string into a NULL, so an unknown value is stored as
// unknown rather than as the empty string — which is a different thing and
// sorts differently.
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// emptyStrings keeps a nil slice out of a NOT NULL array column.
func emptyStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func prefixStrings(p []netip.Prefix) []netip.Prefix {
	if p == nil {
		return []netip.Prefix{}
	}
	return p
}

// prefixed qualifies every column in a list with a table alias, so the same
// column list can be reused in a query that joins the table to itself.
func prefixed(alias, columns string) string {
	parts := strings.Split(columns, ",")
	for i, p := range parts {
		parts[i] = alias + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}
