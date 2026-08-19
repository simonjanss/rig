-- +goose Up
-- +goose StatementBegin

-- What happened, and when it is due. One row per thing worth telling somebody
-- about; the tables it is about point at it through a join table of their own.
--
-- It carries no recipients, and that is the decision the rest of this schema
-- follows from. A post scheduled for Friday notifies whoever can read it on
-- Friday, and somebody added to the group on Thursday evening is one of those
-- people — a list computed when the row was written does not know that, and
-- every system that captures one spends the rest of its life patching around
-- it. The audience is computed at the moment of sending, by asking the table
-- the notification is about.
--
-- There is no title and no body anywhere here. Those are rendering: they are
-- locale-dependent, they belong to a template, and a copy of them in the row is
-- a copy that goes stale the day somebody rewords it. What rig knows is that
-- something happened and what it happened to; kind, payload and the linked
-- subject are everything a template needs.
CREATE TYPE rig_notification_state AS ENUM ('Pending', 'Resolved', 'Cancelled');

CREATE TABLE rig_notification (
    id                      uuid PRIMARY KEY,
    tenant_id               uuid NOT NULL REFERENCES rig_tenant (id),

    created_at              timestamptz NOT NULL DEFAULT now(),
    created_by_account_id   uuid,
    created_by_api_key_id   uuid,
    updated_at              timestamptz,

    kind                    text NOT NULL,
    state                   rig_notification_state NOT NULL DEFAULT 'Pending',
    deliver_at              timestamptz NOT NULL DEFAULT now(),
    resolved_at             timestamptz,
    payload                 jsonb NOT NULL DEFAULT '{}',
    group_key               text,
    account_ids             uuid[]
);

-- What lets a link table put the tenant inside its own foreign key:
--     foreign key (tenant_id, notification_id) references rig_notification (tenant_id, id)
-- The same reason rig_file_tenant_id_key exists, and not optional for the same
-- one: with it, linking another tenant's notification is a constraint violation
-- rather than something every hook has to remember.
CREATE UNIQUE INDEX rig_notification_tenant_id_key ON rig_notification (tenant_id, id);

-- The dispatcher's one query.
CREATE INDEX rig_notification_due_idx ON rig_notification (deliver_at)
    WHERE state = 'Pending';

COMMENT ON TABLE  rig_notification IS 'Something worth telling somebody about, and when it is due. Carries no recipients: the audience is computed when it is sent.';
COMMENT ON COLUMN rig_notification.kind IS 'What happened, as the application names it. Narrow it to an enum of your own to get a switch the compiler can see.';
COMMENT ON COLUMN rig_notification.state IS 'Resolved means the audience was determined and the inbox lines exist. It does not mean anything was sent.';
COMMENT ON COLUMN rig_notification.deliver_at IS 'When this is due. now() is the ordinary case; a scheduled notification is the same row with a later time, which is the only difference between the two.';
COMMENT ON COLUMN rig_notification.resolved_at IS 'When the audience was computed. Null while the notification is still pending or was cancelled.';
COMMENT ON COLUMN rig_notification.payload IS 'What a template needs beyond the linked row. Give it a Go type with the go_type key if it has a shape.';
COMMENT ON COLUMN rig_notification.group_key IS 'What collapses several of these into one inbox line, decided when the announcement was written and copied onto the line when the audience is resolved.';
COMMENT ON COLUMN rig_notification.account_ids IS 'A recipient list captured at write time, and the one exception to computing the audience late. Null is the ordinary case; a list here skips the question entirely, for an audience that genuinely cannot be re-derived.';

-- The inbox line: one row per notification per account, written when the
-- audience was resolved rather than when the notification was written.
--
-- It exists separately from the notification for two reasons that are both
-- structural. One person clearing their inbox must not change what anybody else
-- sees, so the delete is here. And ten comments on one post are one line saying
-- ten, which is the group index below.
--
-- A notification is addressed to an account, not to an identity. An identity has
-- no tenant, so an identity-addressed row would fall outside every generated
-- query's filter, and tenancy.Claims carries no identity id for a handler to
-- narrow by. The consequence worth knowing: a service account has an inbox and
-- no mailbox.
CREATE TABLE rig_notification_recipient (
    id                      uuid PRIMARY KEY,
    tenant_id               uuid NOT NULL REFERENCES rig_tenant (id),
    notification_id         uuid NOT NULL REFERENCES rig_notification (id),
    account_id              uuid NOT NULL REFERENCES rig_account (id),

    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz,
    deleted_at              timestamptz,
    deleted_by_account_id   uuid,

    kind                    text NOT NULL,
    group_key               text,
    event_count             integer NOT NULL DEFAULT 1,
    read_at                 timestamptz
);

-- Fan-out is idempotent by construction rather than by care. The audience may be
-- computed more than once for one notification — a dispatcher that resolved and
-- died before committing, two replicas racing the same nudge — so a repeat is
-- ON CONFLICT DO NOTHING. This index is the whole reason the method that
-- computes an audience can be documented as "a pure read that may be called
-- again" rather than "a read that had better only run once".
CREATE UNIQUE INDEX rig_notification_recipient_key
    ON rig_notification_recipient (notification_id, account_id);

-- What turns ten comments into one line. Ten announcements about one post upsert
-- into one row with event_count = 10; read the row and the next comment starts a
-- fresh one, which falls out of the predicate rather than being coded, so there
-- is no rule to get wrong about when a group ends.
CREATE UNIQUE INDEX rig_notification_recipient_group_key
    ON rig_notification_recipient (account_id, kind, group_key)
    WHERE group_key IS NOT NULL AND read_at IS NULL AND deleted_at IS NULL;

-- The inbox read, newest first, and the badge. Not partial: a partial index is
-- not what the tenant-leading rule is looking for, and every generated query
-- filters by tenant whether or not the row is deleted.
CREATE INDEX rig_notification_recipient_inbox_idx
    ON rig_notification_recipient (tenant_id, account_id, created_at DESC);

CREATE INDEX rig_notification_recipient_notification_idx
    ON rig_notification_recipient (notification_id);

-- The account foreign key, covered on its own. The inbox index above leads with
-- the tenant, which is right for the read and does not cover this.
CREATE INDEX rig_notification_recipient_account_idx
    ON rig_notification_recipient (account_id);

COMMENT ON TABLE  rig_notification_recipient IS 'One inbox line: a notification, an account, and whether it has been read.';
COMMENT ON COLUMN rig_notification_recipient.notification_id IS 'What happened. The line is separate from it so that one person clearing their inbox changes nothing anybody else sees.';
COMMENT ON COLUMN rig_notification_recipient.account_id IS 'Who this line is for. An account rather than an identity: an identity has no tenant, so a row addressed to one would fall outside every generated query.';
COMMENT ON COLUMN rig_notification_recipient.kind IS 'Copied from the notification, so the inbox and its live-sync shape never touch a table holding rows for people who are not recipients.';
COMMENT ON COLUMN rig_notification_recipient.group_key IS 'What collapses several events into one line. Null opts out and every event is its own row.';
COMMENT ON COLUMN rig_notification_recipient.event_count IS 'How many events this line stands for. One unless a group key collapsed them.';
COMMENT ON COLUMN rig_notification_recipient.read_at IS 'When the person read it. Null is unread, which is what the badge counts.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE rig_notification_recipient;
DROP TABLE rig_notification;
DROP TYPE rig_notification_state;

-- +goose StatementEnd
