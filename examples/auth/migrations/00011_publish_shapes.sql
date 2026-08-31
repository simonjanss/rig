-- +goose Up
-- +goose StatementBegin

-- Publish the tables that carry a live-sync shape.
--
-- A shape is a filter in front of a stream the sync service follows over logical
-- replication, and logical replication carries only what a publication names. The sync
-- service will add a table itself on the first subscription, so this is not the
-- difference between a stream and no stream — it is the difference between one that
-- works everywhere and one that works wherever the sync service's database role happens
-- to own the table. Postgres wants ownership both to publish a table and to set
-- REPLICA IDENTITY FULL, and a deployment with least privilege grants neither. RIG5090
-- and RIG5093 are the diagnostics.
--
-- Written by `rig migration new --publish-shapes` from the tables that ask for a shape,
-- which is why rig's own are here beside the project's: an inbox line and a presence row
-- are streamed because `notifications:` and `presence:` are on, without any table's
-- configuration asking.
--
-- The publication is the sync service's own, electric_publication_default, and that is
-- deliberate: it reads only that one. A publication under a name of ours would satisfy
-- nothing at run time. This migration creates it first and therefore owns it, which is
-- what lets the service run as a role owning no tables.
--
-- **The deployment has to set ELECTRIC_MANUAL_TABLE_PUBLISHING=true.** Without it the
-- service tries to maintain this publication itself and fails on table ownership, and
-- the error it reports — `must be owner of table <table>` — says nothing about either.
-- `rig db up` needs nothing: locally the service owns everything.
--
-- Under ELECTRIC_REPLICATION_STREAM_ID the name has a different suffix. Rename it here
-- to match; there is nothing rig can read to know.

-- Postgres has no CREATE PUBLICATION IF NOT EXISTS, hence the block.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_publication WHERE pubname = 'electric_publication_default') THEN
        CREATE PUBLICATION electric_publication_default;
    END IF;
END
$$;

ALTER PUBLICATION electric_publication_default ADD TABLE rig_notification_recipient;

-- REPLICA IDENTITY is the second half of the same job, and it is gated on
-- ownership exactly as the line above is: the sync service wants the whole old row on
-- an update or a delete, and the default identity carries only the primary key. A
-- subscriber then hears that a row changed with nothing to match against the one it is
-- holding — inserts keep working, which is what lets this survive a demo.
ALTER TABLE rig_notification_recipient REPLICA IDENTITY FULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP PUBLICATION electric_publication_default;

ALTER TABLE rig_notification_recipient REPLICA IDENTITY DEFAULT;

-- +goose StatementEnd
