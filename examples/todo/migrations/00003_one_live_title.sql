-- +goose Up
-- +goose StatementBegin

-- One live task per title. The predicate is the whole point, and it has to
-- exclude two things rather than one:
--
--   deleted_at IS NULL          a retired task does not hold its title. Reusing
--                               the title of something you deleted is fine, and
--                               the create rule deliberately does not look in
--                               the trash for one.
--
--   version_type = 'Original'   every update writes a snapshot of the row as it
--                               was, so without this an update would collide
--                               with the copy it had just taken.
--
-- Case-sensitive, matching what the service's rule compares. An index that
-- refuses more than the rule catches is a raw 409 from the driver where a
-- message naming the field should be.
CREATE UNIQUE INDEX todo_live_title_key ON todo (tenant_id, title)
    WHERE deleted_at IS NULL AND version_type = 'Original';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX todo_live_title_key;

-- +goose StatementEnd
