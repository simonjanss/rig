-- +goose Up
-- +goose StatementBegin

-- The board's one real table. Everything the example demonstrates hangs off
-- it: the status column is the kanban, the assignee is who a change notifies,
-- and the audit columns arriving in the next migration's snapshot triple are
-- the history panel.
CREATE TYPE todo_status AS ENUM ('backlog', 'todo', 'in_progress', 'done', 'canceled');
CREATE TYPE todo_priority AS ENUM ('low', 'normal', 'high');

CREATE TABLE todo (
    id                      uuid PRIMARY KEY,
    tenant_id               uuid NOT NULL,

    title                   text NOT NULL,
    description             text,
    status                  todo_status NOT NULL DEFAULT 'todo',
    priority                todo_priority NOT NULL DEFAULT 'normal',
    -- The account rather than the identity, because an assignment is a fact
    -- inside one tenant. Plain `references rig_account (id)`: the foundation
    -- table has no UNIQUE (tenant_id, id) to hang a composite key on, and the
    -- service layer only ever writes the caller's own account here.
    assignee_account_id     uuid REFERENCES rig_account (id),

    created_at              timestamptz NOT NULL DEFAULT now(),
    created_by_account_id   uuid,
    updated_at              timestamptz,
    updated_by_account_id   uuid,
    deleted_at              timestamptz,
    deleted_by_account_id   uuid
);

COMMENT ON TABLE todo IS 'One item on the board.';
COMMENT ON COLUMN todo.title IS 'What needs doing, in a few words.';
COMMENT ON COLUMN todo.description IS 'The longer story, or null while the title says it all.';
COMMENT ON COLUMN todo.status IS 'Which column of the board the item is in.';
COMMENT ON COLUMN todo.priority IS 'How urgently the item wants attention.';
COMMENT ON COLUMN todo.assignee_account_id IS 'Who has taken the item, or null while nobody has.';

-- What the composite keys in the later migrations point at: the attachments
-- and the notification link both carry the tenant inside their foreign keys,
-- so a cross-tenant link is a constraint violation rather than a rule somebody
-- has to remember.
CREATE UNIQUE INDEX todo_tenant_id_key ON todo (tenant_id, id);

CREATE INDEX todo_tenant_created_idx ON todo (tenant_id, created_at DESC);
CREATE INDEX todo_tenant_status_idx ON todo (tenant_id, status);
CREATE INDEX todo_assignee_idx ON todo (assignee_account_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE todo;
DROP TYPE todo_priority;
DROP TYPE todo_status;

-- +goose StatementEnd
