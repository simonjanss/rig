-- +goose Up
-- +goose StatementBegin

CREATE TYPE account_token_kind AS ENUM ('Refresh', 'Access');
CREATE TYPE account_token_client AS ENUM ('Web', 'Mobile', 'Machine');

-- One row per token, and the family root is the session.
--
-- There is no separate session table because there is nothing a session would
-- hold that its root token does not already: who, when, from where, and whether
-- it is still alive. Keeping consumed tokens rather than deleting them is what
-- makes replay detectable — a deleted row cannot tell you it was reused.
CREATE TABLE account_token (
    id                          uuid PRIMARY KEY,
    tenant_id                   uuid NOT NULL REFERENCES tenant (id),
    account_id                  uuid NOT NULL REFERENCES account (id),

    created_at                  timestamptz NOT NULL DEFAULT now(),

    kind                        account_token_kind NOT NULL,
    root_token_id               uuid NOT NULL REFERENCES account_token (id),
    parent_token_id             uuid REFERENCES account_token (id),

    secret_hash                 bytea NOT NULL,
    expires_at                  timestamptz NOT NULL,
    rotated_at                  timestamptz,
    revoked_at                  timestamptz,

    ip_address                  inet,
    user_agent                  text,
    client                      account_token_client NOT NULL DEFAULT 'Web',

    impersonated_by_account_id  uuid REFERENCES account (id),
    api_key_id                  uuid REFERENCES api_key (id),

    -- Whatever the application wants to remember about this session.
    --
    -- It is here rather than inside the token because the token is opaque: the
    -- client holds an identifier, not a document, so this is never handed out,
    -- costs nothing to change, and needs no signing. A JWT can claim none of
    -- those.
    --
    -- Not for anything that decides what somebody may do. It is written when a
    -- session is issued and again only when one is refreshed, so a permission
    -- kept here would go on being true for twelve hours after it was taken away.
    -- That is what resolving grants per request is for.
    payload                     jsonb
);

CREATE INDEX account_token_tenant_created_idx ON account_token (tenant_id, created_at DESC);
CREATE INDEX account_token_account_id_idx ON account_token (account_id);
CREATE INDEX account_token_parent_token_id_idx ON account_token (parent_token_id);
CREATE INDEX account_token_impersonated_by_account_id_idx
    ON account_token (impersonated_by_account_id);
CREATE INDEX account_token_api_key_id_idx ON account_token (api_key_id);
-- Revoking a family is one indexed UPDATE, and listing sessions is one indexed
-- read. Both go through the root.
CREATE INDEX account_token_root_token_id_idx ON account_token (root_token_id);

COMMENT ON TABLE  account_token IS 'One session token. The family root is the session.';
COMMENT ON COLUMN account_token.account_id IS 'Whose session this is.';
COMMENT ON COLUMN account_token.kind IS 'Refresh tokens are exchanged; access tokens authenticate requests.';
COMMENT ON COLUMN account_token.root_token_id IS 'The family this token belongs to. The first token of a session is its own root.';
COMMENT ON COLUMN account_token.parent_token_id IS 'The token this one was minted from, or null for a family root.';
COMMENT ON COLUMN account_token.secret_hash IS 'sha256 of the secret half. The token itself exists only in the response that issued it.';
COMMENT ON COLUMN account_token.expires_at IS 'When the token stops working, regardless of anything else.';
COMMENT ON COLUMN account_token.rotated_at IS 'When the token was first exchanged. A later use is a replay.';
COMMENT ON COLUMN account_token.revoked_at IS 'When the token was killed outright.';
COMMENT ON COLUMN account_token.ip_address IS 'Where the session was opened from.';
COMMENT ON COLUMN account_token.user_agent IS 'What opened the session.';
COMMENT ON COLUMN account_token.client IS 'What kind of thing holds the session.';
COMMENT ON COLUMN account_token.impersonated_by_account_id IS 'The administrator acting as this account, if any. It survives every rotation.';
COMMENT ON COLUMN account_token.api_key_id IS 'The key this session was minted for, if it belongs to a machine.';
COMMENT ON COLUMN account_token.payload IS 'Application context about this session. Carried forward by every rotation. Never authorization: it is only as fresh as the last refresh.';

CREATE TYPE auth_event AS ENUM (
    'LoginAttempted',
    'LoginSucceeded',
    'LoginFailed',
    'AccountLocked',
    'Logout',
    'TokenRefreshed',
    'TokenReuseDetected',
    'PasswordResetRequested',
    'PasswordResetCompleted',
    'PasswordChanged',
    'EmailVerified',
    'VerificationResent',
    'ApiKeyAuthSucceeded',
    'ApiKeyAuthFailed',
    'ImpersonationStarted',
    'ImpersonationEnded',
    'OAuthSignIn',
    'AccountProvisioned',
    'InvitationSent',
    'InvitationAccepted',
    'InvitationRevoked',
    'TenantSwitched'
);

CREATE TYPE auth_outcome AS ENUM ('Succeeded', 'Failed');

