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
    account_ids             uuid[],

    -- The lease, so two replicas do not both compute one audience. Two that did
    -- would be harmless — the recipient index absorbs it — but resolving an
    -- audience is a read of a membership table, and paying for it twice for
    -- nothing is not a trade worth making.
    claimed_at              timestamptz,
    claimed_by              uuid,
    attempts                integer NOT NULL DEFAULT 0
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
COMMENT ON COLUMN rig_notification.claimed_at IS 'When a dispatcher took this to resolve. Past notifications.claim_ttl another may, which is what makes a crashed process recoverable.';
COMMENT ON COLUMN rig_notification.claimed_by IS 'Which process holds the lease, so a stuck one traces to a pod rather than to a mystery.';
COMMENT ON COLUMN rig_notification.attempts IS 'How many times resolving this has been attempted.';
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

-- What a notification is copied to, and what a person can refuse.
--
-- The inbox is not one of these. It is the table above, it is always on, and a
-- switch that turned it off would produce a notification nobody can ever find:
-- the badge would be wrong, the count would be wrong, and the row would sit
-- there unread forever. Every channel here is a *copy* of an inbox line sent
-- somewhere else, which is why they can all be refused and it cannot.
--
-- Desktop and Mobile are separate rather than one push channel with a platform
-- column, and that is the whole reason to name them this way: they are
-- separately answerable. "Not on my phone during dinner, yes on my laptop while
-- I am working" is the setting people reach for, and a platform on a device row
-- cannot express it — the platform says where a token points, and the question
-- is what a person wants.
CREATE TYPE rig_notification_channel AS ENUM ('Desktop', 'Mobile', 'Email');

-- Immediate sends each delivery on its own. Anything else waits for a window to
-- close and then goes out as one message. Off means never send on this channel
-- and still write the inbox line — the person will see it when they look.
CREATE TYPE rig_notification_digest AS ENUM ('Immediate', 'Hourly', 'Daily', 'Weekly', 'Off');

CREATE TYPE rig_notification_delivery_state AS ENUM ('Pending', 'Sent', 'Failed', 'Skipped');

-- Where a push can reach somebody.
CREATE TABLE rig_notification_device (
    id                      uuid PRIMARY KEY,
    tenant_id               uuid NOT NULL REFERENCES rig_tenant (id),
    account_id              uuid NOT NULL REFERENCES rig_account (id),

    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz,

    channel                 rig_notification_channel NOT NULL,
    token                   text NOT NULL,
    label                   text,
    last_seen_at            timestamptz,
    revoked_at              timestamptz,

    -- Email is refused, because there is nothing to register: the address is on
    -- the account and the identity already, and a third copy of it is a third
    -- thing that can disagree.
    CONSTRAINT rig_notification_device_channel
        CHECK (channel IN ('Desktop', 'Mobile'))
);

-- Not partial: a partial index is not what the tenant-leading rule looks for,
-- and the foreign keys need covering whether or not the row is revoked.
CREATE INDEX rig_notification_device_account_idx
    ON rig_notification_device (tenant_id, account_id);
CREATE INDEX rig_notification_device_account_fk_idx ON rig_notification_device (account_id);

COMMENT ON TABLE  rig_notification_device IS 'Where a push can reach somebody. Email is refused: the address is on the account already.';
COMMENT ON COLUMN rig_notification_device.account_id IS 'Whose device it is.';
COMMENT ON COLUMN rig_notification_device.channel IS 'Desktop or Mobile. Email is refused by the CHECK above.';
COMMENT ON COLUMN rig_notification_device.token IS 'What the provider was given to address this device. Opaque to rig.';
COMMENT ON COLUMN rig_notification_device.label IS 'What a person sees in a list of their devices, and the column that makes revoking one possible for somebody who has four.';
COMMENT ON COLUMN rig_notification_device.last_seen_at IS 'When this device last checked in, so a stale one can be told apart from a revoked one.';
COMMENT ON COLUMN rig_notification_device.revoked_at IS 'When it stopped being a place to send to. Kept rather than deleted, so a token that comes back can be recognised.';

