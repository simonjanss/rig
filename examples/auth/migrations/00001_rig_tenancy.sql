-- +goose Up
-- +goose StatementBegin

-- A tenant owns everything. Every other table carries tenant_id, and every
-- generated query filters by it, which is what makes isolation a property of
-- the data layer rather than a rule each endpoint has to remember.
CREATE TABLE rig_tenant (
    id                      uuid PRIMARY KEY,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz,
    deleted_at              timestamptz,

    name                    text NOT NULL,
    slug                    text NOT NULL,
    is_active               boolean NOT NULL DEFAULT true,
    allowed_email_domains   text[] NOT NULL DEFAULT '{}'
);

CREATE UNIQUE INDEX rig_tenant_slug_key ON rig_tenant (lower(slug)) WHERE deleted_at IS NULL;

COMMENT ON TABLE  rig_tenant IS 'An organization that owns its own data. Everything else is scoped to one.';
COMMENT ON COLUMN rig_tenant.name IS 'Display name, as the organization writes it.';
COMMENT ON COLUMN rig_tenant.allowed_email_domains IS 'Domains an account of this tenant may have an address in. Empty allows any, which is the right default for a tenant that is not one company.';
COMMENT ON COLUMN rig_tenant.slug IS 'Short stable identifier used in URLs and subdomains.';
COMMENT ON COLUMN rig_tenant.is_active IS 'Whether the tenant may be signed in to at all.';

