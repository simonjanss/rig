-- +goose Up
-- +goose StatementBegin

-- One ordinary table, so a signed-in caller has something to read.
--
-- It exists to prove the session works, and to prove it is scoped: a bookmark
-- saved at acme.localhost is invisible at beta.localhost, because every generated
-- query ANDs in the tenant and the tenant came from the host.
CREATE TABLE bookmark (
    id                      uuid PRIMARY KEY,
    tenant_id               uuid NOT NULL REFERENCES tenant (id),

    title                   text NOT NULL,
    url                     text NOT NULL,

    created_at              timestamptz NOT NULL DEFAULT now(),
    created_by_account_id   uuid REFERENCES account (id),
    updated_at              timestamptz,
    updated_by_account_id   uuid REFERENCES account (id)
);

COMMENT ON TABLE  bookmark IS 'A link somebody saved.';
COMMENT ON COLUMN bookmark.title IS 'What to call it in a list.';
COMMENT ON COLUMN bookmark.url IS 'Where it points.';

CREATE INDEX bookmark_tenant_created_idx ON bookmark (tenant_id, created_at DESC);
CREATE INDEX bookmark_created_by_idx ON bookmark (created_by_account_id);
CREATE INDEX bookmark_updated_by_idx ON bookmark (updated_by_account_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE bookmark;

-- +goose StatementEnd
