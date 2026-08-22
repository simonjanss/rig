-- +goose Up
-- +goose StatementBegin

-- One uploaded file. Metadata only: the bytes live in a blob store, and the key
-- that finds them there never leaves the server.
--
-- The row is written before the upload and finalized after it, because the row
-- and the bytes cannot be committed together and one of them has to lead. A row
-- with a null uploaded_at is invisible to every read and reapable with one
-- query; bytes with no row would need a bucket scan to find. That is the whole
-- reason the order is this way round and not the other.
--
-- tenant_id has no foreign key, and it is the one place this foundation departs
-- from the others. Files are useful in a project with no authentication at all —
-- rig_tenant may simply not exist — and UNIQUE (tenant_id, id) below is what the
-- referencing tables actually need. A project with the tenancy part can add the
-- reference in a migration of its own.
CREATE TABLE rig_file (
    id                      uuid PRIMARY KEY,
    tenant_id               uuid NOT NULL,

    created_at              timestamptz NOT NULL DEFAULT now(),
    created_by_account_id   uuid,
    created_by_api_key_id   uuid,
    deleted_at              timestamptz,
    deleted_by_account_id   uuid,

    storage_key             text NOT NULL,
    url                     text,
    file_name               text NOT NULL,
    content_type            text NOT NULL,
    declared_content_type   text,
    size_bytes              bigint NOT NULL DEFAULT 0,
    checksum                text,
    uploaded_at             timestamptz
);

-- What lets a referencing table put the tenant inside its own foreign key:
--     foreign key (tenant_id, cover_file_id) references rig_file (tenant_id, id)
-- With it, attaching another tenant's file is a constraint violation rather than
-- something every hook has to remember. It is not optional for that reason.
CREATE UNIQUE INDEX rig_file_tenant_id_key ON rig_file (tenant_id, id);

-- The sweeper's two queries, and nothing else needs an index here: a file is
-- always reached through the row that owns it.
CREATE INDEX rig_file_pending_idx ON rig_file (created_at) WHERE uploaded_at IS NULL;
CREATE INDEX rig_file_trash_idx ON rig_file (deleted_at) WHERE deleted_at IS NOT NULL;

CREATE UNIQUE INDEX rig_file_storage_key_key ON rig_file (storage_key);

COMMENT ON TABLE  rig_file IS 'One uploaded file. Metadata only; the bytes live in the blob store.';
COMMENT ON COLUMN rig_file.storage_key IS 'Where the bytes are. Derived from the identifier and never from a supplied name, so a filename can never become a path.';
COMMENT ON COLUMN rig_file.url IS 'Where the file is served from. Stable and unsigned, so it is safe to sync and grants nothing on its own; the endpoint behind it still authorizes.';
COMMENT ON COLUMN rig_file.file_name IS 'What the file was called when it arrived. Used for the save dialog and compared against the download path, never for the storage key.';
COMMENT ON COLUMN rig_file.content_type IS 'What the bytes were sniffed to be. This is what a download announces.';
COMMENT ON COLUMN rig_file.declared_content_type IS 'What the client said the bytes were, kept beside the sniffed type rather than instead of it.';
COMMENT ON COLUMN rig_file.size_bytes IS 'How large the object is, known only once the upload finished.';
COMMENT ON COLUMN rig_file.checksum IS 'Hex sha256 of the content, computed as the bytes went past. Null until the upload is finalized.';
COMMENT ON COLUMN rig_file.uploaded_at IS 'When the bytes landed. Null means the row exists and the upload does not, which is what makes an abandoned upload reapable.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE rig_file;

-- +goose StatementEnd
