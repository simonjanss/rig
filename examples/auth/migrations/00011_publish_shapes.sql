-- +goose Up
-- +goose StatementBegin

-- Publish the tables that carry a live-sync shape.
--
-- A shape is a filter in front of a stream the sync service follows over
-- logical replication, and logical replication carries only what a publication
-- names. The sync service will add a table itself on the first subscription, so
-- this is not the difference between a stream and no stream — it is the
-- difference between one that works everywhere and one that works wherever
-- Electric's database role happens to own the table. Postgres wants ownership
-- both to publish a table and to set REPLICA IDENTITY FULL, and a deployment
-- with least privilege grants neither. RIG5090 is the diagnostic.
--
-- No table here asks for a stream. The inbox gets one from rig anyway,
-- because `notifications:` is on, and this is the whole cost of that: one
-- table published so a client can subscribe to its own notifications.
--
-- Postgres has no CREATE PUBLICATION IF NOT EXISTS, hence the block.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_publication WHERE pubname = 'rig_publication') THEN
        CREATE PUBLICATION rig_publication;
    END IF;
END
$$;

ALTER PUBLICATION rig_publication ADD TABLE rig_notification_recipient;

-- REPLICA IDENTITY is the second half of the same job, and it is gated on
-- ownership exactly as the line above is: Electric wants the whole old row on
-- an update or a delete, and the default identity carries only the primary key.
-- Publishing without it would leave the deployment this migration exists for
-- failing on the half nobody wrote down.
ALTER TABLE rig_notification_recipient REPLICA IDENTITY FULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP PUBLICATION rig_publication;

ALTER TABLE rig_notification_recipient REPLICA IDENTITY DEFAULT;

-- +goose StatementEnd
