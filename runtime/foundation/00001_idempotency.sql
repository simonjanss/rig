-- +goose Up
-- +goose StatementBegin

-- One write that carried an Idempotency-Key, and what answering it produced.
--
-- The row and the write it describes are committed together. That is the whole
-- design: a claim recorded separately from the write it claims can outlive a
-- rolled-back write, and then a key that wrote nothing replays a success
-- forever. One transaction makes the record and the effect the same fact.
--
-- Which is also why a second request holding the same key simply waits. It
-- blocks on the unique index below until the first transaction ends, and then
-- either reads a completed row and replays it, or finds nothing because the
-- first attempt rolled back and does the work itself. There is no in-flight
-- state, no lease and no TTL to expire, because Postgres is already keeping
-- exactly that bookkeeping for its own reasons.
--
-- tenant_id has no foreign key, following rig_file and for the same reason: a
-- retried write is worth deduplicating in a project with no authentication at
-- all, where rig_tenant may simply not exist.
CREATE TABLE rig_idempotency (
    tenant_id       uuid        NOT NULL,
    key             text        NOT NULL,
    endpoint        text        NOT NULL,

    fingerprint     bytea       NOT NULL,
    status          integer,
    response        text,
    request_id      text        NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (tenant_id, key, endpoint)
);

-- The retention sweep, and the only read that is not by primary key. It leads
-- with created_at rather than with tenant_id, which is the other way round from
-- every other index in the foundation and is the shape the one query that reads
-- it has: the sweep deletes by age across all tenants, and a composite leading
-- with tenant_id gives that no range to scan.
CREATE INDEX rig_idempotency_created_idx ON rig_idempotency (created_at);

COMMENT ON TABLE  rig_idempotency IS 'One write that carried an Idempotency-Key, recorded in the same transaction as the write itself.';
COMMENT ON COLUMN rig_idempotency.tenant_id IS 'Who the key belongs to. Keys are per tenant, so two tenants choosing the same string is not a collision.';
COMMENT ON COLUMN rig_idempotency.key IS 'What the client sent in Idempotency-Key. Opaque here: it is the client''s to choose and the server''s only to match.';
COMMENT ON COLUMN rig_idempotency.endpoint IS 'The route pattern the key was used against, not the path. Part of the key so that one client-side identifier reused across two endpoints is two records rather than a replay of the wrong one.';
COMMENT ON COLUMN rig_idempotency.fingerprint IS 'sha256 of the request body. A second request with the same key and a different body is refused rather than answered, because replaying the wrong response is worse than any error. For a multipart write it covers the fields and not the file bytes, which would mean buffering an upload larger than memory.';
COMMENT ON COLUMN rig_idempotency.status IS 'The HTTP status the write answered with. Null only inside the transaction that is about to fill it in, so a committed row always has one and a reader never has to handle the gap.';
COMMENT ON COLUMN rig_idempotency.response IS 'The response body, replayed verbatim. Text and not jsonb, because jsonb normalises: it reorders keys and re-renders whitespace, so a replay would be semantically equal to what the first caller got rather than identical to it. Nothing queries this column, so the bytes are worth more than the type.';
COMMENT ON COLUMN rig_idempotency.request_id IS 'The request that did the work, kept so a replay can say which one it is replaying.';
COMMENT ON COLUMN rig_idempotency.created_at IS 'When the write committed. What the retention sweep reads.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS rig_idempotency;
-- +goose StatementEnd
