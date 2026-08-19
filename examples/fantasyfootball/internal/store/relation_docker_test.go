//go:build docker

package store_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/examples/fantasyfootball/internal/model"
	"github.com/simonjanss/rig/examples/fantasyfootball/internal/store"
	"github.com/simonjanss/rig/runtime/dbhook"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// This is the suite the relation filters exist for: a question about a row is
// answered by looking at rows in another table, and what the answer must never
// depend on is anything the caller is not allowed to see.
func TestFilteringAcrossARelation(t *testing.T) {
	w := newWorld(t)

	rovers := w.team(t, "Rovers")
	athletic := w.team(t, "Athletic")
	derby := w.fixture(t, rovers.ID, athletic.ID)

	t.Run("a belongs-to matches on the row it points at", func(t *testing.T) {
		got := w.fixturesWhere(t, model.FixtureFilter{
			Equals: &model.FixtureFilterEquals{HomeTeam: named("Rovers")},
		})
		if len(got) != 1 || got[0].ID != derby.ID {
			t.Errorf("filtering by the home team's name should find the derby, got %d rows", len(got))
		}

		// The same condition on the other end of the same relation is a
		// different question, and the row answers only one of them.
		away := model.FixtureFilter{Equals: &model.FixtureFilterEquals{AwayTeam: named("Rovers")}}
		if got := w.fixturesWhere(t, away); len(got) != 0 {
			t.Errorf("Rovers played at home, so the away filter should not match: %d rows", len(got))
		}
	})

	t.Run("a has-many matches when at least one related row does", func(t *testing.T) {
		got := w.teamsWhere(t, model.TeamFilter{
			Equals: &model.TeamFilterEquals{
				HomeFixtures: &model.FixtureFilterEquals{ID: &derby.ID},
			},
		})
		if len(got) != 1 || got[0].ID != rovers.ID {
			t.Errorf("Rovers hosted the derby, so it should match: %d rows", len(got))
		}
	})

	t.Run("a many-to-many reaches through the link table", func(t *testing.T) {
		keeper := w.player(t, "Iker", model.PlayerPositionGoalkeeper)
		w.pick(t, rovers.ID, keeper.ID)

		got := w.teamsWhere(t, model.TeamFilter{
			Equals: &model.TeamFilterEquals{
				Players: &model.PlayerFilterEquals{Position: ptr(model.PlayerPositionGoalkeeper)},
			},
		})
		if len(got) != 1 || got[0].ID != rovers.ID {
			t.Errorf("only Rovers has a goalkeeper: %d rows", len(got))
		}

		// An empty object on the far side asks whether there is one at all,
		// which is why the field is a pointer: nil is no condition, and an empty
		// one is a condition that any related row satisfies.
		any := model.TeamFilter{Equals: &model.TeamFilterEquals{Players: &model.PlayerFilterEquals{}}}
		if got := w.teamsWhere(t, any); len(got) != 1 {
			t.Errorf("only Rovers has picked anybody: %d rows", len(got))
		}
	})

	// The reason conditions on one relation are collected into one subquery
	// rather than one subquery each. Written across two operators, they are
	// still a question about a single related row.
	t.Run("conditions on one relation hold of the same row", func(t *testing.T) {
		squad := w.team(t, "United")
		w.pick(t, squad.ID, w.numbered(t, "Manuel", model.PlayerPositionGoalkeeper, 1).ID)
		w.pick(t, squad.ID, w.numbered(t, "Robert", model.PlayerPositionForward, 9).ID)

		// A forward wearing more than 5: Robert satisfies both.
		got := w.teamsWhere(t, model.TeamFilter{
			Equals: &model.TeamFilterEquals{
				Name:    ptr("United"),
				Players: &model.PlayerFilterEquals{Position: ptr(model.PlayerPositionForward)},
			},
			GreaterThan: &model.TeamFilterRange{
				Players: &model.PlayerFilterRange{ShirtNumber: ptr(5)},
			},
		})
		if len(got) != 1 {
			t.Errorf("Robert is a forward wearing 9, so United matches: %d rows", len(got))
		}

		// A goalkeeper wearing more than 5: the squad has a goalkeeper and has
		// somebody wearing more than 5, but not one player who is both. A
		// subquery per operator would answer yes here.
		got = w.teamsWhere(t, model.TeamFilter{
			Equals: &model.TeamFilterEquals{
				Name:    ptr("United"),
				Players: &model.PlayerFilterEquals{Position: ptr(model.PlayerPositionGoalkeeper)},
			},
			GreaterThan: &model.TeamFilterRange{
				Players: &model.PlayerFilterRange{ShirtNumber: ptr(5)},
			},
		})
		if len(got) != 0 {
			t.Errorf("no United player is both a goalkeeper and wearing 9: %d rows", len(got))
		}

		// Under OR the same two conditions are a different question — either
		// will do — and one subquery whose inside is a disjunction says that.
		got = w.teamsWhere(t, model.TeamFilter{
			OrCondition: true,
			Equals: &model.TeamFilterEquals{
				Players: &model.PlayerFilterEquals{Position: ptr(model.PlayerPositionGoalkeeper)},
			},
			GreaterThan: &model.TeamFilterRange{
				Players: &model.PlayerFilterRange{ShirtNumber: ptr(5)},
			},
		})
		if len(got) == 0 {
			t.Error("United has a goalkeeper, so an OR of the two should match it")
		}
	})

	// The property the whole design is arranged around. A foreign key does not
	// have to stay inside a tenant — Postgres will happily let one row point at
	// another tenant's — so the subquery scopes the far side itself. Without
	// that, this filter would answer yes, and a filter that answers yes about a
	// row the caller cannot read is how you enumerate somebody else's data.
	t.Run("the far side is scoped to the caller's tenant", func(t *testing.T) {
		other := newWorld(t)
		secret := other.team(t, "Barcelona")

		// A fixture in this tenant pointing at the other tenant's squad.
		crossed := w.crossedFixture(t, secret.ID, athletic.ID)

		hidden := model.FixtureFilter{Equals: &model.FixtureFilterEquals{HomeTeam: named("Barcelona")}}
		if got := w.fixturesWhere(t, hidden); len(got) != 0 {
			t.Errorf("a filter should not see another tenant's squad, got %d rows", len(got))
		}

		// The row itself is still this tenant's and still readable. It is the
		// question about the related row that is refused, not the row.
		if _, err := w.repos.Fixtures.Get(w.ctx, crossed.ID); err != nil {
			t.Errorf("the fixture is this tenant's: %v", err)
		}
	})

	// Soft delete is the same argument as the tenant: a read option widens what
	// the query returns, not what it may look through to decide.
	t.Run("the far side is live whatever the read asked for", func(t *testing.T) {
		ghosts := w.team(t, "Ghosts")
		haunting := w.fixture(t, ghosts.ID, athletic.ID)

		if err := w.repos.Teams.Delete(w.ctx, dbhook.Delete[model.TeamDeleteInput, model.Team]{
			Input: model.TeamDeleteInput{ID: ghosts.ID},
		}); err != nil {
			t.Fatal(err)
		}

		gone := model.FixtureFilter{Equals: &model.FixtureFilterEquals{HomeTeam: named("Ghosts")}}
		if got := w.fixturesWhere(t, gone); len(got) != 0 {
			t.Errorf("the squad is deleted, so nothing matches through it: %d rows", len(got))
		}
		// And the fixture is still there to be found by any other means.
		if _, err := w.repos.Fixtures.Get(w.ctx, haunting.ID); err != nil {
			t.Errorf("deleting the squad should not hide the fixture: %v", err)
		}
	})

	// The filter types are mutually recursive, so how deep a request may nest
	// them is a limit the server sets rather than one the client observes by
	// running out of memory. Every level checks, so the answer is a 400 rather
	// than a query with a hundred correlated subqueries in it.
	t.Run("nesting is bounded", func(t *testing.T) {
		deep := model.FixtureFilter{Equals: &model.FixtureFilterEquals{
			HomeTeam: &model.TeamFilterEquals{
				Players: &model.PlayerFilterEquals{Teams: &model.TeamFilterEquals{
					Players: &model.PlayerFilterEquals{},
				}},
			},
		}}

		_, _, err := w.repos.Fixtures.List(w.ctx, deep, model.FixturePage{})
		if err == nil {
			t.Fatal("a filter nested past the limit should be refused")
		}
		if code := rigerr.CodeOf(err); code != rigerr.CodeUnprocessableEntity {
			t.Errorf("the client should be told it asked for too much, got %s: %v", code, err)
		}
	})

	// A relation condition narrows; it does not multiply. A join to a has-many
	// would return one row per match and report a total to go with it, so a
	// squad with three fixtures would appear three times on the first page.
	t.Run("a total counts rows and not matches", func(t *testing.T) {
		many := w.team(t, "Wanderers")
		for range 3 {
			w.fixture(t, many.ID, athletic.ID)
		}

		rows, total, err := w.repos.Teams.List(w.ctx, model.TeamFilter{
			Equals: &model.TeamFilterEquals{
				Name:         ptr("Wanderers"),
				HomeFixtures: &model.FixtureFilterEquals{},
			},
		}, model.TeamPage{})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || total != 1 {
			t.Errorf("one squad with three fixtures is one row: %d rows, total %d", len(rows), total)
		}
	})
}

