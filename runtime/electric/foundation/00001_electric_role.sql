-- +goose Up
-- +goose StatementBegin

-- The Postgres role the sync service authenticates as, and what it is allowed to read.
--
-- Nothing here is a credential, which is the whole reason this can be a file in git: the
-- role is created with LOGIN and no password at all, and cannot connect until something
-- gives it one. That something is electric.SetRolePassword, which runs immediately after
-- these migrations, reads the same connection string the sync service itself reads as
-- DATABASE_URL, and sets the password out of it. One secret with two readers rather than a
-- second one holding a duplicate that could drift.
--
-- Two things this arrangement does not answer, and they are accepted rather than hidden. A
-- role is cluster-scoped rather than part of one database's schema, so a migration is a
-- slightly odd place for it; and creating one needs privileges an application should not
-- carry for longer than it has to. Both are true. The grants below have to live in a
-- migration regardless — see ALTER DEFAULT PRIVILEGES — and splitting the password out is
-- what removes the reason that actually mattered, which was that the statement carried one.

DO $$
BEGIN
  -- Postgres has no CREATE ROLE IF NOT EXISTS, and a role is cluster-scoped: it can
  -- already exist because another database on the same instance created it, or because
  -- this migration ran against an environment somebody had set the role up in by hand.
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'electric') THEN
    CREATE ROLE electric LOGIN;
  END IF;

  -- Both halves of this branch are the only form that works where they run, and neither
  -- works in both places.
  --
  -- Setting the REPLICATION attribute requires a true superuser, and a managed Postgres
  -- gives nobody one: on RDS the master user is a member of rds_superuser, which carries
  -- CREATEROLE but not rolsuper, so ALTER ROLE ... REPLICATION fails for every principal
  -- on the instance. rds_replication is AWS's stand-in and is what rds_superuser is
  -- allowed to grant.
  --
  -- On the postgres image `rig db up` starts there is no rds_replication role to grant,
  -- and migrations run as the superuser, so the attribute is both available and required.
  -- Branching on the role existing rather than on an environment variable keeps the
  -- condition a fact about the database rather than a thing to remember to set.
  IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'rds_replication') THEN
    GRANT rds_replication TO electric;
  ELSE
    ALTER ROLE electric REPLICATION;
  END IF;

  -- CREATE is not obvious and the sync service does not start without it. It creates
  -- electric_publication_default and electric_slot_default itself on first connection, and
  -- CREATE PUBLICATION is a database-level privilege.
  --
  -- current_database() rather than a literal: the name is whatever the deployment calls it,
  -- and whatever `rig db up` made locally. format(%I) quotes it as an identifier.
  EXECUTE format('GRANT CONNECT, CREATE ON DATABASE %I TO electric', current_database());
END
$$;

-- +goose StatementEnd

-- +goose StatementBegin

-- Read access to the schema as it is, and as it will be.
--
-- ALTER DEFAULT PRIVILEGES is recorded per grantor and per schema rather than globally: it
-- covers tables created by the role that executed it and no others. Running it from a
-- migration is what makes it the right role — the same one that creates every table — and
-- it is why these statements have to live in a migration even though the role creation
-- above is a one-off. Get this wrong and every table added afterwards is invisible to the
-- sync service, and its shapes come back empty rather than failing.
--
-- This set applies after rig's other foundation sets and before the project's own, which is
-- what makes the two lines cover everything between them: ON ALL TABLES covers the tables
-- rig has already created, and the default privileges cover every one the project adds
-- after.
--
-- SELECT is not the whole story, and the rest is not solved here. ALTER PUBLICATION ... ADD
-- TABLE and setting a replica identity both require table *ownership*, so a shape request
-- against a table this role does not own still fails — which is what `rig migration new
-- --publish-shapes` writes, and what RIG5090 refuses a project without. The sync service
-- starts, creates its publication and its slot and reports healthy without any of it:
-- ownership is what a shape request needs, not what booting needs.
GRANT USAGE ON SCHEMA public TO electric;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO electric;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO electric;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- The grants come off. The role does not.
--
-- DROP ROLE is not the inverse of CREATE ROLE here: a role is cluster-scoped, it may hold
-- privileges in other databases this migration cannot see, and it owns the replication slot
-- the sync service is holding open. Dropping it while that service is connected fails, and
-- dropping it when it is not is a way to lose a slot rather than a way to go back. Removing
-- the role is a deliberate act against a stopped sync service, not a rollback step.
ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE SELECT ON TABLES FROM electric;
REVOKE SELECT ON ALL TABLES IN SCHEMA public FROM electric;
REVOKE USAGE ON SCHEMA public FROM electric;

DO $$
BEGIN
  EXECUTE format('REVOKE CONNECT, CREATE ON DATABASE %I FROM electric', current_database());

  IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'rds_replication') THEN
    REVOKE rds_replication FROM electric;
  ELSE
    ALTER ROLE electric NOREPLICATION;
  END IF;
END
$$;

-- +goose StatementEnd