-- What somebody wants on a channel, and when.
--
-- Resolution is three steps and they are worth stating once, because a settings
-- system whose precedence is folklore is one nobody trusts: the row for this
-- kind and this channel, else the row for this channel with a null kind, else
-- the default in rig.yaml. The two indexes below keep each of the first two
-- steps single.
--
-- The window is stated as when to deliver, not when to stay quiet, and that is
-- not a naming preference. The setting people describe is "mobile, weekdays,
-- nine to five" — one row. As quiet hours it is two, because the quiet period
-- wraps a weekend and a night, and somebody who wants to change the end of their
-- working day has to reason about the complement.
CREATE TABLE rig_notification_setting (
    id                      uuid PRIMARY KEY,
    tenant_id               uuid NOT NULL REFERENCES rig_tenant (id),
    account_id              uuid NOT NULL REFERENCES rig_account (id),

    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz,

    kind                    text,
    channel                 rig_notification_channel NOT NULL,
    is_enabled              boolean NOT NULL DEFAULT true,
    digest                  rig_notification_digest NOT NULL DEFAULT 'Immediate',

    active_from             time,
    active_until            time,
    active_days             smallint[] NOT NULL DEFAULT '{}'
);

-- The first step of the resolution, and the second.
CREATE UNIQUE INDEX rig_notification_setting_kind_key
    ON rig_notification_setting (account_id, kind, channel) WHERE kind IS NOT NULL;
CREATE UNIQUE INDEX rig_notification_setting_default_key
    ON rig_notification_setting (account_id, channel) WHERE kind IS NULL;
CREATE INDEX rig_notification_setting_tenant_idx ON rig_notification_setting (tenant_id, account_id);
CREATE INDEX rig_notification_setting_account_fk_idx ON rig_notification_setting (account_id);

COMMENT ON TABLE  rig_notification_setting IS 'What somebody wants on a channel, and when. Resolved in three steps: this kind, then this channel, then the project default.';
COMMENT ON COLUMN rig_notification_setting.account_id IS 'Whose preference it is.';
COMMENT ON COLUMN rig_notification_setting.channel IS 'Which channel it answers for.';
COMMENT ON COLUMN rig_notification_setting.kind IS 'Null is the default for the channel, which is the second step of the resolution.';
COMMENT ON COLUMN rig_notification_setting.is_enabled IS 'False writes no delivery row at all. Not the same as digest Off, which is about preferring to look rather than be told.';
COMMENT ON COLUMN rig_notification_setting.digest IS 'Immediate sends each on its own; anything else waits for the window and goes out as one message.';
COMMENT ON COLUMN rig_notification_setting.active_from IS 'When to start delivering, in the account time zone. Null means all day. A window that wraps midnight is ordinary: 22:00 to 06:00 is how somebody says "not overnight".';
COMMENT ON COLUMN rig_notification_setting.active_until IS 'When to stop. Outside the window a delivery is held, not dropped: the inbox line exists either way, and a discarded copy makes the badge and the mailbox disagree.';
COMMENT ON COLUMN rig_notification_setting.active_days IS 'ISO weekdays, 1 being Monday. Empty means every day. An array rather than a bitmask because [1,2,3,4,5] is legible in a settings payload and 62 is not.';

-- One copy of an inbox line, on its way somewhere.
--
-- The claim is a lease rather than a lock, and that is the whole reason this
-- table has four columns it would not otherwise need. SELECT ... FOR UPDATE
-- SKIP LOCKED inside the transaction that sends is correct and unusable: a row
-- lock lives as long as its transaction, so it would be held across a call to
-- SMTP or APNs — and one slow provider then holds a pool connection per message
-- in flight. So a send is three short transactions, and the middle one has no
-- transaction open at all.
CREATE TABLE rig_notification_delivery (
    id                      uuid PRIMARY KEY,
    tenant_id               uuid NOT NULL REFERENCES rig_tenant (id),
    recipient_id            uuid NOT NULL REFERENCES rig_notification_recipient (id),
    account_id              uuid NOT NULL REFERENCES rig_account (id),

    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz,

    channel                 rig_notification_channel NOT NULL,
    kind                    text NOT NULL,
    -- Copied from the setting that decided it, so a claim knows whether this
    -- row is one message or part of one without joining back. Immediate rows
    -- are sent on their own even when several come due at the same instant:
    -- "you asked to be told as things happen" and "you asked for a summary" are
    -- different requests, and grouping by account alone would answer them the
    -- same way.
    digest                  rig_notification_digest NOT NULL DEFAULT 'Immediate',
    state                   rig_notification_delivery_state NOT NULL DEFAULT 'Pending',
    deliver_at              timestamptz NOT NULL DEFAULT now(),
    sent_at                 timestamptz,
    failed_reason           text,

    attempts                integer NOT NULL DEFAULT 0,
    claimed_at              timestamptz,
    claimed_by              uuid
);

