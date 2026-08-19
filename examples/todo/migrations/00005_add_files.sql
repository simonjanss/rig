-- +goose Up
-- +goose StatementBegin

-- Both forms a file takes in rig, in one migration, because they are the same
-- convention and the difference between them is only whether one row can have
-- more than one file.
--
-- The column is the declaration: `<role>_file_id` referencing rig_file is what
-- makes the endpoints, the permission keys and the client methods appear.
-- Nothing in a configuration file says any of it, and nothing can disagree
-- with it.

-- What a composite key pointing at todo needs on the other side, and what
-- todo_attachment below uses.
CREATE UNIQUE INDEX todo_tenant_id_key ON todo (tenant_id, id);

-- One file on the todo itself. Nullable, because a todo does not need a picture
-- and one that has none should not be a row that cannot exist.
--
-- The key carries the tenant. `references rig_file (id)` alone would prove the
-- file exists and say nothing about whose it is, and attaching another tenant's
-- file would then be something a hook had to remember. This makes it a
-- constraint violation.
ALTER TABLE todo ADD COLUMN cover_file_id uuid;

ALTER TABLE todo ADD CONSTRAINT todo_cover_file_fk
    FOREIGN KEY (tenant_id, cover_file_id) REFERENCES rig_file (tenant_id, id);

CREATE INDEX todo_cover_file_idx ON todo (tenant_id, cover_file_id);

COMMENT ON COLUMN todo.cover_file_id IS 'A picture for the todo, if it has one.';

-- And the many-file form, which is an ordinary table rather than a second
-- mechanism.
--
-- A gallery, an attachment list, a set of receipts — rig's answer to all of
-- them is the answer it gives for every other one-to-many: write the table. It
-- gets everything an ordinary rig table gets, including its own file endpoints,
-- and it gets captions and ordering and soft delete for free because those are
-- just columns.
CREATE TABLE todo_attachment (
    id                      uuid PRIMARY KEY,
    tenant_id               uuid NOT NULL,

    todo_id                 uuid NOT NULL,
    -- Not null, which is the whole reason the create endpoint also accepts a
    -- form: the row and its bytes have to be committed together, or this column
    -- could never be written at all.
    attachment_file_id      uuid NOT NULL,
    caption                 text,
    position                integer NOT NULL DEFAULT 0,

    created_at              timestamptz NOT NULL DEFAULT now(),
    created_by_account_id   uuid,
    updated_at              timestamptz,
    updated_by_account_id   uuid,
    deleted_at              timestamptz,
    deleted_by_account_id   uuid,

    FOREIGN KEY (tenant_id, todo_id) REFERENCES todo (tenant_id, id),
    FOREIGN KEY (tenant_id, attachment_file_id) REFERENCES rig_file (tenant_id, id)
);

COMMENT ON TABLE todo_attachment IS 'A file attached to a todo, with a caption and a place in the order.';
COMMENT ON COLUMN todo_attachment.todo_id IS 'The todo this is attached to.';
COMMENT ON COLUMN todo_attachment.attachment_file_id IS 'The file itself. Required: an attachment with no file is nothing.';
COMMENT ON COLUMN todo_attachment.caption IS 'What the attachment is, in a few words.';
COMMENT ON COLUMN todo_attachment.position IS 'Where this sits in the todo''s list of attachments.';

CREATE INDEX todo_attachment_tenant_todo_idx ON todo_attachment (tenant_id, todo_id, position);
CREATE INDEX todo_attachment_tenant_file_idx ON todo_attachment (tenant_id, attachment_file_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE todo_attachment;

ALTER TABLE todo DROP CONSTRAINT todo_cover_file_fk;
ALTER TABLE todo DROP COLUMN cover_file_id;
DROP INDEX todo_tenant_id_key;

-- +goose StatementEnd