// Absence is the question no operator on a related row can ask: an operator is
// always about some related row, so a row with none never matches one — not even
// a negated one. NOT EXISTS is the anti-join, and it is what `without` renders.
func TestFilteringOnTheAbsenceOfARelation(t *testing.T) {
	w := newWorld(t)

	lonely := w.team(t, "Hermits")
	social := w.team(t, "Rovers")
	w.pick(t, social.ID, w.player(t, "Iker", model.PlayerPositionGoalkeeper).ID)

	t.Run("no related row at all", func(t *testing.T) {
		got := w.teamsWhere(t, model.TeamFilter{
			Without: &model.TeamFilterWithout{Players: &model.PlayerFilter{}},
		})
		if len(got) != 1 || got[0].ID != lonely.ID {
			t.Errorf("only Hermits has picked nobody: %d rows", len(got))
		}
	})

	t.Run("no related row matching", func(t *testing.T) {
		// Hermits has no players at all, so it has no goalkeeper either. That is
		// the half an operator condition cannot express: notEquals on the
		// position asks for a player who is not a goalkeeper, and a squad with
		// no players has none of those to offer.
		got := w.teamsWhere(t, model.TeamFilter{
			Without: &model.TeamFilterWithout{Players: &model.PlayerFilter{
				Equals: &model.PlayerFilterEquals{Position: ptr(model.PlayerPositionGoalkeeper)},
			}},
		})
		if len(got) != 1 || got[0].ID != lonely.ID {
			t.Errorf("Rovers has a goalkeeper and Hermits has nobody: %d rows", len(got))
		}
	})

	// The optional belongs-to, which is where an inner join and an anti-join
	// disagree most visibly. A fixture pointing at another tenant's squad is,
	// as far as this tenant can tell, a fixture with no home team — so it
	// satisfies every "without" condition, including this one.
	t.Run("a relation the caller cannot see counts as absent", func(t *testing.T) {
		other := newWorld(t)
		crossed := w.crossedFixture(t, other.team(t, "Barcelona").ID, social.ID)

		got := w.fixturesWhere(t, model.FixtureFilter{
			Without: &model.FixtureFilterWithout{HomeTeam: &model.TeamFilter{
				Equals: &model.TeamFilterEquals{Name: ptr("Barcelona")},
			}},
		})

		var found bool
		for _, f := range got {
			if f.ID == crossed.ID {
				found = true
			}
		}
		if !found {
			t.Error("the other tenant's squad is not a home team this caller has, " +
				"so the fixture is without one")
		}
	})

	// An ordinary negation, for contrast: it asks about a related row that is
	// there, which is a different question and still worth being able to ask.
	t.Run("a negated operator still requires the relation", func(t *testing.T) {
		got := w.teamsWhere(t, model.TeamFilter{
			NotEquals: &model.TeamFilterEquals{
				Players: &model.PlayerFilterEquals{Position: ptr(model.PlayerPositionGoalkeeper)},
			},
		})
		for _, team := range got {
			if team.ID == lonely.ID {
				t.Error("Hermits has no player who is not a goalkeeper, because it has no players")
			}
		}
	})
}