-- The claim query, and nothing else needs an index here.
CREATE INDEX rig_notification_delivery_due_idx
    ON rig_notification_delivery (deliver_at) WHERE state = 'Pending';
CREATE INDEX rig_notification_delivery_recipient_idx
    ON rig_notification_delivery (recipient_id);
CREATE INDEX rig_notification_delivery_account_idx
    ON rig_notification_delivery (tenant_id, account_id);
CREATE INDEX rig_notification_delivery_account_fk_idx ON rig_notification_delivery (account_id);

-- One delivery per inbox line per channel. What makes writing them idempotent,
-- the way the recipient index makes the fan-out idempotent.
CREATE UNIQUE INDEX rig_notification_delivery_key
    ON rig_notification_delivery (recipient_id, channel);

COMMENT ON TABLE  rig_notification_delivery IS 'One copy of an inbox line on its way to a channel. Claimed by lease, sent outside any transaction, and marked afterwards.';
COMMENT ON COLUMN rig_notification_delivery.recipient_id IS 'The inbox line this is a copy of. The line is the truth and this is one way it was repeated.';
COMMENT ON COLUMN rig_notification_delivery.account_id IS 'Who it is for, denormalized off the line so a claim can group by it without a join.';
COMMENT ON COLUMN rig_notification_delivery.channel IS 'Where it is going. One row per line per channel, which is what the unique index below enforces.';
COMMENT ON COLUMN rig_notification_delivery.kind IS 'Copied from the notification, so the settings resolution needs no join either.';
COMMENT ON COLUMN rig_notification_delivery.digest IS 'What the setting said when this row was written. Immediate rows are sent on their own; the rest are batched per account and channel.';
COMMENT ON COLUMN rig_notification_delivery.sent_at IS 'When a channel accepted it, which is not the same as it arriving.';
COMMENT ON COLUMN rig_notification_delivery.failed_reason IS 'What the channel said last time. Kept on a retry as well as on a failure, so a pattern is visible before the cap is reached.';
COMMENT ON COLUMN rig_notification_delivery.deliver_at IS 'When this is due. A delivery held outside somebody hours moves to the next opening; a digested one moves to its window close.';
COMMENT ON COLUMN rig_notification_delivery.state IS 'Skipped means a setting refused it, which is different from Failed and worth telling apart in a report.';
COMMENT ON COLUMN rig_notification_delivery.attempts IS 'How many times this has been claimed. Past notifications.max_attempts it is Failed and stops being claimed.';
COMMENT ON COLUMN rig_notification_delivery.claimed_at IS 'When a dispatcher took it. Past notifications.claim_ttl another one may, which is what makes a crashed process recoverable.';
COMMENT ON COLUMN rig_notification_delivery.claimed_by IS 'Which process holds the lease. A uuid generated once per process, with the hostname beside it in the log line, so a stuck lease traces to a pod rather than to a mystery.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE rig_notification_delivery;
DROP TABLE rig_notification_setting;
DROP TABLE rig_notification_device;
DROP TABLE rig_notification_recipient;
DROP TABLE rig_notification;
DROP TYPE rig_notification_delivery_state;
DROP TYPE rig_notification_digest;
DROP TYPE rig_notification_channel;
DROP TYPE rig_notification_state;

-- +goose StatementEnd
