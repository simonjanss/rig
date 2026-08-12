-- +goose Up
-- +goose StatementBegin

-- The example's own authorization model. rig ships none.
--
-- These four tables are not part of the foundation `rig setup-project` writes:
-- rig derives the permission *keys* from the schema and generates the check that
-- reads them off a caller's claims, and stops there. Deciding who holds which is
-- the application's, because every product answers it differently — a role table
-- like this one, a switch on the account's level, a claim on a federated token,
-- a group in a directory.
--
-- What connects them is services/tenant: it seeds these rows from
-- api.PermissionKeys(), and its Grants function is handed to auth.Config, which
-- is the whole integration.

-- Permissions are global and roles are per tenant.
--
-- A permission is a fact about what the software can do — "lesson.publish"
-- means the same thing everywhere — while a role is a decision one organization
-- made about who may do it. Letting each tenant invent permissions would mean
-- the code could never check one by name.
CREATE TABLE permission (
    id                      uuid PRIMARY KEY,
    created_at              timestamptz NOT NULL DEFAULT now(),

    key                     text NOT NULL,
    name                    text NOT NULL,
    description             text NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX permission_key_key ON permission (key);

COMMENT ON TABLE  permission IS 'One thing the software can do. Global: the same key means the same thing in every tenant.';
COMMENT ON COLUMN permission.key IS 'What code checks for, such as lesson.publish.';
COMMENT ON COLUMN permission.name IS 'A short label for an administration screen.';
COMMENT ON COLUMN permission.description IS 'What holding this permission lets somebody do.';

CREATE TABLE role (
    id                      uuid PRIMARY KEY,
    tenant_id               uuid NOT NULL REFERENCES tenant (id),

    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz,

    key                     text NOT NULL,
    name                    text NOT NULL,
    description             text NOT NULL DEFAULT '',
    is_system               boolean NOT NULL DEFAULT false
);

CREATE UNIQUE INDEX role_tenant_key_key ON role (tenant_id, key);
CREATE INDEX role_tenant_created_idx ON role (tenant_id, created_at DESC);

COMMENT ON TABLE  role IS 'A named bundle of permissions, defined per tenant.';
COMMENT ON COLUMN role.key IS 'Stable identifier, such as editor.';
COMMENT ON COLUMN role.name IS 'What to call the role in the interface.';
COMMENT ON COLUMN role.description IS 'What the role is for.';
COMMENT ON COLUMN role.is_system IS 'Whether rig created the role. A system role should not be editable.';

CREATE TABLE role_permission (
    role_id                 uuid NOT NULL REFERENCES role (id),
    permission_id           uuid NOT NULL REFERENCES permission (id),
    PRIMARY KEY (role_id, permission_id)
);

CREATE INDEX role_permission_permission_id_idx ON role_permission (permission_id);

COMMENT ON TABLE  role_permission IS 'Which permissions a role grants.';
COMMENT ON COLUMN role_permission.role_id IS 'The role doing the granting.';
COMMENT ON COLUMN role_permission.permission_id IS 'The permission it grants.';

-- A key of its own, unlike role_permission above.
--
-- Not a style choice. rig turns a pure join table into a relation instead of a
-- resource, and it can only do that when it projects both ends: role_permission
-- links two tables in this schema, so rig sees it as one. This one links to
-- account, which belongs to the foundation and is deliberately not projected —
-- so rig sees an ordinary table, and an ordinary table is addressed by `id`.
CREATE TABLE account_role (
    id                      uuid PRIMARY KEY,
    account_id              uuid NOT NULL REFERENCES account (id),
    role_id                 uuid NOT NULL REFERENCES role (id),
    UNIQUE (account_id, role_id)
);

CREATE INDEX account_role_role_id_idx ON account_role (role_id);

COMMENT ON TABLE  account_role IS 'Which roles an account holds.';
COMMENT ON COLUMN account_role.id IS 'Surrogate key; the pair below is what is unique.';
COMMENT ON COLUMN account_role.account_id IS 'The account holding the role.';
COMMENT ON COLUMN account_role.role_id IS 'The role it holds.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE account_role;
DROP TABLE role_permission;
DROP TABLE role;
DROP TABLE permission;

-- +goose StatementEnd
