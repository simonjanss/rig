-- +goose Up
-- +goose StatementBegin

-- A corpus covering everything rig needs to read back out of Postgres.
CREATE TYPE lesson_status AS ENUM ('planned', 'in_progress', 'completed');
CREATE TYPE lesson_version_type AS ENUM ('Original', 'Snapshot');

CREATE TABLE lesson (
    id                      uuid PRIMARY KEY,
    tenant_id               uuid NOT NULL,

    created_at              timestamptz NOT NULL DEFAULT now(),
    created_by_account_id   uuid,
    updated_at              timestamptz,
    updated_by_account_id   uuid,
    deleted_at              timestamptz,
    deleted_by_account_id   uuid,

    version_type            lesson_version_type NOT NULL DEFAULT 'Original',
    snapshot_from_lesson_id uuid REFERENCES lesson(id),
    snapshot_from_lesson_at timestamptz,

    title                   text NOT NULL,
    notes                   text,
    status                  lesson_status NOT NULL,
    capacity                integer,
    price                   numeric(10,2),
    tags                    text[],
    payload                 jsonb,
    starts_at               timestamptz NOT NULL,
    starts_date             date,
    is_published            boolean NOT NULL DEFAULT false,
    slug                    text GENERATED ALWAYS AS (lower(title)) STORED,

    UNIQUE (tenant_id, title),
    CONSTRAINT lesson_capacity_positive CHECK (capacity IS NULL OR capacity > 0)
);

COMMENT ON TABLE lesson IS 'A scheduled teaching occasion.';
COMMENT ON COLUMN lesson.title IS 'Name shown in the timetable.';

CREATE INDEX lesson_tenant_starts_idx ON lesson (tenant_id, starts_at DESC);
CREATE INDEX lesson_live_idx ON lesson (tenant_id) WHERE deleted_at IS NULL;
CREATE INDEX lesson_snapshot_idx ON lesson (snapshot_from_lesson_id);
CREATE INDEX lesson_payload_idx ON lesson USING gin (payload);

CREATE TABLE tag (
    id         uuid PRIMARY KEY,
    tenant_id  uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    label      text NOT NULL
);
CREATE INDEX tag_tenant_idx ON tag (tenant_id);

CREATE TABLE lesson_tag (
    lesson_id uuid NOT NULL REFERENCES lesson(id) ON DELETE CASCADE,
    tag_id    uuid NOT NULL REFERENCES tag(id) ON UPDATE RESTRICT,
    PRIMARY KEY (lesson_id, tag_id)
);
CREATE INDEX lesson_tag_tag_idx ON lesson_tag (tag_id);

CREATE VIEW published_lesson AS
    SELECT id, tenant_id, title FROM lesson WHERE is_published;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP VIEW published_lesson;
DROP TABLE lesson_tag;
DROP TABLE tag;
DROP TABLE lesson;
DROP TYPE lesson_version_type;
DROP TYPE lesson_status;
-- +goose StatementEnd
