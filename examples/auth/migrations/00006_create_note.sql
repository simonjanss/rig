-- +goose Up
-- +goose StatementBegin

-- One ordinary table, so that there is something to protect. Everything before
-- this migration is the authentication foundation `rig setup-project` wrote;
-- this is the application.
CREATE TABLE note (
    id                      uuid PRIMARY KEY,
    tenant_id               uuid NOT NULL REFERENCES rig_tenant (id),

    title                   text NOT NULL,
    body                    text,

    created_at              timestamptz NOT NULL DEFAULT now(),
    created_by_account_id   uuid REFERENCES rig_account (id),
    updated_at              timestamptz,
    updated_by_account_id   uuid REFERENCES rig_account (id),
    deleted_at              timestamptz,
    deleted_by_account_id   uuid REFERENCES rig_account (id),

    -- Beside the account columns, not instead of them. The account says whose
    -- change it was — a service account when an integration did it — and these
    -- say which credential it came through, which is the one you revoke.
    created_by_api_key_id   uuid REFERENCES rig_api_key (id),
    updated_by_api_key_id   uuid REFERENCES rig_api_key (id),
    deleted_by_api_key_id   uuid REFERENCES rig_api_key (id)
);

COMMENT ON TABLE note IS 'Something somebody wrote down.';
COMMENT ON COLUMN note.title IS 'What the note is about, in a few words.';
COMMENT ON COLUMN note.body IS 'The note itself, or null while it is just a title.';

CREATE INDEX note_tenant_created_idx ON note (tenant_id, created_at DESC);
CREATE INDEX note_created_by_idx ON note (created_by_account_id);
CREATE INDEX note_updated_by_idx ON note (updated_by_account_id);
CREATE INDEX note_deleted_by_idx ON note (deleted_by_account_id);
CREATE INDEX note_created_by_key_idx ON note (created_by_api_key_id);
CREATE INDEX note_updated_by_key_idx ON note (updated_by_api_key_id);
CREATE INDEX note_deleted_by_key_idx ON note (deleted_by_api_key_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE note;

-- +goose StatementEnd
