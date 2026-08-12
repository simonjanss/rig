-- +goose Up
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