-- The audit trail and the rate-limit substrate, deliberately the same table.
--
-- A limit that counts the rows the audit records cannot drift from what really
-- happened, and there is no second store to deploy or explain during an
-- incident. It is also why the indexes below look the way they do: they are
-- shaped for the counting queries, not for browsing.
CREATE TABLE auth_log (
    id                      uuid PRIMARY KEY,
    -- Nullable, which is the one place in the foundation tenant_id is. An
    -- attempt that resolved to no tenant is a real event and the one a rate
    -- limit needs most: somebody signing in without naming a tenant, or
    -- guessing an address nobody has. Refusing to record those would mean the
    -- lockout counts nothing, because the limiter counts these rows.
    --
    -- The cost is that this table cannot be exposed through auth.expose: a
    -- generated query filters by tenant_id, so the tenant-less rows would be
    -- invisible — which is safe, and not what anybody reading an audit trail
    -- wants. Read it with a query instead.
    tenant_id               uuid REFERENCES tenant (id),

    created_at              timestamptz NOT NULL DEFAULT now(),

    event                   auth_event NOT NULL,
    outcome                 auth_outcome NOT NULL,

    account_id              uuid REFERENCES account (id),
    email_address           text,
    ip_address              inet,
    user_agent              text,
    token_root_id           uuid,
    detail                  jsonb,

    api_key_id              uuid REFERENCES api_key (id),
    api_key_ref             text
);

CREATE INDEX auth_log_tenant_created_idx ON auth_log (tenant_id, created_at DESC);
CREATE INDEX auth_log_account_id_idx ON auth_log (account_id);
CREATE INDEX auth_log_email_idx ON auth_log (lower(email_address), created_at DESC);
CREATE INDEX auth_log_ip_idx ON auth_log (ip_address, created_at DESC);
CREATE INDEX auth_log_token_root_idx ON auth_log (token_root_id, created_at DESC);
CREATE INDEX auth_log_api_key_id_idx ON auth_log (api_key_id);
CREATE INDEX auth_log_api_key_ref_idx ON auth_log (api_key_ref, created_at DESC);

COMMENT ON TABLE  auth_log IS 'Every authentication event. It is both the audit trail and what rate limits count.';
COMMENT ON COLUMN auth_log.tenant_id IS 'The tenant involved, or null when the attempt never resolved to one — an unknown address, or a sign-in that named no tenant. Those are the entries a rate limit needs most.';
COMMENT ON COLUMN auth_log.account_id IS 'The account involved, when the attempt resolved to one.';
COMMENT ON COLUMN auth_log.event IS 'What happened.';
COMMENT ON COLUMN auth_log.outcome IS 'Whether it worked.';
COMMENT ON COLUMN auth_log.email_address IS 'The address as presented, lowercased. Present even when no account matched, which is the case a limit most needs.';
COMMENT ON COLUMN auth_log.ip_address IS 'Where the attempt came from.';
COMMENT ON COLUMN auth_log.user_agent IS 'What made the attempt.';
COMMENT ON COLUMN auth_log.token_root_id IS 'The session family involved, for refresh limits and reuse investigations.';
COMMENT ON COLUMN auth_log.api_key_id IS 'The key involved, when it resolved to one.';
COMMENT ON COLUMN auth_log.api_key_ref IS 'The key identifier as presented, whether or not it exists. A limit that can only count real keys is no limit.';
COMMENT ON COLUMN auth_log.detail IS 'Anything else worth knowing, such as where a replayed token was originally issued.';

-- Somebody who has proved who they are but is not in a tenant yet.
--
-- Separate from account_token, and that separation is the point. An account_token
-- names a tenant and an account, and every generated query relies on it: claims
-- with no tenant would be claims that cannot scope a read, so the type refuses to
-- exist. This is the other state a person can be in — signed in, belonging to
-- nowhere — and it is a different credential rather than a weaker version of the
-- same one.
--
-- What it can do is correspondingly small: list the tenants this person belongs
-- to, list the invitations addressed to them, accept one, and create a tenant
-- if the application allows it. Nothing here can read a row of application data,
-- because there is no tenant to read it in.
--
-- No rotation and no family. A refresh token rotates because it is long-lived and
-- travels; this is short-lived, is exchanged for a real session as soon as
-- somebody picks a tenant, and has nothing to protect but a list of names.
CREATE TABLE identity_session (
    id                      uuid PRIMARY KEY,
    identity_id             uuid NOT NULL REFERENCES identity (id),

    secret_hash             bytea NOT NULL,

    created_at              timestamptz NOT NULL DEFAULT now(),
    expires_at              timestamptz NOT NULL,
    revoked_at              timestamptz,

    ip_address              inet,
    user_agent              text
);

CREATE INDEX identity_session_identity_id_idx ON identity_session (identity_id);

COMMENT ON TABLE  identity_session IS 'A person who has signed in but has not chosen a tenant.';
COMMENT ON COLUMN identity_session.identity_id IS 'Who proved who they are.';
COMMENT ON COLUMN identity_session.secret_hash IS 'sha256 of the token secret. The secret itself is shown once and never stored.';
COMMENT ON COLUMN identity_session.expires_at IS 'When it stops working. Short: it exists to get somebody as far as picking a tenant.';
COMMENT ON COLUMN identity_session.revoked_at IS 'When it was ended, by signing out or by choosing a tenant.';
COMMENT ON COLUMN identity_session.ip_address IS 'Where the sign-in came from.';
COMMENT ON COLUMN identity_session.user_agent IS 'What the sign-in came from.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE identity_session;
DROP TABLE auth_log;
DROP TYPE auth_outcome;
DROP TYPE auth_event;
DROP TABLE account_token;
DROP TYPE account_token_client;
DROP TYPE account_token_kind;

-- +goose StatementEnd