-- Who somebody is, independent of where they work.
--
-- The split between this table and rig_account is the one structural decision in the
-- foundation worth explaining. An identity is a person who can sign in: one
-- address, one password, one set of linked providers, no tenant. An account is
-- that person inside one tenant: their role there, their display name there,
-- whether they are still there. Somebody who belongs to two tenants has one
-- identity and two accounts, and signs in once with one password.
--
-- Keeping the per-tenant half in rig_account rather than making membership a join
-- table is deliberate: it still carries tenant_id, so every generated query
-- is still scoped automatically and every created_by_account_id still points at
-- a row that belongs to exactly one tenant. Nothing downstream has to remember
-- anything.
CREATE TABLE rig_identity (
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
CREATE UNIQUE INDEX rig_identity_email_key
    ON rig_identity (lower(email_address)) WHERE deleted_at IS NULL;

COMMENT ON TABLE  rig_identity IS 'A person who can sign in. Global: one address, one password, however many tenants.';
COMMENT ON COLUMN rig_identity.email_address IS 'How the person signs in and where mail is sent. Unique across every tenant.';
COMMENT ON COLUMN rig_identity.display_name IS 'What to call the person before any tenant has an opinion. An account may override it.';
COMMENT ON COLUMN rig_identity.is_active IS 'Whether the person may sign in at all, anywhere. Refused with 403, not 401.';
COMMENT ON COLUMN rig_identity.email_verified_at IS 'When the address was confirmed, or null if it has not been. It is the address that gets verified, so this is here and not on account.';

-- Credentials live apart from the identity so that reading one never reads a
-- password hash, and so that adding a second factor later is a new table rather
-- than a wider one.
CREATE TABLE rig_identity_credential (
    id                      uuid PRIMARY KEY,
    identity_id             uuid NOT NULL REFERENCES rig_identity (id),

    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz,

    password_hash           text NOT NULL,
    algorithm               text NOT NULL,
    params                  jsonb NOT NULL
);

CREATE UNIQUE INDEX rig_identity_credential_identity_id_key
    ON rig_identity_credential (identity_id);
-- Finding every credential below the current cost has to be a query, or nobody
-- ever raises the cost.
CREATE INDEX rig_identity_credential_algorithm_idx ON rig_identity_credential (algorithm);

COMMENT ON TABLE  rig_identity_credential IS 'What a person signs in with. One row per identity, so one password covers every tenant.';
COMMENT ON COLUMN rig_identity_credential.identity_id IS 'The person these credentials belong to.';
COMMENT ON COLUMN rig_identity_credential.password_hash IS 'The PHC-encoded hash. It carries its own salt and cost.';
COMMENT ON COLUMN rig_identity_credential.algorithm IS 'Hashing algorithm, so rows on an old one can be found without parsing.';
COMMENT ON COLUMN rig_identity_credential.params IS 'Cost parameters, so rows below the current cost can be found without parsing.';

CREATE TYPE rig_identity_verification_kind AS ENUM (
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
CREATE TABLE rig_identity_verification (
    id                      uuid PRIMARY KEY,
    identity_id             uuid NOT NULL REFERENCES rig_identity (id),
    invited_to_tenant_id    uuid REFERENCES rig_tenant (id),

    created_at              timestamptz NOT NULL DEFAULT now(),

    kind                    rig_identity_verification_kind NOT NULL,
    token_hash              bytea NOT NULL,
    expires_at              timestamptz NOT NULL,
    consumed_at             timestamptz,
    revoked_at              timestamptz
);

CREATE INDEX rig_identity_verification_identity_id_idx
    ON rig_identity_verification (identity_id);
CREATE INDEX rig_identity_verification_invited_to_tenant_id_idx
    ON rig_identity_verification (invited_to_tenant_id, created_at DESC);
CREATE UNIQUE INDEX rig_identity_verification_token_hash_key
    ON rig_identity_verification (token_hash);

COMMENT ON TABLE  rig_identity_verification IS 'A single-use link: email confirmation, password reset, or invitation.';
COMMENT ON COLUMN rig_identity_verification.identity_id IS 'The person the link is for.';
COMMENT ON COLUMN rig_identity_verification.invited_to_tenant_id IS 'The tenant an invitation is into, or null for a link that is about the person rather than one tenant.';
COMMENT ON COLUMN rig_identity_verification.kind IS 'What the link is for.';
COMMENT ON COLUMN rig_identity_verification.token_hash IS 'sha256 of the token. The token itself is only ever in the mail.';
COMMENT ON COLUMN rig_identity_verification.expires_at IS 'When the link stops working.';
COMMENT ON COLUMN rig_identity_verification.consumed_at IS 'When it was used, or null while it is still usable.';
COMMENT ON COLUMN rig_identity_verification.revoked_at IS 'When the link was cancelled, which is not the same as used: an invitation somebody withdrew and one somebody accepted are different things to find in an audit trail.';

-- The noun is account, not user: user is reserved in Postgres, and a table you
-- have to quote in every hand-written query is a table nobody enjoys. The rig_
-- prefix in front of it is what keeps the foundation's tables apart from yours.
-- What an account is. A service account exists so that an integration's writes
-- are attributable to something that is not a person: it has no credential and
-- cannot sign in, and deactivating the human who created it does not stop it.
CREATE TYPE rig_account_kind AS ENUM ('Person', 'Service');

-- The coarse level, for the decisions every product makes the same way: who may
-- change billing, who may invite, who may only get on with their work. The role
-- and permission tables are the fine grain — this is one column so that "is
-- somebody an admin" does not need a join.
CREATE TYPE rig_account_role_level AS ENUM ('Owner', 'Admin', 'Basic');

CREATE TABLE rig_account (
    id                      uuid PRIMARY KEY,
    tenant_id               uuid NOT NULL REFERENCES rig_tenant (id),
    -- Null exactly when this is a service account: nobody signs in as one, so
    -- there is no person for it to be. The CHECK below makes that structural
    -- rather than a convention somebody has to know.
    identity_id             uuid REFERENCES rig_identity (id),

    created_at              timestamptz NOT NULL DEFAULT now(),
    created_by_account_id   uuid,
    updated_at              timestamptz,
    updated_by_account_id   uuid,
    deleted_at              timestamptz,
    deleted_by_account_id   uuid,

    kind                    rig_account_kind NOT NULL DEFAULT 'Person',
    role                    rig_account_role_level NOT NULL DEFAULT 'Basic',

    email_address           text NOT NULL,
    display_name            text NOT NULL,
    time_zone               text,
    is_active               boolean NOT NULL DEFAULT true
);

ALTER TABLE rig_account
    ADD CONSTRAINT rig_account_person_has_identity
        CHECK ((kind = 'Person') = (identity_id IS NOT NULL));

-- One account per person per tenant. Somebody invited twice joins once.
CREATE UNIQUE INDEX rig_account_tenant_identity_key
    ON rig_account (tenant_id, identity_id) WHERE deleted_at IS NULL;
-- The address is a copy of the identity's, so this index is not what keeps
-- people unique — rig_identity_email_key is. It is here to stop the copy going
-- wrong, and because looking an account up by address is what a support query
-- does all day.
CREATE UNIQUE INDEX rig_account_tenant_email_key
    ON rig_account (tenant_id, lower(email_address)) WHERE deleted_at IS NULL;
CREATE INDEX rig_account_tenant_created_idx ON rig_account (tenant_id, created_at DESC);
CREATE INDEX rig_account_identity_id_idx ON rig_account (identity_id);

-- The self-references are added after the table exists so the column list above
-- stays readable.
ALTER TABLE rig_account
    ADD CONSTRAINT rig_account_created_by_account_id_fkey
        FOREIGN KEY (created_by_account_id) REFERENCES rig_account (id),
    ADD CONSTRAINT rig_account_updated_by_account_id_fkey
        FOREIGN KEY (updated_by_account_id) REFERENCES rig_account (id),
    ADD CONSTRAINT rig_account_deleted_by_account_id_fkey
        FOREIGN KEY (deleted_by_account_id) REFERENCES rig_account (id);

CREATE INDEX rig_account_created_by_account_id_idx ON rig_account (created_by_account_id);
CREATE INDEX rig_account_updated_by_account_id_idx ON rig_account (updated_by_account_id);
CREATE INDEX rig_account_deleted_by_account_id_idx ON rig_account (deleted_by_account_id);

COMMENT ON TABLE  rig_account IS 'One person inside one tenant. The person is the identity; this is who they are here.';
COMMENT ON COLUMN rig_account.identity_id IS 'The person this account belongs to, or null for a service account, which is nobody.';
COMMENT ON COLUMN rig_account.kind IS 'Whether this is a person or a service account an integration acts as.';
COMMENT ON COLUMN rig_account.role IS 'The coarse level in this tenant: Owner, Admin, or Basic. Somebody can be an Owner here and Basic elsewhere.';
COMMENT ON COLUMN rig_account.time_zone IS 'IANA name, for example Europe/Stockholm. Null means UTC.';
COMMENT ON COLUMN rig_account.email_address IS 'A copy of the identity''s address, kept here so listing accounts is one query. For a service account it is a label nobody signs in with.';
COMMENT ON COLUMN rig_account.display_name IS 'What to call the person in this tenant.';
COMMENT ON COLUMN rig_account.is_active IS 'Whether the account may be used. A disabled account is refused with 403, not 401.';

-- rig_identity's own audit columns, added now that rig_account exists to reference.
-- An identity is global but the person who invited it is not, so the actor is
-- an account like everywhere else.
ALTER TABLE rig_identity
    ADD CONSTRAINT rig_identity_created_by_account_id_fkey
        FOREIGN KEY (created_by_account_id) REFERENCES rig_account (id),
    ADD CONSTRAINT rig_identity_updated_by_account_id_fkey
        FOREIGN KEY (updated_by_account_id) REFERENCES rig_account (id),
    ADD CONSTRAINT rig_identity_deleted_by_account_id_fkey
        FOREIGN KEY (deleted_by_account_id) REFERENCES rig_account (id);

CREATE INDEX rig_identity_created_by_account_id_idx ON rig_identity (created_by_account_id);
CREATE INDEX rig_identity_updated_by_account_id_idx ON rig_identity (updated_by_account_id);
CREATE INDEX rig_identity_deleted_by_account_id_idx ON rig_identity (deleted_by_account_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- rig_identity references rig_account, so the references have to go before the table
-- they point at does.
ALTER TABLE rig_identity
    DROP CONSTRAINT rig_identity_created_by_account_id_fkey,
    DROP CONSTRAINT rig_identity_updated_by_account_id_fkey,
    DROP CONSTRAINT rig_identity_deleted_by_account_id_fkey;

DROP TABLE rig_account;
DROP TYPE rig_account_role_level;
DROP TYPE rig_account_kind;
DROP TABLE rig_identity_verification;
DROP TYPE rig_identity_verification_kind;
DROP TABLE rig_identity_credential;
DROP TABLE rig_identity;
DROP TABLE rig_tenant;

-- +goose StatementEnd
