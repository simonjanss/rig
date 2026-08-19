-- +goose Up
-- +goose StatementBegin

-- What makes a todo notifiable, and it is the whole declaration: a join table
-- with rig_notification on one side. rig finds it by scanning link tables
-- rather than by parsing names, so `todo_notification` is a recommendation and
-- nothing reads it.
--
-- This example has no authentication — claims come from headers — and the inbox
-- works anyway, which is the point of doing it here as well as in examples/auth.
-- What a notification needs is an account to be addressed to, not a sign-in
-- endpoint: rig_account came with the tenancy migration and nothing else did.
--
-- todo already has UNIQUE (tenant_id, id) — the file columns needed it first,
-- for the same reason — so the composite foreign key below has something to
-- point at without adding one.
CREATE TABLE todo_notification (
    tenant_id       uuid NOT NULL REFERENCES rig_tenant (id),
    todo_id         uuid NOT NULL,
    notification_id uuid NOT NULL,

    PRIMARY KEY (todo_id, notification_id),
    FOREIGN KEY (tenant_id, todo_id)         REFERENCES todo (tenant_id, id),
    FOREIGN KEY (tenant_id, notification_id) REFERENCES rig_notification (tenant_id, id)
);

COMMENT ON TABLE todo_notification IS 'What makes a todo notifiable: the join, and nothing else.';
COMMENT ON COLUMN todo_notification.tenant_id IS 'Inside both foreign keys, so a cross-tenant link is a constraint violation rather than a rule somebody has to remember.';
COMMENT ON COLUMN todo_notification.todo_id IS 'The task a notification is about.';
COMMENT ON COLUMN todo_notification.notification_id IS 'The notification.';

CREATE INDEX todo_notification_notification_idx ON todo_notification (tenant_id, notification_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE todo_notification;

-- +goose StatementEnd