// Ordering across a relation is the one place rig emits a join, and it has to be
// a left one: the join is there to reach a value, not to decide anything.
func TestOrderingByARelatedColumn(t *testing.T) {
	w := newWorld(t)

	var (
		zulu    = w.team(t, "Zulu Wanderers")
		alpha   = w.team(t, "Alpha Rovers")
		neutral = w.team(t, "Neutral")
	)
	late := w.fixture(t, zulu.ID, neutral.ID)
	early := w.fixture(t, alpha.ID, neutral.ID)

	t.Run("a list can be ordered by the related row's column", func(t *testing.T) {
		rows, _, err := w.repos.Fixtures.List(w.ctx, model.FixtureFilter{},
			model.FixturePage{OrderBy: []model.FixtureOrder{
				model.FixtureOrderHomeTeam(model.TeamOrderNameAsc),
			}})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) < 2 || rows[0].ID != early.ID || rows[1].ID != late.ID {
			t.Errorf("Alpha Rovers should come before Zulu Wanderers, got %d rows", len(rows))
		}

		// And the other way, so the ordering is doing the work rather than the
		// insertion order.
		rows, _, err = w.repos.Fixtures.List(w.ctx, model.FixtureFilter{},
			model.FixturePage{OrderBy: []model.FixtureOrder{
				model.FixtureOrderHomeTeam(model.TeamOrderNameDesc),
			}})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) < 2 || rows[0].ID != late.ID {
			t.Error("descending should put Zulu Wanderers first")
		}
	})

	// The reason this is a LEFT JOIN. An inner one would drop the row whose
	// related squad this caller cannot see, so asking for an order would have
	// quietly shortened the list — and nothing in the response would say so.
	t.Run("a row whose related row is missing is kept", func(t *testing.T) {
		other := newWorld(t)
		crossed := w.crossedFixture(t, other.team(t, "Invisible").ID, neutral.ID)

		unordered, total, err := w.repos.Fixtures.List(w.ctx, model.FixtureFilter{}, model.FixturePage{})
		if err != nil {
			t.Fatal(err)
		}

		rows, orderedTotal, err := w.repos.Fixtures.List(w.ctx, model.FixtureFilter{},
			model.FixturePage{OrderBy: []model.FixtureOrder{
				model.FixtureOrderHomeTeam(model.TeamOrderNameAsc),
			}})
		if err != nil {
			t.Fatal(err)
		}

		if len(rows) != len(unordered) || orderedTotal != total {
			t.Errorf("ordering changed the result set: %d of %d rows, was %d of %d",
				len(rows), orderedTotal, len(unordered), total)
		}

		var found bool
		for _, f := range rows {
			if f.ID == crossed.ID {
				found = true
			}
		}
		if !found {
			t.Error("the fixture whose home team is invisible should still be listed")
		}
	})

	t.Run("ordering and filtering across relations compose", func(t *testing.T) {
		rows, _, err := w.repos.Fixtures.List(w.ctx, model.FixtureFilter{
			Equals: &model.FixtureFilterEquals{AwayTeam: named("Neutral")},
		}, model.FixturePage{OrderBy: []model.FixtureOrder{
			model.FixtureOrderHomeTeam(model.TeamOrderNameAsc),
		}})
		if err != nil {
			t.Fatal(err)
		}
		// The subquery's alias and the join's are separate spaces, so the
		// condition is answered against team r1 and the ordering reads team o1.
		if len(rows) < 2 || rows[0].ID != early.ID {
			t.Errorf("the filter and the ordering should both apply: %d rows", len(rows))
		}
	})

	t.Run("a column the relation does not have is refused", func(t *testing.T) {
		_, _, err := w.repos.Fixtures.List(w.ctx, model.FixtureFilter{},
			model.FixturePage{OrderBy: []model.FixtureOrder{
				{Relation: "HomeTeam", Column: "drop table team"},
			}})
		if err == nil {
			t.Fatal("an unknown column should be refused rather than pasted into the statement")
		}
		if code := rigerr.CodeOf(err); code != rigerr.CodeUnprocessableEntity {
			t.Errorf("the caller should be told what is wrong, got %s: %v", code, err)
		}
	})

	t.Run("a relation that cannot be ordered through is refused", func(t *testing.T) {
		// Players is a many-to-many: ordering by a column of a table with many
		// rows per row of this one needs an aggregate, not a join.
		_, _, err := w.repos.Teams.List(w.ctx, model.TeamFilter{},
			model.TeamPage{OrderBy: []model.TeamOrder{
				{Relation: "Players", Column: "full_name"},
			}})
		if err == nil {
			t.Fatal("ordering through a has-many should be refused")
		}
	})
}

