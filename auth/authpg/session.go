package authpg

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/session"
	"github.com/simonjanss/rig/runtime/dbx"
)

// SessionStore keeps tokens in rig_account_token.
type SessionStore struct {
	db dbx.Conn
	tx dbx.Beginner
}

// One store for both credentials, because they are the same concern read from two
// tables: what a request presented, and who it turned out to be.
var (
	_ session.Store         = (*SessionStore)(nil)
	_ session.IdentityStore = (*SessionStore)(nil)
)

const tokenColumns = `id, tenant_id, account_id, kind, root_token_id, parent_token_id,
	secret_hash, created_at, expires_at, rotated_at, revoked_at,
	ip_address, user_agent, client, impersonated_by_account_id, api_key_id, payload`

// Insert implements [session.Store].
func (s *SessionStore) Insert(ctx context.Context, t *session.Token) error {
	_, err := conn(ctx, s.db).Exec(ctx, `
		INSERT INTO rig_account_token (`+tokenColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
		t.ID, t.TenantID, t.AccountID, string(t.Kind), t.RootTokenID, t.ParentTokenID,
		t.SecretHash, t.CreatedAt, t.ExpiresAt, t.RotatedAt, t.RevokedAt,
		addrValue(t.IPAddress), nullable(t.UserAgent), string(t.Client),
		t.ImpersonatedByAccountID, t.APIKeyID, payloadValue(t.Payload))
	if err != nil {
		return fmt.Errorf("authpg: insert token: %w", err)
	}
	return nil
}

// Find implements [session.Store].
func (s *SessionStore) Find(ctx context.Context, id uuid.UUID) (*session.Token, error) {
	return s.one(ctx, `SELECT `+tokenColumns+` FROM rig_account_token WHERE id = $1`, id)
}

// Lock implements [session.Store].
//
// FOR UPDATE is the whole point. Rotation reads a token, decides whether this
// is a first use, a retry, or a replay, and writes — and two requests doing
// that at once would both see an unconsumed token and both mint a child.
func (s *SessionStore) Lock(ctx context.Context, id uuid.UUID) (*session.Token, error) {
	return s.one(ctx, `SELECT `+tokenColumns+` FROM rig_account_token WHERE id = $1 FOR UPDATE`, id)
}

func (s *SessionStore) one(ctx context.Context, sql string, args ...any) (*session.Token, error) {
	rows, err := conn(ctx, s.db).Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("authpg: read token: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("authpg: read token: %w", err)
		}
		// A token nobody has is the ordinary answer to a made-up value, not an
		// error worth propagating.
		return nil, nil
	}
	return scanToken(rows)
}

// MarkRotated implements [session.Store].
//
// The WHERE clause is what makes the leeway safe. Without `rotated_at IS NULL`
// a replay would push the timestamp forward, and somebody replaying every
// twenty seconds would hold a thirty-second window open indefinitely.
func (s *SessionStore) MarkRotated(ctx context.Context, id uuid.UUID, at time.Time) error {
	_, err := conn(ctx, s.db).Exec(ctx,
		`UPDATE rig_account_token SET rotated_at = $2 WHERE id = $1 AND rotated_at IS NULL`, id, at)
	if err != nil {
		return fmt.Errorf("authpg: mark rotated: %w", err)
	}
	return nil
}

