-- +goose Up
-- +goose StatementBegin

-- The replication half of the corpus. What rig reads here decides whether a
-- table can carry a live-sync shape, and both answers need a case: a table a
-- publication names, and one Postgres will never publish anything about.
CREATE PUBLICATION corpus_publication FOR TABLE lesson;

CREATE UNLOGGED TABLE scratch (
    id         uuid PRIMARY KEY,
    tenant_id  uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    note       text NOT NULL
);
CREATE INDEX scratch_tenant_idx ON scratch (tenant_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE scratch;
DROP PUBLICATION corpus_publication;
-- +goose StatementEnd