// world is one tenant's view of the database.
type world struct {
	repos *store.Store
	ctx   context.Context
	pool  *pgxpool.Pool
	// claims are also on the context. They are kept here for the one helper that
	// writes a row the repository would refuse and so has to stamp the tenant
	// itself.
	claims tenancy.Claims
}

func newWorld(t *testing.T) *world {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://rig:rig@localhost:55441/rig?sslmode=disable"
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("no database at %s: %v — run `rig db up` first", dsn, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("no database at %s: %v — run `rig db up` first", dsn, err)
	}
	t.Cleanup(pool.Close)

	// A fresh tenant per world, so one run's rows are invisible to the next.
	claims := tenancy.Claims{TenantID: uuid.New(), AccountID: uuid.New()}
	return &world{
		repos:  store.New(pool, store.Config{}),
		ctx:    tenancy.NewContext(ctx, claims),
		pool:   pool,
		claims: claims,
	}
}

func (w *world) team(t *testing.T, name string) *model.Team {
	t.Helper()
	row, err := w.repos.Teams.Create(w.ctx, dbhook.Create[model.TeamCreateInput, model.Team]{
		Input: model.TeamCreateInput{Name: name, IsActive: true},
	})
	if err != nil {
		t.Fatalf("create team %s: %v", name, err)
	}
	return row
}