// RevokeFamily implements [session.Store].
func (s *SessionStore) RevokeFamily(ctx context.Context, rootID uuid.UUID, at time.Time) (int, error) {
	tag, err := conn(ctx, s.db).Exec(ctx,
		`UPDATE rig_account_token SET revoked_at = $2 WHERE root_token_id = $1 AND revoked_at IS NULL`,
		rootID, at)
	if err != nil {
		return 0, fmt.Errorf("authpg: revoke family: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// Families implements [session.Store].
//
// The last-used time is derived from the newest token in each family rather
// than kept in a column, so nothing has to be written on the hot path to keep
// a "signed in devices" screen honest.
func (s *SessionStore) Families(ctx context.Context, tenantID, accountID uuid.UUID) ([]session.Family, error) {
	return s.families(ctx, `tenant_id = $1 AND account_id = $2`, tenantID, accountID)
}

// TenantFamilies implements [session.Store].
func (s *SessionStore) TenantFamilies(ctx context.Context, tenantID uuid.UUID) ([]session.Family, error) {
	return s.families(ctx, `tenant_id = $1`, tenantID)
}

// families is the grouping query both readers use. The predicate goes in the
// inner select rather than beside the join, so the aggregate counts the tokens
// of the sessions being returned and nothing else — filtering after the group
// would count them and then hide them.
func (s *SessionStore) families(ctx context.Context, where string, args ...any) ([]session.Family, error) {
	rows, err := conn(ctx, s.db).Query(ctx, `
		SELECT `+prefixed("root.", tokenColumns)+`,
		       family.last_used_at,
		       family.token_count
		FROM (
			SELECT root_token_id, max(created_at) AS last_used_at, count(*) AS token_count
			FROM rig_account_token
			WHERE `+where+`
			GROUP BY root_token_id
		) AS family
		JOIN rig_account_token root ON root.id = family.root_token_id
		WHERE root.revoked_at IS NULL AND root.expires_at > now()
		ORDER BY root.created_at DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("authpg: list sessions: %w", err)
	}
	defer rows.Close()

	var out []session.Family
	for rows.Next() {
		var (
			t          session.Token
			lastUsedAt time.Time
			count      int64
		)
		if err := scanTokenInto(rows, &t, &lastUsedAt, &count); err != nil {
			return nil, err
		}
		out = append(out, session.Family{Root: &t, LastUsedAt: lastUsedAt, Tokens: int(count)})
	}
	return out, rows.Err()
}

// InTx implements [session.Store].
func (s *SessionStore) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return dbx.InTx(ctx, s.tx, func(ctx context.Context, _ dbx.Conn) error { return fn(ctx) })
}

type scanner interface{ Scan(dest ...any) error }

// payloadValue keeps an absent payload out of the column.
//
// A nil json.RawMessage would otherwise reach Postgres as the empty string,
// which is not valid JSON and fails the insert. NULL is what "the application
// said nothing" means, and it round-trips back to nil.
func payloadValue(p json.RawMessage) any {
	if len(p) == 0 {
		return nil
	}
	return []byte(p)
}

func scanToken(row scanner) (*session.Token, error) {
	var t session.Token
	if err := scanTokenInto(row, &t, nil, nil); err != nil {
		return nil, err
	}
	return &t, nil
}

// scanTokenInto reads a token row, optionally with the two aggregate columns
// [SessionStore.Families] adds.
func scanTokenInto(row scanner, t *session.Token, lastUsedAt *time.Time, count *int64) error {
	var (
		kind      string
		client    string
		ip        *netip.Addr
		userAgent *string
	)

	dest := []any{
		&t.ID, &t.TenantID, &t.AccountID, &kind, &t.RootTokenID, &t.ParentTokenID,
		&t.SecretHash, &t.CreatedAt, &t.ExpiresAt, &t.RotatedAt, &t.RevokedAt,
		&ip, &userAgent, &client, &t.ImpersonatedByAccountID, &t.APIKeyID, &t.Payload,
	}
	if lastUsedAt != nil {
		dest = append(dest, lastUsedAt, count)
	}

	if err := row.Scan(dest...); err != nil {
		return fmt.Errorf("authpg: scan token: %w", err)
	}

	t.Kind = session.Kind(kind)
	t.Client = session.Client(client)
	t.IPAddress = addrString(ip)
	if userAgent != nil {
		t.UserAgent = *userAgent
	}

	// Every instant leaves the database in UTC, the same rule a generated
	// repository follows. pgx decodes a timestamptz into the host's zone, so
	// without this a session's timestamps are rendered against whatever TZ the
	// process happens to have — and these are the ones an endpoint puts in JSON.
	t.CreatedAt = dbx.UTC(t.CreatedAt)
	t.ExpiresAt = dbx.UTC(t.ExpiresAt)
	t.RotatedAt = dbx.UTCPtr(t.RotatedAt)
	t.RevokedAt = dbx.UTCPtr(t.RevokedAt)
	if lastUsedAt != nil {
		*lastUsedAt = dbx.UTC(*lastUsedAt)
	}
	return nil
}

const identitySessionColumns = `id, identity_id, secret_hash, created_at,
	expires_at, revoked_at, ip_address, user_agent`

// InsertIdentitySession implements [session.IdentityStore].
func (s *SessionStore) InsertIdentitySession(ctx context.Context, in *session.Identity) error {
	_, err := conn(ctx, s.db).Exec(ctx, `
		INSERT INTO rig_identity_session (`+identitySessionColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		in.ID, in.IdentityID, in.SecretHash, in.CreatedAt,
		in.ExpiresAt, in.RevokedAt, addrValue(in.IPAddress), nullable(in.UserAgent))
	if err != nil {
		return fmt.Errorf("authpg: insert identity session: %w", err)
	}
	return nil
}

// FindIdentitySession implements [session.IdentityStore].
func (s *SessionStore) FindIdentitySession(ctx context.Context, id uuid.UUID) (*session.Identity, error) {
	rows, err := conn(ctx, s.db).Query(ctx,
		`SELECT `+identitySessionColumns+` FROM rig_identity_session WHERE id = $1`, id)
	if err != nil {
		return nil, fmt.Errorf("authpg: read identity session: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("authpg: read identity session: %w", err)
		}
		// A session nobody has is the ordinary answer to a made-up value.
		return nil, nil
	}

	var (
		out       session.Identity
		ip        *netip.Addr
		userAgent *string
	)
	if err := rows.Scan(&out.ID, &out.IdentityID, &out.SecretHash, &out.CreatedAt,
		&out.ExpiresAt, &out.RevokedAt, &ip, &userAgent); err != nil {
		return nil, fmt.Errorf("authpg: scan identity session: %w", err)
	}

	out.IPAddress = addrString(ip)
	if userAgent != nil {
		out.UserAgent = *userAgent
	}

	// UTC out of the database, the same rule every other instant follows.
	out.CreatedAt, out.ExpiresAt = out.CreatedAt.UTC(), out.ExpiresAt.UTC()
	if out.RevokedAt != nil {
		when := out.RevokedAt.UTC()
		out.RevokedAt = &when
	}
	return &out, nil
}

// RevokeIdentitySession implements [session.IdentityStore].
//
// `revoked_at IS NULL` keeps the first revocation's timestamp, which is the one
// that says when the session actually ended.
func (s *SessionStore) RevokeIdentitySession(ctx context.Context, id uuid.UUID, at time.Time) error {
	_, err := conn(ctx, s.db).Exec(ctx,
		`UPDATE rig_identity_session SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL`, id, at)
	if err != nil {
		return fmt.Errorf("authpg: revoke identity session: %w", err)
	}
	return nil
}

// RevokeIdentitySessionsFor implements [session.IdentityStore].
func (s *SessionStore) RevokeIdentitySessionsFor(ctx context.Context, identityID uuid.UUID, at time.Time) (int, error) {
	tag, err := conn(ctx, s.db).Exec(ctx,
		`UPDATE rig_identity_session SET revoked_at = $2
		  WHERE identity_id = $1 AND revoked_at IS NULL`, identityID, at)
	if err != nil {
		return 0, fmt.Errorf("authpg: revoke identity sessions: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
