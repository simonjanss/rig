-- +goose Up
-- +goose StatementBegin

-- The example's own authorization model, adapted from examples/auth — that
-- file says why each decision goes the way it does. rig derives the permission
-- keys from the schema and generates the check that reads them off a caller's
-- claims; who holds which is the application's, and this is one ordinary
-- answer: global permissions, per-tenant roles, and a join to accounts.
--
-- services/authz seeds these rows from api.PermissionKeys() and authz.AuthKeys(),
-- and its Grants function is handed to auth.Config in main.go — the whole
-- integration.

CREATE TABLE permission (
    id                      uuid PRIMARY KEY,
    created_at              timestamptz NOT NULL DEFAULT now(),

    key                     text NOT NULL,
    name                    text NOT NULL,
    description             text NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX permission_key_key ON permission (key);

COMMENT ON TABLE  permission IS 'One thing the software can do. Global: the same key means the same thing in every tenant.';
COMMENT ON COLUMN permission.key IS 'What code checks for, such as todo.write.';
COMMENT ON COLUMN permission.name IS 'A short label for an administration screen.';
COMMENT ON COLUMN permission.description IS 'What holding this permission lets somebody do.';

CREATE TABLE role (
    id                      uuid PRIMARY KEY,
    tenant_id               uuid NOT NULL REFERENCES rig_tenant (id),

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
COMMENT ON COLUMN role.key IS 'Stable identifier, such as member.';
COMMENT ON COLUMN role.name IS 'What to call the role in the interface.';
COMMENT ON COLUMN role.description IS 'What the role is for.';
COMMENT ON COLUMN role.is_system IS 'Whether the application created the role. A system role should not be editable.';

CREATE TABLE role_permission (
    role_id                 uuid NOT NULL REFERENCES role (id),
    permission_id           uuid NOT NULL REFERENCES permission (id),
    PRIMARY KEY (role_id, permission_id)
);

CREATE INDEX role_permission_permission_id_idx ON role_permission (permission_id);

COMMENT ON TABLE  role_permission IS 'Which permissions a role grants.';
COMMENT ON COLUMN role_permission.role_id IS 'The role doing the granting.';
COMMENT ON COLUMN role_permission.permission_id IS 'The permission it grants.';

-- A key of its own, unlike role_permission above: this table links to a
-- foundation table rig deliberately does not project as a resource, so rig
-- sees an ordinary table, and an ordinary table is addressed by `id`.
CREATE TABLE account_role (
    id                      uuid PRIMARY KEY,
    account_id              uuid NOT NULL REFERENCES rig_account (id),
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
