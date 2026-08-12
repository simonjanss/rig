package scaffold

// The foundation migrations.
//
// They follow the same conventions rig asks of any schema, because they are not
// special: `rig sync` reads them, `rig validate` checks them, and `rig generate`
// produces their repositories the same way it produces yours. Every table and
// column carries a COMMENT ON, so a freshly set-up project validates without
// anyone filling in a TODO.

const tenancySQL = `-- +goose Up
-- +goose StatementBegin

-- A tenant owns everything. Every other table carries tenant_id, and every
-- generated query filters by it, which is what makes isolation a property of
-- the data layer rather than a rule each endpoint has to remember.
CREATE TABLE tenant (
    id                      uuid PRIMARY KEY,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz,
    deleted_at              timestamptz,

    name                    text NOT NULL,
    slug                    text NOT NULL,
    is_active               boolean NOT NULL DEFAULT true,
    allowed_email_domains   text[] NOT NULL DEFAULT '{}'
);

CREATE UNIQUE INDEX tenant_slug_key ON tenant (lower(slug)) WHERE deleted_at IS NULL;

COMMENT ON TABLE  tenant IS 'An organization that owns its own data. Everything else is scoped to one.';
COMMENT ON COLUMN tenant.name IS 'Display name, as the organization writes it.';
COMMENT ON COLUMN tenant.allowed_email_domains IS 'Domains an account of this tenant may have an address in. Empty allows any, which is the right default for a tenant that is not one company.';
COMMENT ON COLUMN tenant.slug IS 'Short stable identifier used in URLs and subdomains.';
COMMENT ON COLUMN tenant.is_active IS 'Whether the tenant may be signed in to at all.';

-- Who somebody is, independent of where they work.
--
-- The split between this table and account is the one structural decision in the
-- foundation worth explaining. An identity is a person who can sign in: one
-- address, one password, one set of linked providers, no tenant. An account is
-- that person inside one tenant: their role there, their display name there,
-- whether they are still there. Somebody who belongs to two tenants has one
-- identity and two accounts, and signs in once with one password.
--
-- Keeping the per-tenant half in account rather than making membership a join
-- table is deliberate: account still carries tenant_id, so every generated query
-- is still scoped automatically and every created_by_account_id still points at
-- a row that belongs to exactly one tenant. Nothing downstream has to remember
-- anything.
CREATE TABLE identity (
    id                      uuid PRIMARY KEY,

    created_at              timestamptz NOT NULL DEFAULT now(),
    created_by_account_id   uuid,
    updated_at              timestamptz,
    updated_by_account_id   uuid,
    deleted_at              timestamptz,
    deleted_by_account_id   uuid,

    email_address           text NOT NULL,
    display_name            text NOT NULL,
    is_active               boolean NOT NULL DEFAULT true,
    email_verified_at       timestamptz
);

-- Globally unique, and compared case-insensitively. One address is one person:
-- if it were unique per tenant instead, the same human signing in to two tenants
-- would be two people with two passwords, which is the thing this table exists
-- to prevent.
CREATE UNIQUE INDEX identity_email_key
    ON identity (lower(email_address)) WHERE deleted_at IS NULL;

COMMENT ON TABLE  identity IS 'A person who can sign in. Global: one address, one password, however many tenants.';
COMMENT ON COLUMN identity.email_address IS 'How the person signs in and where mail is sent. Unique across every tenant.';
COMMENT ON COLUMN identity.display_name IS 'What to call the person before any tenant has an opinion. An account may override it.';
COMMENT ON COLUMN identity.is_active IS 'Whether the person may sign in at all, anywhere. Refused with 403, not 401.';
COMMENT ON COLUMN identity.email_verified_at IS 'When the address was confirmed, or null if it has not been. It is the address that gets verified, so this is here and not on account.';

-- Credentials live apart from the identity so that reading one never reads a
-- password hash, and so that adding a second factor later is a new table rather
-- than a wider one.
CREATE TABLE identity_credential (
    id                      uuid PRIMARY KEY,
    identity_id             uuid NOT NULL REFERENCES identity (id),

    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz,

    password_hash           text NOT NULL,
    algorithm               text NOT NULL,
    params                  jsonb NOT NULL
);

CREATE UNIQUE INDEX identity_credential_identity_id_key
    ON identity_credential (identity_id);
-- Finding every credential below the current cost has to be a query, or nobody
-- ever raises the cost.
CREATE INDEX identity_credential_algorithm_idx ON identity_credential (algorithm);

COMMENT ON TABLE  identity_credential IS 'What a person signs in with. One row per identity, so one password covers every tenant.';
COMMENT ON COLUMN identity_credential.identity_id IS 'The person these credentials belong to.';
COMMENT ON COLUMN identity_credential.password_hash IS 'The PHC-encoded hash. It carries its own salt and cost.';
COMMENT ON COLUMN identity_credential.algorithm IS 'Hashing algorithm, so rows on an old one can be found without parsing.';
COMMENT ON COLUMN identity_credential.params IS 'Cost parameters, so rows below the current cost can be found without parsing.';

CREATE TYPE identity_verification_kind AS ENUM (
    'EmailVerification',
    'PasswordReset',
    'Invitation'
);

-- One table for every single-use link, because they are the same thing: a
-- hashed token with an expiry that can be consumed once.
-- Not tenant-scoped, and the column name says so: a reset link belongs to a
-- person, not to one of the tenants they work in. It is invited_to_tenant_id
-- rather than tenant_id because tenant_id has a meaning rig acts on — every
-- generated query filters by it — and a link that is deliberately global would
-- be a table where that filter is wrong.
CREATE TABLE identity_verification (
    id                      uuid PRIMARY KEY,
    identity_id             uuid NOT NULL REFERENCES identity (id),
    invited_to_tenant_id    uuid REFERENCES tenant (id),

    created_at              timestamptz NOT NULL DEFAULT now(),

    kind                    identity_verification_kind NOT NULL,
    token_hash              bytea NOT NULL,
    expires_at              timestamptz NOT NULL,
    consumed_at             timestamptz,
    revoked_at              timestamptz
);

CREATE INDEX identity_verification_identity_id_idx
    ON identity_verification (identity_id);
CREATE INDEX identity_verification_invited_to_tenant_id_idx
    ON identity_verification (invited_to_tenant_id, created_at DESC);
CREATE UNIQUE INDEX identity_verification_token_hash_key
    ON identity_verification (token_hash);

COMMENT ON TABLE  identity_verification IS 'A single-use link: email confirmation, password reset, or invitation.';
COMMENT ON COLUMN identity_verification.identity_id IS 'The person the link is for.';
COMMENT ON COLUMN identity_verification.invited_to_tenant_id IS 'The tenant an invitation is into, or null for a link that is about the person rather than one tenant.';
COMMENT ON COLUMN identity_verification.kind IS 'What the link is for.';
COMMENT ON COLUMN identity_verification.token_hash IS 'sha256 of the token. The token itself is only ever in the mail.';
COMMENT ON COLUMN identity_verification.expires_at IS 'When the link stops working.';
COMMENT ON COLUMN identity_verification.consumed_at IS 'When it was used, or null while it is still usable.';
COMMENT ON COLUMN identity_verification.revoked_at IS 'When the link was cancelled, which is not the same as used: an invitation somebody withdrew and one somebody accepted are different things to find in an audit trail.';

-- The table is named account, not user: user is reserved in Postgres, and a
-- table you have to quote in every hand-written query is a table nobody enjoys.
-- What an account is. A service account exists so that an integration's writes
-- are attributable to something that is not a person: it has no credential and
-- cannot sign in, and deactivating the human who created it does not stop it.
CREATE TYPE account_kind AS ENUM ('Person', 'Service');

-- The coarse level, for the decisions every product makes the same way: who may
-- change billing, who may invite, who may only get on with their work. The role
-- and permission tables are the fine grain — this is one column so that "is
-- somebody an admin" does not need a join.
CREATE TYPE account_role_level AS ENUM ('Owner', 'Admin', 'Basic');

CREATE TABLE account (
    id                      uuid PRIMARY KEY,
    tenant_id               uuid NOT NULL REFERENCES tenant (id),
    -- Null exactly when this is a service account: nobody signs in as one, so
    -- there is no person for it to be. The CHECK below makes that structural
    -- rather than a convention somebody has to know.
    identity_id             uuid REFERENCES identity (id),

    created_at              timestamptz NOT NULL DEFAULT now(),
    created_by_account_id   uuid,
    updated_at              timestamptz,
    updated_by_account_id   uuid,
    deleted_at              timestamptz,
    deleted_by_account_id   uuid,

    kind                    account_kind NOT NULL DEFAULT 'Person',
    role                    account_role_level NOT NULL DEFAULT 'Basic',

    email_address           text NOT NULL,
    display_name            text NOT NULL,
    time_zone               text,
    is_active               boolean NOT NULL DEFAULT true
);

ALTER TABLE account
    ADD CONSTRAINT account_person_has_identity
        CHECK ((kind = 'Person') = (identity_id IS NOT NULL));

-- One account per person per tenant. Somebody invited twice joins once.
CREATE UNIQUE INDEX account_tenant_identity_key
    ON account (tenant_id, identity_id) WHERE deleted_at IS NULL;
-- The address is a copy of the identity's, so this index is not what keeps
-- people unique — identity_email_key is. It is here to stop the copy going
-- wrong, and because looking an account up by address is what a support query
-- does all day.
CREATE UNIQUE INDEX account_tenant_email_key
    ON account (tenant_id, lower(email_address)) WHERE deleted_at IS NULL;
CREATE INDEX account_tenant_created_idx ON account (tenant_id, created_at DESC);
CREATE INDEX account_identity_id_idx ON account (identity_id);

-- The self-references are added after the table exists so the column list above
-- stays readable.
ALTER TABLE account
    ADD CONSTRAINT account_created_by_account_id_fkey
        FOREIGN KEY (created_by_account_id) REFERENCES account (id),
    ADD CONSTRAINT account_updated_by_account_id_fkey
        FOREIGN KEY (updated_by_account_id) REFERENCES account (id),
    ADD CONSTRAINT account_deleted_by_account_id_fkey
        FOREIGN KEY (deleted_by_account_id) REFERENCES account (id);

CREATE INDEX account_created_by_account_id_idx ON account (created_by_account_id);
CREATE INDEX account_updated_by_account_id_idx ON account (updated_by_account_id);
CREATE INDEX account_deleted_by_account_id_idx ON account (deleted_by_account_id);

COMMENT ON TABLE  account IS 'One person inside one tenant. The person is the identity; this is who they are here.';
COMMENT ON COLUMN account.identity_id IS 'The person this account belongs to, or null for a service account, which is nobody.';
COMMENT ON COLUMN account.kind IS 'Whether this is a person or a service account an integration acts as.';
COMMENT ON COLUMN account.role IS 'The coarse level in this tenant: Owner, Admin, or Basic. Somebody can be an Owner here and Basic elsewhere.';
COMMENT ON COLUMN account.time_zone IS 'IANA name, for example Europe/Stockholm. Null means UTC.';
COMMENT ON COLUMN account.email_address IS 'A copy of the identity''s address, kept here so listing accounts is one query. For a service account it is a label nobody signs in with.';
COMMENT ON COLUMN account.display_name IS 'What to call the person in this tenant.';
COMMENT ON COLUMN account.is_active IS 'Whether the account may be used. A disabled account is refused with 403, not 401.';

-- identity's own audit columns, added now that account exists to reference.
-- An identity is global but the person who invited it is not, so the actor is
-- an account like everywhere else.
ALTER TABLE identity
    ADD CONSTRAINT identity_created_by_account_id_fkey
        FOREIGN KEY (created_by_account_id) REFERENCES account (id),
    ADD CONSTRAINT identity_updated_by_account_id_fkey
        FOREIGN KEY (updated_by_account_id) REFERENCES account (id),
    ADD CONSTRAINT identity_deleted_by_account_id_fkey
        FOREIGN KEY (deleted_by_account_id) REFERENCES account (id);

CREATE INDEX identity_created_by_account_id_idx ON identity (created_by_account_id);
CREATE INDEX identity_updated_by_account_id_idx ON identity (updated_by_account_id);
CREATE INDEX identity_deleted_by_account_id_idx ON identity (deleted_by_account_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- identity references account, so the references have to go before the table
-- they point at does.
ALTER TABLE identity
    DROP CONSTRAINT identity_created_by_account_id_fkey,
    DROP CONSTRAINT identity_updated_by_account_id_fkey,
    DROP CONSTRAINT identity_deleted_by_account_id_fkey;

DROP TABLE account;
DROP TYPE account_role_level;
DROP TYPE account_kind;
DROP TABLE identity_verification;
DROP TYPE identity_verification_kind;
DROP TABLE identity_credential;
DROP TABLE identity;
DROP TABLE tenant;

-- +goose StatementEnd
`