func (w *world) player(t *testing.T, name string, pos model.PlayerPosition) *model.Player {
	t.Helper()
	row, err := w.repos.Players.Create(w.ctx, dbhook.Create[model.PlayerCreateInput, model.Player]{
		Input: model.PlayerCreateInput{FullName: name, Position: pos},
	})
	if err != nil {
		t.Fatalf("create player %s: %v", name, err)
	}
	return row
}

// numbered is player with a shirt number, for the conditions that need two
// facts about the same one.
func (w *world) numbered(t *testing.T, name string, pos model.PlayerPosition, shirt int) *model.Player {
	t.Helper()
	row, err := w.repos.Players.Create(w.ctx, dbhook.Create[model.PlayerCreateInput, model.Player]{
		Input: model.PlayerCreateInput{FullName: name, Position: pos, ShirtNumber: &shirt},
	})
	if err != nil {
		t.Fatalf("create player %s: %v", name, err)
	}
	return row
}

func (w *world) fixture(t *testing.T, home, away uuid.UUID) *model.Fixture {
	t.Helper()
	row, err := w.repos.Fixtures.Create(w.ctx, dbhook.Create[model.FixtureCreateInput, model.Fixture]{
		Input: model.FixtureCreateInput{
			HomeTeamID: home, AwayTeamID: away,
			KickoffAt: time.Now().Add(24 * time.Hour).Truncate(time.Second),
		},
	})
	if err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	return row
}

// crossedFixture writes a fixture pointing at a team in another tenant.
//
// It has to use SQL, because the repository refuses to write this row: a
// generated create checks every foreign key against the same scope the target's
// own reads are built from, and a team in another tenant is not in it. That
// refusal is the point, and it is asserted directly in
// TestAReferenceMustBeOneTheCallerCouldRead.
//
// The state is still worth constructing, because the repository is not the only
// thing that ever wrote to this database. Rows predating the check, a migration,
// a restore, a hand-run UPDATE — the read side has to stay correct in front of
// any of them, and the tests below are what says so. Postgres will take the row
// happily: `references team (id)` says the team exists, not whose it is.
func (w *world) crossedFixture(t *testing.T, home, away uuid.UUID) *model.Fixture {
	t.Helper()

	id := uuid.New()
	_, err := w.pool.Exec(w.ctx,
		`INSERT INTO fixture (id, tenant_id, created_at, created_by_account_id,
		    home_team_id, away_team_id, kickoff_at)
		 VALUES ($1, $2, now(), $3, $4, $5, $6)`,
		id, w.claims.TenantID, w.claims.AccountID, home, away,
		time.Now().Add(24*time.Hour).Truncate(time.Second))
	if err != nil {
		t.Fatalf("write a crossed fixture: %v", err)
	}

	row, err := w.repos.Fixtures.Get(w.ctx, id)
	if err != nil {
		t.Fatalf("read back the crossed fixture: %v", err)
	}
	return row
}

// pick adds a row to the join table.
//
// team_player is a pure join table, so rig read it as a relation rather than a
// resource and there is no repository for it — which is the point of the
// convention, and the reason this one helper uses SQL.
func (w *world) pick(t *testing.T, team, player uuid.UUID) {
	t.Helper()
	_, err := w.pool.Exec(w.ctx,
		"INSERT INTO team_player (team_id, player_id) VALUES ($1, $2)", team, player)
	if err != nil {
		t.Fatalf("pick player: %v", err)
	}
}

func (w *world) fixturesWhere(t *testing.T, f model.FixtureFilter) []*model.Fixture {
	t.Helper()
	rows, _, err := w.repos.Fixtures.List(w.ctx, f, model.FixturePage{})
	if err != nil {
		t.Fatalf("list fixtures: %v", err)
	}
	return rows
}

func (w *world) teamsWhere(t *testing.T, f model.TeamFilter) []*model.Team {
	t.Helper()
	rows, _, err := w.repos.Teams.List(w.ctx, f, model.TeamPage{})
	if err != nil {
		t.Fatalf("list teams: %v", err)
	}
	return rows
}

func named(name string) *model.TeamFilterEquals {
	return &model.TeamFilterEquals{Name: &name}
}

func ptr[T any](v T) *T { return &v }
