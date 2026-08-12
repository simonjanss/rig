-- +goose Up
-- +goose StatementBegin

CREATE TYPE player_position AS ENUM ('goalkeeper', 'defender', 'midfielder', 'forward');

CREATE TABLE team (
    id                      uuid PRIMARY KEY,
    tenant_id               uuid NOT NULL,

    name                    text NOT NULL,
    is_active               boolean NOT NULL DEFAULT true,

    created_at              timestamptz NOT NULL DEFAULT now(),
    created_by_account_id   uuid,
    updated_at              timestamptz,
    updated_by_account_id   uuid,
    deleted_at              timestamptz,
    deleted_by_account_id   uuid
);

COMMENT ON TABLE team IS 'A squad somebody manages.';
COMMENT ON COLUMN team.name IS 'What the manager calls their squad.';
COMMENT ON COLUMN team.is_active IS 'Whether the squad is playing this season.';

CREATE INDEX team_tenant_name_idx ON team (tenant_id, name);

CREATE TABLE player (
    id                      uuid PRIMARY KEY,
    tenant_id               uuid NOT NULL,

    full_name               text NOT NULL,
    position                player_position NOT NULL,
    shirt_number            integer,

    created_at              timestamptz NOT NULL DEFAULT now(),
    created_by_account_id   uuid,
    updated_at              timestamptz,
    updated_by_account_id   uuid,
    deleted_at              timestamptz,
    deleted_by_account_id   uuid
);

COMMENT ON TABLE player IS 'Somebody a squad can pick.';
COMMENT ON COLUMN player.full_name IS 'The name on the back of the shirt.';
COMMENT ON COLUMN player.position IS 'Where on the pitch the player lines up.';
COMMENT ON COLUMN player.shirt_number IS 'The squad number, or null if unassigned.';

CREATE INDEX player_tenant_name_idx ON player (tenant_id, full_name);

-- A pure join table: its primary key is exactly its two foreign keys, so rig
-- reads it as a many-to-many relation rather than a resource of its own.
CREATE TABLE team_player (
    team_id                 uuid NOT NULL REFERENCES team (id),
    player_id               uuid NOT NULL REFERENCES player (id),

    PRIMARY KEY (team_id, player_id)
);

COMMENT ON TABLE team_player IS 'Which players a squad has picked.';

CREATE INDEX team_player_player_idx ON team_player (player_id);

CREATE TABLE fixture (
    id                      uuid PRIMARY KEY,
    tenant_id               uuid NOT NULL,

    home_team_id            uuid NOT NULL REFERENCES team (id),
    away_team_id            uuid NOT NULL REFERENCES team (id),
    kickoff_at              timestamptz NOT NULL,

    created_at              timestamptz NOT NULL DEFAULT now(),
    created_by_account_id   uuid,
    updated_at              timestamptz,
    updated_by_account_id   uuid
);

COMMENT ON TABLE fixture IS 'One match between two squads.';
COMMENT ON COLUMN fixture.home_team_id IS 'The squad playing at home.';
COMMENT ON COLUMN fixture.away_team_id IS 'The squad playing away.';
COMMENT ON COLUMN fixture.kickoff_at IS 'When the match starts.';

CREATE INDEX fixture_tenant_kickoff_idx ON fixture (tenant_id, kickoff_at DESC);
CREATE INDEX fixture_home_team_idx ON fixture (home_team_id);
CREATE INDEX fixture_away_team_idx ON fixture (away_team_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE fixture;
DROP TABLE team_player;
DROP TABLE player;
DROP TABLE team;
DROP TYPE player_position;

-- +goose StatementEnd