const sessionsSQL = `-- +goose Up
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
`

const apiKeysSQL = `-- +goose Up
-- +goose StatementBegin

-- Machine-to-machine credentials.
--
-- The secret is shown once, at creation, and only its sha256 is stored. sha256
-- rather than argon2id: a key secret is 256 bits from the system's random
-- source, so no amount of guessing will find it, and running a memory-hard
-- function on every machine request would be a denial of service aimed at
-- ourselves.
-- Who a key acts as. An integration key has a service account of its own, so
-- that what it writes is attributed to the integration and revoking the person
-- who set it up does not break it. A personal key acts as its owner, for
-- somebody automating their own work — the API they can reach is exactly the API
-- they can reach in the product, which is the point of it.
CREATE TYPE api_key_kind AS ENUM ('Integration', 'Personal');

CREATE TABLE api_key (
    id                      uuid PRIMARY KEY,
    tenant_id               uuid NOT NULL REFERENCES tenant (id),
    account_id              uuid NOT NULL REFERENCES account (id),

    created_at              timestamptz NOT NULL DEFAULT now(),
    created_by_account_id   uuid REFERENCES account (id),

    kind                    api_key_kind NOT NULL DEFAULT 'Integration',
    -- Self-referencing, and for the same reason as everywhere else: an
    -- integration that provisions keys should leave a trail of which of its own
    -- credentials did it.
    created_by_api_key_id   uuid REFERENCES api_key (id),
    name                    text NOT NULL,
    key_id                  text NOT NULL,
    secret_hash             bytea NOT NULL,
    scopes                  text[] NOT NULL DEFAULT '{}',
    cidr_allow_list         cidr[] NOT NULL DEFAULT '{}',

    expires_at              timestamptz,
    last_used_at            timestamptz,
    revoked_at              timestamptz
);

-- A personal key acts as the person who made it, and the database says so
-- rather than trusting whoever writes the row. Without this, "personal" would
-- be a label on a key that could quietly be acting as somebody else.
ALTER TABLE api_key
    ADD CONSTRAINT api_key_personal_is_its_own
        CHECK (kind <> 'Personal' OR account_id = created_by_account_id);

CREATE UNIQUE INDEX api_key_key_id_key ON api_key (key_id);
CREATE INDEX api_key_tenant_created_idx ON api_key (tenant_id, created_at DESC);
CREATE INDEX api_key_account_id_idx ON api_key (account_id);
CREATE INDEX api_key_created_by_account_id_idx ON api_key (created_by_account_id);
CREATE INDEX api_key_created_by_api_key_id_idx ON api_key (created_by_api_key_id);

COMMENT ON TABLE  api_key IS 'A machine-to-machine credential.';
COMMENT ON COLUMN api_key.kind IS 'Whether the key belongs to an integration, with a service account of its own, or to a person automating their own work.';
COMMENT ON COLUMN api_key.account_id IS 'The service account the key acts as, so a machine''s writes are attributable to something.';
COMMENT ON COLUMN api_key.created_by_account_id IS 'Who minted it. This is the audit trail either way — for an integration it is the person who set it up.';
COMMENT ON COLUMN api_key.name IS 'What the key is for. A list of unnamed keys is a list nobody can safely revoke from.';
COMMENT ON COLUMN api_key.key_id IS 'The public half. It appears in the presented value, in logs, and in rate limits.';
COMMENT ON COLUMN api_key.secret_hash IS 'sha256 of the secret half. The secret is shown once and never stored.';
COMMENT ON COLUMN api_key.scopes IS 'Permission keys, the same vocabulary roles use, so a key can never exceed what a role could express.';
COMMENT ON COLUMN api_key.cidr_allow_list IS 'Networks the key may be used from. Empty means anywhere.';
COMMENT ON COLUMN api_key.expires_at IS 'When the key stops working, or null for no expiry.';
COMMENT ON COLUMN api_key.last_used_at IS 'Roughly when the key was last used. Written sparingly, so key authentication stays one read.';
COMMENT ON COLUMN api_key.revoked_at IS 'When the key was killed.';

-- An account and an identity can now say which key changed them, which is the
-- rule everywhere else: a table that records who made a change records the
-- credential too, or a write from an integration is the one write nobody can
-- attribute. It is an ALTER rather than three columns in the CREATE TABLE above
-- because api_key references account and this references api_key — somebody has
-- to be second.
ALTER TABLE account
    ADD COLUMN created_by_api_key_id uuid REFERENCES api_key (id),
    ADD COLUMN updated_by_api_key_id uuid REFERENCES api_key (id),
    ADD COLUMN deleted_by_api_key_id uuid REFERENCES api_key (id);

CREATE INDEX account_created_by_api_key_id_idx ON account (created_by_api_key_id);
CREATE INDEX account_updated_by_api_key_id_idx ON account (updated_by_api_key_id);
CREATE INDEX account_deleted_by_api_key_id_idx ON account (deleted_by_api_key_id);

ALTER TABLE identity
    ADD COLUMN created_by_api_key_id uuid REFERENCES api_key (id),
    ADD COLUMN updated_by_api_key_id uuid REFERENCES api_key (id),
    ADD COLUMN deleted_by_api_key_id uuid REFERENCES api_key (id);

CREATE INDEX identity_created_by_api_key_id_idx ON identity (created_by_api_key_id);
CREATE INDEX identity_updated_by_api_key_id_idx ON identity (updated_by_api_key_id);
CREATE INDEX identity_deleted_by_api_key_id_idx ON identity (deleted_by_api_key_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE identity
    DROP COLUMN deleted_by_api_key_id,
    DROP COLUMN updated_by_api_key_id,
    DROP COLUMN created_by_api_key_id;
ALTER TABLE account
    DROP COLUMN deleted_by_api_key_id,
    DROP COLUMN updated_by_api_key_id,
    DROP COLUMN created_by_api_key_id;
DROP TABLE api_key;
DROP TYPE api_key_kind;

-- +goose StatementEnd
`

