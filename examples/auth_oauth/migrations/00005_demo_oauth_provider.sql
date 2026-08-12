-- +goose NO TRANSACTION
-- +goose Up
-- +goose StatementBegin

-- A fourth provider, for the stand-in this example serves itself.
--
-- oauth.Provider.Name has to be a label of this enum, because it is what gets
-- stored in identity_oauth.provider — so adding a provider is a migration, not a
-- line of configuration. That is the point worth taking from this file: the same
-- is true of a real one. Wiring Okta or Auth0 up starts here.
--
-- NO TRANSACTION because Postgres will not let a value added to an enum be used
-- in the same transaction that added it, and goose wraps a migration in one by
-- default. Adding a label is not reversible either, which is why Down leaves it:
-- dropping a value would need the type rebuilt and every column using it
-- rewritten, and an unused label costs nothing.
ALTER TYPE oauth_provider ADD VALUE IF NOT EXISTS 'Demo';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Deliberately empty. See above.
SELECT 1;

-- +goose StatementEnd
