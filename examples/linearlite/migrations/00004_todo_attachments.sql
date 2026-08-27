-- +goose Up
-- +goose StatementBegin

-- Attachments, in the many-file form: an ordinary table whose
-- `attachment_file_id` column referencing rig_file is the whole declaration.
-- The endpoints, the permission keys and the client methods all follow from
-- the column; examples/todo shows the one-file form beside this one.
CREATE TABLE todo_attachment (
    id                      uuid PRIMARY KEY,
    tenant_id               uuid NOT NULL,

    todo_id                 uuid NOT NULL,
    -- Not null, which is why the create endpoint also accepts a multipart
    -- form: the row and its bytes have to be committed together, or this
    -- column could never be written at all.
    attachment_file_id      uuid NOT NULL,
    caption                 text,

    created_at              timestamptz NOT NULL DEFAULT now(),
    created_by_account_id   uuid,
    updated_at              timestamptz,
    updated_by_account_id   uuid,
    deleted_at              timestamptz,
    deleted_by_account_id   uuid,

    -- The tenant travels inside both keys, so attaching another tenant's file
    -- — or attaching to another tenant's todo — is a constraint violation
    -- rather than a rule a hook has to remember.
    FOREIGN KEY (tenant_id, todo_id) REFERENCES todo (tenant_id, id),
    FOREIGN KEY (tenant_id, attachment_file_id) REFERENCES rig_file (tenant_id, id)
);

COMMENT ON TABLE todo_attachment IS 'A file attached to a todo.';
COMMENT ON COLUMN todo_attachment.todo_id IS 'The todo this is attached to.';
COMMENT ON COLUMN todo_attachment.attachment_file_id IS 'The file itself. Required: an attachment with no file is nothing.';
COMMENT ON COLUMN todo_attachment.caption IS 'What the attachment is, in a few words.';

CREATE INDEX todo_attachment_tenant_todo_idx ON todo_attachment (tenant_id, todo_id);
CREATE INDEX todo_attachment_tenant_file_idx ON todo_attachment (tenant_id, attachment_file_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE todo_attachment;

-- +goose StatementEnd
