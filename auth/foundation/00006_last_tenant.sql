-- +goose Up
-- +goose StatementBegin

-- Where somebody was last, answerable in one index probe.
--
-- A sign-in that names no tenant lands in the tenant the person was last in,
-- which is the tenant they meant. The question is "the most recent session for
-- any of this identity's accounts", and the row that answers it is the family
-- root — one per sign-in rather than one per rotation.
--
-- The index is partial for that reason: roots are a small fraction of the table
-- and they are the only rows this asks about. It needs an index at all because
-- consumed tokens are kept rather than deleted — that is what makes replay
-- detectable — so without one the query is a sort over every token an account
-- has ever held, on a path that runs on every sign-in.
--
-- No new table and no new column. The data was already here; only the way in
-- was missing.
CREATE INDEX rig_account_token_account_root_created_idx
    ON rig_account_token (account_id, created_at DESC)
    WHERE root_token_id = id;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX rig_account_token_account_root_created_idx;

-- +goose StatementEnd
