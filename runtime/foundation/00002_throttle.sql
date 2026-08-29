-- +goose Up
-- +goose StatementBegin

-- How many calls one caller has made inside one window.
--
-- This is the half of rate limiting that has to be written down. The auth
-- limits need no table at all: they count rows in rig_auth_log, which is a
-- trail somebody wanted anyway, so the counter and the audit record cannot
-- drift apart because they are the same rows. That trick only works for events
-- worth keeping. An ordinary API call is not one, and there are several orders
-- of magnitude more of them, so counting those means keeping a counter.
--
-- A row is one (limit, key, bucket) and holds nothing but the tally. Buckets
-- are the limit's own window, truncated: a limit of 1000/minute keys a row per
-- caller per minute, and the sliding count is that row plus a weighted slice of
-- the one before it. Bucket boundaries rather than a row per event, because a
-- row per event is the audit-log design again and it is what this table exists
-- to avoid.
--
-- Nothing here has a foreign key and there is no tenant_id. A limit on an
-- address that resolved to no tenant is the one a limiter needs most — the same
-- argument that makes rig_auth_log.tenant_id nullable — and a key here is
-- deliberately opaque: an address, an account, an API key or a tenant, and the
-- table is not entitled to know which.
--
-- The count is deliberately approximate across replicas. See runtime/throttle:
-- each process holds its increments locally while a caller is nowhere near
-- their limit and only reconciles here as they approach it, so a quiet caller
-- costs no writes at all and a loud one is counted closely. Exactness would
-- mean a round trip per request to a single row, which under the load this
-- table exists for is a bottleneck rather than a defence.
CREATE TABLE rig_throttle (
    limit_name      text        NOT NULL,
    key_kind        text        NOT NULL,
    key_value       text        NOT NULL,
    bucket_at       timestamptz NOT NULL,

    n               integer     NOT NULL,

    PRIMARY KEY (limit_name, key_kind, key_value, bucket_at)
);

-- The sweep, and the only read that is not by primary key. It leads with
-- bucket_at for the reason rig_idempotency_created_idx leads with created_at:
-- the sweep deletes by age across every key there is, and a composite leading
-- with the key gives that no range to scan.
--
-- n is deliberately outside every index. It is the only column that ever
-- changes and it changes constantly, so leaving it unindexed is what lets those
-- updates stay on the same page as the row they update.
CREATE INDEX rig_throttle_bucket_idx ON rig_throttle (bucket_at);

COMMENT ON TABLE  rig_throttle IS 'How many calls one caller has made inside one window. Approximate by design — see runtime/throttle for what each replica holds back and why.';
COMMENT ON COLUMN rig_throttle.limit_name IS 'Which limit this tally belongs to, matching Limit.Name — for example "api.account". Part of the key so that two limits over the same caller are two budgets rather than one shared one.';
COMMENT ON COLUMN rig_throttle.key_kind IS 'What sort of thing key_value names: an ip, an account, a tenant, an api_key. Separate from the value because an address and an account id are different budgets even in the impossible case that they read the same.';
COMMENT ON COLUMN rig_throttle.key_value IS 'The caller, opaque. Whatever the kind says it is, stringified, and never parsed back.';
COMMENT ON COLUMN rig_throttle.bucket_at IS 'The start of the window this tally covers, truncated to the limit''s own window. What the sweep reads, and what makes a stale tally worthless rather than wrong.';
COMMENT ON COLUMN rig_throttle.n IS 'The tally. Incremented by a delta rather than by one, because a replica that held requests back flushes them together.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS rig_throttle;
-- +goose StatementEnd
