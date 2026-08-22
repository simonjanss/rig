-- +goose Up
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
CREATE TYPE rig_api_key_kind AS ENUM ('Integration', 'Personal');

CREATE TABLE rig_api_key (
    id                      uuid PRIMARY KEY,
    tenant_id               uuid NOT NULL REFERENCES rig_tenant (id),
    account_id              uuid NOT NULL REFERENCES rig_account (id),

    created_at              timestamptz NOT NULL DEFAULT now(),
    created_by_account_id   uuid REFERENCES rig_account (id),

    kind                    rig_api_key_kind NOT NULL DEFAULT 'Integration',
    -- Self-referencing, and for the same reason as everywhere else: an
    -- integration that provisions keys should leave a trail of which of its own
    -- credentials did it.
    created_by_api_key_id   uuid REFERENCES rig_api_key (id),
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
ALTER TABLE rig_api_key
    ADD CONSTRAINT rig_api_key_personal_is_its_own
        CHECK (kind <> 'Personal' OR account_id = created_by_account_id);

CREATE UNIQUE INDEX rig_api_key_key_id_key ON rig_api_key (key_id);
CREATE INDEX rig_api_key_tenant_created_idx ON rig_api_key (tenant_id, created_at DESC);
CREATE INDEX rig_api_key_account_id_idx ON rig_api_key (account_id);
CREATE INDEX rig_api_key_created_by_account_id_idx ON rig_api_key (created_by_account_id);
CREATE INDEX rig_api_key_created_by_api_key_id_idx ON rig_api_key (created_by_api_key_id);

COMMENT ON TABLE  rig_api_key IS 'A machine-to-machine credential.';
COMMENT ON COLUMN rig_api_key.kind IS 'Whether the key belongs to an integration, with a service account of its own, or to a person automating their own work.';
COMMENT ON COLUMN rig_api_key.account_id IS 'The service account the key acts as, so a machine''s writes are attributable to something.';
COMMENT ON COLUMN rig_api_key.created_by_account_id IS 'Who minted it. This is the audit trail either way — for an integration it is the person who set it up.';
COMMENT ON COLUMN rig_api_key.name IS 'What the key is for. A list of unnamed keys is a list nobody can safely revoke from.';
COMMENT ON COLUMN rig_api_key.key_id IS 'The public half. It appears in the presented value, in logs, and in rate limits.';
COMMENT ON COLUMN rig_api_key.secret_hash IS 'sha256 of the secret half. The secret is shown once and never stored.';
COMMENT ON COLUMN rig_api_key.scopes IS 'Permission keys, the same vocabulary roles use, so a key can never exceed what a role could express.';
COMMENT ON COLUMN rig_api_key.cidr_allow_list IS 'Networks the key may be used from. Empty means anywhere.';
COMMENT ON COLUMN rig_api_key.expires_at IS 'When the key stops working, or null for no expiry.';
COMMENT ON COLUMN rig_api_key.last_used_at IS 'Roughly when the key was last used. Written sparingly, so key authentication stays one read.';
COMMENT ON COLUMN rig_api_key.revoked_at IS 'When the key was killed.';

-- An account and an identity can now say which key changed them, which is the
-- rule everywhere else: a table that records who made a change records the
-- credential too, or a write from an integration is the one write nobody can
-- attribute. It is an ALTER rather than three columns in the CREATE TABLE above
-- because rig_api_key references rig_account and this references rig_api_key — somebody has
-- to be second.
ALTER TABLE rig_account
    ADD COLUMN created_by_api_key_id uuid REFERENCES rig_api_key (id),
    ADD COLUMN updated_by_api_key_id uuid REFERENCES rig_api_key (id),
    ADD COLUMN deleted_by_api_key_id uuid REFERENCES rig_api_key (id);

CREATE INDEX rig_account_created_by_api_key_id_idx ON rig_account (created_by_api_key_id);
CREATE INDEX rig_account_updated_by_api_key_id_idx ON rig_account (updated_by_api_key_id);
CREATE INDEX rig_account_deleted_by_api_key_id_idx ON rig_account (deleted_by_api_key_id);

ALTER TABLE rig_identity
    ADD COLUMN created_by_api_key_id uuid REFERENCES rig_api_key (id),
    ADD COLUMN updated_by_api_key_id uuid REFERENCES rig_api_key (id),
    ADD COLUMN deleted_by_api_key_id uuid REFERENCES rig_api_key (id);

CREATE INDEX rig_identity_created_by_api_key_id_idx ON rig_identity (created_by_api_key_id);
CREATE INDEX rig_identity_updated_by_api_key_id_idx ON rig_identity (updated_by_api_key_id);
CREATE INDEX rig_identity_deleted_by_api_key_id_idx ON rig_identity (deleted_by_api_key_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE rig_identity
    DROP COLUMN deleted_by_api_key_id,
    DROP COLUMN updated_by_api_key_id,
    DROP COLUMN created_by_api_key_id;
ALTER TABLE rig_account
    DROP COLUMN deleted_by_api_key_id,
    DROP COLUMN updated_by_api_key_id,
    DROP COLUMN created_by_api_key_id;
DROP TABLE rig_api_key;
DROP TYPE rig_api_key_kind;

-- +goose StatementEnd
