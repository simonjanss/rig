-- +goose Up
-- +goose StatementBegin

-- Who is here, and what they are looking at. One row per browser tab, rewritten
-- every heartbeat and deleted once it goes stale.
--
-- It is the shortest-lived table rig has, and most of what follows is that one
-- fact restated.
--
-- There is no deleted_at, because there is nothing to restore: a presence that
-- ended is not in the trash, it is over. A soft delete would also be the wrong
-- mechanism as well as a useless one — the sync service re-evaluates a shape's
-- filter only when a row *changes*, so a departure has to be a row that goes,
-- not a row that stops matching a predicate.
--
-- There are no snapshot columns, because a durable record of who looked at which
-- field, kept forever, is a surveillance log nobody asked for — on the one table
-- here that is written several times a second.
--
-- And there is no UNIQUE (tenant_id, id). The other foundation tables carry one
-- so that a project's own table can put the tenant inside a foreign key pointing
-- at them, and nothing will ever point at a row that lives twenty seconds.
CREATE TYPE rig_presence_activity AS ENUM ('viewing', 'editing');

CREATE TABLE rig_presence (
    id              uuid PRIMARY KEY,
    tenant_id       uuid NOT NULL REFERENCES rig_tenant (id),
    account_id      uuid NOT NULL REFERENCES rig_account (id),
    session_key     text NOT NULL,

    created_at      timestamptz NOT NULL DEFAULT now(),
    seen_at         timestamptz NOT NULL DEFAULT now(),

    scope           text NOT NULL,
    target_table    text,
    target_id       uuid,
    target_field    text,
    activity        rig_presence_activity NOT NULL DEFAULT 'viewing',

    -- A control belongs to a row. Focused on "title" while looking at a list is
    -- not a state any client can produce and not one this table will hold.
    CONSTRAINT rig_presence_field_needs_a_row
        CHECK (target_field IS NULL OR target_id IS NOT NULL),
    -- And a row belongs to a table. An identifier with nothing to say which
    -- table it is in is an identifier no reader can use.
    CONSTRAINT rig_presence_row_needs_a_table
        CHECK (target_id IS NULL OR target_table IS NOT NULL)
);

COMMENT ON TABLE rig_presence IS
    'Who is here and what they are looking at. One row per browser tab, rewritten every heartbeat and deleted once it is stale.';
COMMENT ON COLUMN rig_presence.id IS
    'The row''s identifier. Nothing references it: a client addresses its own presence by session key, so this identifier never leaves the server except on the stream.';
COMMENT ON COLUMN rig_presence.tenant_id IS
    'The tenant this presence is inside. Every read of this table is filtered on it, the stream included.';
COMMENT ON COLUMN rig_presence.account_id IS
    'Who is present. Read from the credential and never from a request body, which is what makes "you may only write your own presence" a sentence a client cannot phrase rather than a rule somebody enforces.';
COMMENT ON COLUMN rig_presence.session_key IS
    'Which tab this is. The browser names itself, and the name points at nothing: one sign-in can have several tabs, and each is present separately rather than overwriting the others.';
COMMENT ON COLUMN rig_presence.created_at IS
    'When this tab first appeared. It deliberately does not move on a heartbeat, so "joined four minutes ago" stays answerable.';
COMMENT ON COLUMN rig_presence.seen_at IS
    'The last heartbeat. Not updated_at: this is not when the row was last edited, it is the whole meaning of the row — whoever is reading decides whether somebody is here by comparing it against the configured TTL.';
COMMENT ON COLUMN rig_presence.scope IS
    'Which part of the application this presence is in, named by the application: a board, a document, a tenant. It is what a subscriber narrows the stream by, so it decides how much presence traffic one screen pays for.';
COMMENT ON COLUMN rig_presence.target_table IS
    'The table the row being looked at is in, checked against this API''s own tables. Null means present in the scope without being on a particular row.';
COMMENT ON COLUMN rig_presence.target_id IS
    'Which row. There is deliberately no foreign key, and this is the one place rig accepts a polymorphic reference: nothing joins to a presence row, nothing embeds one, and no client filters a list of them — so the integrity a key would buy has no reader. A presence pointing at a row somebody just deleted is not a bug, it is a row that will be gone before anybody looks.';
COMMENT ON COLUMN rig_presence.target_field IS
    'Which field, when the application tracks focus that finely. This is what turns "Simon is on this issue" into "Simon is editing the title".';
COMMENT ON COLUMN rig_presence.activity IS
    'Whether they are looking or typing. Separate from target_field because a client may know somebody is editing before it knows which control has focus.';

-- There is deliberately no `state jsonb` column, and there was going to be: an
-- escape hatch for whatever else an application wants drawn beside a name — a
-- colour, a selection range.
--
-- It cannot work, and the reason is the read path. Presence is read over a live
-- shape; the generated row type is what a TanStack DB collection is
-- parameterised by; and that type's index signature does not admit `unknown`.
-- So a jsonb column here makes the only way to read this table fail to compile,
-- and the hatch would have been reachable from nothing.
--
-- What stands in for it is the three target columns plus `activity`, which is a
-- closed vocabulary rather than an open one. An application that needs more says
-- it in a table of its own, where it can have the columns it means.

-- One row per tab, and the lookup every heartbeat makes. The column order is
-- that lookup's.
CREATE UNIQUE INDEX rig_presence_session_key
    ON rig_presence (tenant_id, account_id, session_key);

-- What a new subscriber's first fetch reads, and what the shape narrows by.
--
-- Two columns and not three. A trailing `seen_at DESC` would have made the
-- ordered read free, and it would also have put the one column every heartbeat
-- rewrites into an index — which is the thing the note below says this table
-- cannot afford. The read it would have helped is a few hundred rows.
CREATE INDEX rig_presence_scope_idx
    ON rig_presence (tenant_id, scope);

-- The foreign key, covered. A single-column key gets no credit for a composite
-- index it does not lead, so the unique key above — which does contain
-- account_id, in second place — is not this.
CREATE INDEX rig_presence_account_idx ON rig_presence (account_id);

-- There is deliberately no index on seen_at, and the sweeper is the one thing
-- that wants one.
--
-- An UPDATE touching an indexed column cannot be a HOT update, and every
-- heartbeat rewrites seen_at and nothing else — so an index there would turn the
-- most frequent write in the application into index maintenance on every beat.
-- The sweeper scans instead, which is the right trade because this table is
-- bounded by people rather than by time: a few hundred rows in a large tenant,
-- where one sequential scan a minute costs less than the write amplification
-- would have.
--
-- fillfactor leaves room on the page for the new tuple versions, which is what
-- keeps those HOT updates actually HOT.
ALTER TABLE rig_presence SET (fillfactor = 70);

-- A table rewritten every twenty seconds that never grows needs a vacuum
-- schedule of its own. The default scale factor is a *fraction* of the table,
-- and a fraction of three hundred rows is never reached however many dead tuples
-- pile up behind it — so the table bloats to hundreds of pages and every scan
-- the sweeper does gets permanently slower. Absolute thresholds are the fix, and
-- this is the one table in the foundation that needs them.
ALTER TABLE rig_presence SET (
    autovacuum_vacuum_scale_factor  = 0.0,
    autovacuum_vacuum_threshold     = 200,
    autovacuum_analyze_scale_factor = 0.0,
    autovacuum_analyze_threshold    = 200
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE rig_presence;
DROP TYPE rig_presence_activity;

-- +goose StatementEnd
