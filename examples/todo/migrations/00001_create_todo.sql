-- +goose Up
-- +goose StatementBegin

CREATE TYPE todo_priority AS ENUM ('low', 'normal', 'high');

CREATE TABLE todo (
    id                      uuid PRIMARY KEY,
    tenant_id               uuid NOT NULL,

    title                   text NOT NULL,
    notes                   text,
    is_done                 boolean NOT NULL DEFAULT false,
    priority                todo_priority NOT NULL DEFAULT 'normal',
    due_at                  timestamptz,

    created_at              timestamptz NOT NULL DEFAULT now(),
    created_by_account_id   uuid,
    updated_at              timestamptz,
    updated_by_account_id   uuid,
    deleted_at              timestamptz,
    deleted_by_account_id   uuid
);

COMMENT ON TABLE todo IS 'One thing somebody means to do.';
COMMENT ON COLUMN todo.title IS 'What needs doing, in a few words.';
COMMENT ON COLUMN todo.notes IS 'Anything worth remembering that does not fit in the title.';
COMMENT ON COLUMN todo.is_done IS 'Whether the task has been completed.';
COMMENT ON COLUMN todo.priority IS 'How urgently the task wants attention.';
COMMENT ON COLUMN todo.due_at IS 'When the task is due, or null if it never expires.';

CREATE INDEX todo_tenant_created_idx ON todo (tenant_id, created_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE todo;
DROP TYPE todo_priority;

-- +goose StatementEnd
