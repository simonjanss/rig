//go:build docker

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/examples/fantasyfootball/internal/model"
	"github.com/simonjanss/rig/runtime/dbhook"
	"github.com/simonjanss/rig/runtime/patch"
	"github.com/simonjanss/rig/runtime/rigerr"
)

// A generated write may not reference a row the caller could not have read.
//
// Fixture is the example that has the shape: two foreign keys to team, both
// NOT NULL, both writable on create and on update, and a team the caller cannot
// see is one of a tenant they are not in or one already in the trash.
//
// The suite next door tests the read side of the same fact — that a filter
// cannot look through a relation at a row it may not see. This is the write
// side, and until it existed a caller could put the row there in the first
// place.
func TestAReferenceMustBeOneTheCallerCouldRead(t *testing.T) {
	w := newWorld(t)
	ours := w.team(t, "Ours")

	kickoff := func() time.Time { return time.Now().Add(24 * time.Hour).Truncate(time.Second) }

	create := func(home, away uuid.UUID) error {
		_, err := w.repos.Fixtures.Create(w.ctx, dbhook.Create[model.FixtureCreateInput, model.Fixture]{
			Input: model.FixtureCreateInput{HomeTeamID: home, AwayTeamID: away, KickoffAt: kickoff()},
		})
		return err
	}

	// The field error the failure has to arrive as, whichever way it failed.
	refusedHomeTeam := func(t *testing.T, err error) *model.FixtureCreateInputError {
		t.Helper()
		if err == nil {
			t.Fatal("the write should have been refused")
		}
		var fields *model.FixtureCreateInputError
		if !errors.As(err, &fields) {
			t.Fatalf("want a field error naming homeTeamId, got %T: %v", err, err)
		}
		if fields.HomeTeamID == nil {
			t.Fatalf("the error names no field: %v", err)
		}
		if got := fields.HomeTeamID.Code; got != rigerr.FieldCodeNotFound {
			t.Errorf("field code = %q, want %q", got, rigerr.FieldCodeNotFound)
		}
		// A 422 about the input, not a 403 about the caller. The caller is
		// perfectly entitled to create fixtures; the identifier they sent is the
		// thing that is wrong with the request.
		if got := rigerr.StatusOf(err); got != 422 {
			t.Errorf("status = %d, want 422", got)
		}
		return fields
	}

	t.Run("a team in another tenant cannot be referenced", func(t *testing.T) {
		theirs := newWorld(t).team(t, "Theirs")

		err := create(theirs.ID, ours.ID)
		refusedHomeTeam(t, err)
	})

	t.Run("a team the caller does own can be", func(t *testing.T) {
		also := w.team(t, "Also Ours")
		if err := create(also.ID, ours.ID); err != nil {
			t.Errorf("both teams are this tenant's: %v", err)
		}
	})

	// The security property, stated as an assertion rather than as a comment.
	// If these two messages ever diverge, a caller can tell "that row is not
	// yours" from "that row does not exist" — which is exactly what an
	// invisible row is trying not to say.
	t.Run("a hidden team is refused in the same words as a missing one", func(t *testing.T) {
		theirs := newWorld(t).team(t, "Theirs")

		hidden := refusedHomeTeam(t, create(theirs.ID, ours.ID))
		missing := refusedHomeTeam(t, create(uuid.New(), ours.ID))

		// The identifiers differ, so compare the shape around them.
		if hidden.HomeTeamID.Code != missing.HomeTeamID.Code {
			t.Errorf("codes differ: hidden %q, missing %q",
				hidden.HomeTeamID.Code, missing.HomeTeamID.Code)
		}
		wantHidden := "no Team with id " + theirs.ID.String()
		if hidden.HomeTeamID.Message != wantHidden {
			t.Errorf("hidden message = %q, want %q", hidden.HomeTeamID.Message, wantHidden)
		}
	})

	// A row in the trash is not a row to point new ones at: the sweeper is
	// coming for it, and the reference would outlive it.
	t.Run("a deleted team cannot be referenced", func(t *testing.T) {
		retired := w.team(t, "Retired")
		if err := w.repos.Teams.Delete(w.ctx, dbhook.Delete[model.TeamDeleteInput, model.Team]{
			Input: model.TeamDeleteInput{ID: retired.ID},
		}); err != nil {
			t.Fatal(err)
		}

		refusedHomeTeam(t, create(retired.ID, ours.ID))
	})

	t.Run("an update is checked on the keys it sends", func(t *testing.T) {
		theirs := newWorld(t).team(t, "Theirs")
		fixture := w.fixture(t, ours.ID, ours.ID)

		_, err := w.repos.Fixtures.Update(w.ctx, fixture.ID,
			dbhook.Update[model.FixtureUpdateInput, model.Fixture]{
				Input: model.FixtureUpdateInput{HomeTeamID: patch.NewOptional(theirs.ID)},
			})
		if err == nil {
			t.Fatal("an update naming another tenant's team should be refused")
		}
		var fields *model.FixtureUpdateInputError
		if !errors.As(err, &fields) || fields.HomeTeamID == nil {
			t.Fatalf("want a field error naming homeTeamId, got %T: %v", err, err)
		}

		// And an update that does not mention a key is not asked about one. The
		// alternative — re-checking whatever the row already held — would make
		// changing the kickoff time fail because a team moved out of scope
		// afterwards.
		later := kickoff().Add(time.Hour)
		if _, err := w.repos.Fixtures.Update(w.ctx, fixture.ID,
			dbhook.Update[model.FixtureUpdateInput, model.Fixture]{
				Input: model.FixtureUpdateInput{KickoffAt: patch.NewOptional(later)},
			}); err != nil {
			t.Errorf("an update touching no key should not be checked for one: %v", err)
		}
	})
}
