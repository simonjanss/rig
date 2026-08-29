-- +goose Up
-- +goose StatementBegin

-- What makes a note notifiable, and it is the whole declaration: a join table
-- with rig_notification on one side. rig finds it by scanning link tables
-- rather than by parsing names, so `note_notification` is what the
-- documentation recommends and nothing reads it.
--
-- The tenant is inside both foreign keys, which is the form rig recommends and
-- the reason it recommends it: linking another tenant's note to this tenant's
-- notification is a constraint violation rather than something a hook has to
-- remember. That needs UNIQUE (tenant_id, id) on both sides — rig_notification
-- ships with one, and note gains one below.
--
-- The link points at the notification and not at an inbox line, which is what
-- keeps it small: an announcement to a group of two thousand adds one row here.
CREATE UNIQUE INDEX note_tenant_id_key ON note (tenant_id, id);

CREATE TABLE note_notification (
    tenant_id       uuid NOT NULL REFERENCES rig_tenant (id),
    note_id         uuid NOT NULL,
    notification_id uuid NOT NULL,

    PRIMARY KEY (note_id, notification_id),
    FOREIGN KEY (tenant_id, note_id)         REFERENCES note (tenant_id, id),
    FOREIGN KEY (tenant_id, notification_id) REFERENCES rig_notification (tenant_id, id)
);

COMMENT ON TABLE note_notification IS 'What makes a note notifiable: the join, and nothing else.';
COMMENT ON COLUMN note_notification.tenant_id IS 'Inside both foreign keys, so a cross-tenant link is a constraint violation rather than a rule somebody has to remember.';
COMMENT ON COLUMN note_notification.note_id IS 'The note a notification is about.';
COMMENT ON COLUMN note_notification.notification_id IS 'The notification.';

CREATE INDEX note_notification_notification_idx ON note_notification (tenant_id, notification_id);

-- When a note goes live, which is when the people who can read it are told.
-- Null is a draft: NotifyAt returns false for one, so an announcement about it
-- is written, linked, and cancelled — and clearing this column later cancels
-- what was still pending, because rig asks again after every update.
ALTER TABLE note ADD COLUMN publish_at timestamptz;

COMMENT ON COLUMN note.publish_at IS 'When the note goes live, or null while it is a draft. Notifications about it are due then.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE note DROP COLUMN publish_at;
DROP TABLE note_notification;
DROP INDEX note_tenant_id_key;

-- +goose StatementEnd