const oauthSQL = `-- +goose Up
-- +goose StatementBegin

CREATE TYPE oauth_provider AS ENUM ('Google', 'Microsoft', 'GitHub');

-- One row per provider account a person has linked.
--
-- The subject, not the address, is what identifies somebody: people change
-- their email address and providers reuse addresses, but a subject is stable
-- for the life of the account. Matching on address instead is how one person
-- ends up signed in as another.
--
-- It hangs off identity rather than account, and has no tenant_id, because a
-- Google account is one Google account: linking it once means "sign in with
-- Google" works for every tenant the person belongs to, which is what anybody
-- would expect it to mean.
CREATE TABLE identity_oauth (
    id                      uuid PRIMARY KEY,
    identity_id             uuid NOT NULL REFERENCES identity (id),

    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz,

    provider                oauth_provider NOT NULL,
    subject                 text NOT NULL,
    email_address           text NOT NULL
);

CREATE UNIQUE INDEX identity_oauth_provider_subject_key
    ON identity_oauth (provider, subject);
CREATE INDEX identity_oauth_identity_id_idx ON identity_oauth (identity_id);

COMMENT ON TABLE  identity_oauth IS 'A provider account linked to a person. Global, like the person.';
COMMENT ON COLUMN identity_oauth.identity_id IS 'The person the provider account is linked to.';
COMMENT ON COLUMN identity_oauth.provider IS 'Which provider vouched for the person.';
COMMENT ON COLUMN identity_oauth.subject IS 'The provider''s stable identifier for them. Not the address: addresses change and get reused.';
COMMENT ON COLUMN identity_oauth.email_address IS 'The address the provider reported, for display.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE identity_oauth;
DROP TYPE oauth_provider;

-- +goose StatementEnd
`
