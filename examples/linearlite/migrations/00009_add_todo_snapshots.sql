-- +goose Up
-- +goose StatementBegin

-- The snapshot triple. Its presence is what makes the table versioned: every
-- update copies the row as it was before writing the change, so the history
-- panel in the front end is rows in this same table, streamed live over
-- /todo/{id}/_versions/_stream.
CREATE TYPE todo_version_type AS ENUM ('Original', 'Snapshot');

ALTER TABLE todo
    ADD COLUMN version_type          todo_version_type NOT NULL DEFAULT 'Original',
    ADD COLUMN snapshot_from_todo_id uuid REFERENCES todo(id),
    ADD COLUMN snapshot_from_todo_at timestamptz;

-- The constraint is what keeps a snapshot immutable and an original unmarked.
-- Without it the two states are only a convention, and one stray UPDATE turns
-- history into fiction.
ALTER TABLE todo ADD CONSTRAINT todo_version_check CHECK (
    (version_type = 'Original'
        AND snapshot_from_todo_id IS NULL
        AND snapshot_from_todo_at IS NULL)
    OR
    (version_type = 'Snapshot'
        AND snapshot_from_todo_id IS NOT NULL
        AND snapshot_from_todo_at IS NOT NULL
        AND updated_at IS NULL
        AND deleted_at IS NULL)
);

-- Deliberately not partial. A partial index would be smaller, but the
-- foreign-key check Postgres runs when a row is deleted looks across every row,
-- so it could not use one.
CREATE INDEX todo_snapshot_idx ON todo (snapshot_from_todo_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- The snapshots go with the columns: without them a row is history nothing can
-- point at.
DELETE FROM todo WHERE version_type = 'Snapshot';

ALTER TABLE todo DROP CONSTRAINT todo_version_check;
ALTER TABLE todo
    DROP COLUMN version_type,
    DROP COLUMN snapshot_from_todo_id,
    DROP COLUMN snapshot_from_todo_at;

DROP TYPE todo_version_type;

-- +goose StatementEnd
