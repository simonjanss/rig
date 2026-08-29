//go:build docker

package store_test

import (
	"strings"
	"testing"

	"github.com/simonjanss/rig/examples/fantasyfootball/internal/model"
)

// A fixture points at team twice, so both kinds of join have to keep the two
// apart. An unaliased second reference is not a subtle bug: Postgres refuses the
// statement outright, and the two conditions would answer about one squad.
func TestOneTableReferencedTwice(t *testing.T) {
	w := newWorld(t)

	alpha := w.team(t, "Alpha")
	zulu := w.team(t, "Zulu")
	derby := w.fixture(t, alpha.ID, zulu.ID)
	w.fixture(t, zulu.ID, alpha.ID) // the same two squads, the other way round

	t.Run("ordering by two relations to the same table", func(t *testing.T) {
		rows, total, err := w.repos.Fixtures.List(w.ctx, model.FixtureFilter{},
			model.FixturePage{OrderBy: []model.FixtureOrder{
				model.FixtureOrderHomeTeam(model.TeamOrderNameAsc),
				model.FixtureOrderAwayTeam(model.TeamOrderNameDesc),
			}})
		if err != nil {
			t.Fatalf("two joins to one table: %v", err)
		}
		if total != 2 || len(rows) != 2 {
			t.Fatalf("both fixtures should be listed: %d of %d", len(rows), total)
		}
		// Alpha at home comes first, which is only true if the ordering read the
		// home squad's name rather than whichever team row the join happened to
		// reach.
		if rows[0].ID != derby.ID {
			t.Error("ordering should follow the home team's name")
		}
	})

	t.Run("filtering on two relations to the same table", func(t *testing.T) {
		got := w.fixturesWhere(t, model.FixtureFilter{
			Equals: &model.FixtureFilterEquals{
				HomeTeam: &model.TeamFilterEquals{Name: ptr("Alpha")},
				AwayTeam: &model.TeamFilterEquals{Name: ptr("Zulu")},
			},
		})
		if len(got) != 1 || got[0].ID != derby.ID {
			t.Errorf("only one fixture has Alpha at home and Zulu away: %d rows", len(got))
		}
	})

	t.Run("a relation that comes back to where it started", func(t *testing.T) {
		// team → its home fixtures → their home team, which is team again. The
		// outer table and the inner one are the same table at two depths, and the
		// aliases are what keep the correlation honest.
		got := w.teamsWhere(t, model.TeamFilter{
			Equals: &model.TeamFilterEquals{
				HomeFixtures: &model.FixtureFilterEquals{
					HomeTeam: &model.TeamFilterEquals{Name: ptr("Alpha")},
				},
			},
		})
		if len(got) != 1 || got[0].ID != alpha.ID {
			names := make([]string, 0, len(got))
			for _, team := range got {
				names = append(names, team.Name)
			}
			t.Errorf("only Alpha hosts a fixture whose home team is Alpha: %v",
				strings.Join(names, ", "))
		}
	})
}
