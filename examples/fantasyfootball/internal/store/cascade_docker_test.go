//go:build docker

package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/simonjanss/rig/examples/fantasyfootball/internal/model"
	"github.com/simonjanss/rig/runtime/dbhook"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// A child gets a function call when its parent is deleted, inside the same
// transaction, and can refuse it.
//
// Every case here is about the transaction rather than about the call: what a
// refusal leaves behind, what a rollback takes with it, and what a child that
// read the input did differently. A hook that ran after the commit could get
// none of them right, which is the whole reason this runs where it does.
//
// The hooks are handed to the repository directly, which is what the generated
// Link does through the writer. Driving it from this side is what lets one test
// hold both ends of an edge at once.
func TestAParentDeleteReachesItsChildren(t *testing.T) {
	w := newWorld(t)

	type deleteHooks = dbhook.DeleteHooks[model.TeamDeleteInput, model.Team]
	type childDelete = dbhook.ChildDelete[model.TeamDeleteInput, model.Team]

	// The child refuses, and the parent is still there. The refusal is an error
	// from inside the transaction, so nothing about the delete happened.
	t.Run("a child can refuse the delete", func(t *testing.T) {
		rovers := w.team(t, "Rovers")
		athletic := w.team(t, "Athletic")
		w.fixture(t, rovers.ID, athletic.ID)

		refused := rigerr.Conflict("the season is not over")
		err := w.repos.Teams.Delete(w.ctx, dbhook.Delete[model.TeamDeleteInput, model.Team]{
			Input: model.TeamDeleteInput{ID: rovers.ID, Hard: true},
			Hooks: deleteHooks{Children: []childDelete{{
				Child: "fixture",
				Deleting: func(context.Context, tenancy.Claims, *model.Team, model.TeamDeleteInput) error {
					return refused
				},
			}}},
		})
		if !errors.Is(err, refused) {
			t.Fatalf("err = %v, want the child's own refusal", err)
		}
		// Named, because the whole reason this beats a bare 23503 is that the
		// answer can say which relation refused.
		if !strings.Contains(err.Error(), "fixture") {
			t.Errorf("the error should name the relation: %v", err)
		}
		if _, err := w.repos.Teams.Get(w.ctx, rovers.ID); err != nil {
			t.Errorf("a refused delete leaves the parent where it was: %v", err)
		}
	})

	// The child clears its rows and the delete then succeeds — a hard delete the
	// foreign key would otherwise refuse with 23503.
	t.Run("a child that clears its rows unblocks a hard delete", func(t *testing.T) {
		rovers := w.team(t, "Rovers II")
		athletic := w.team(t, "Athletic II")
		derby := w.fixture(t, rovers.ID, athletic.ID)

		var sawHard bool
		err := w.repos.Teams.Delete(w.ctx, dbhook.Delete[model.TeamDeleteInput, model.Team]{
			Input: model.TeamDeleteInput{ID: rovers.ID, Hard: true},
			Hooks: deleteHooks{Children: []childDelete{{
				Child: "fixture",
				Deleting: func(ctx context.Context, _ tenancy.Claims, _ *model.Team, in model.TeamDeleteInput) error {
					// The input is passed for exactly this. A child that nulled
					// a link on a soft delete would have destroyed the only
					// record of what to re-link on a restore.
					sawHard = in.Hard
					return w.repos.Fixtures.Delete(ctx,
						dbhook.Delete[model.FixtureDeleteInput, model.Fixture]{
							Input: model.FixtureDeleteInput{ID: derby.ID},
						})
				},
			}}},
		})
		if err != nil {
			t.Fatalf("the delete should succeed once the child is gone: %v", err)
		}
		if !sawHard {
			t.Error("the child should see that this was a hard delete")
		}
		if _, err := w.repos.Teams.Get(w.ctx, rovers.ID); !rigerr.Is(err, rigerr.CodeNotFound) {
			t.Errorf("the parent should be gone: %v", err)
		}
		if _, err := w.repos.Fixtures.Get(w.ctx, derby.ID); !rigerr.Is(err, rigerr.CodeNotFound) {
			t.Errorf("the child should be gone: %v", err)
		}
	})

	// The child's write joined the parent's transaction, so a later refusal
	// unwinds it. This is the case a hook running after the commit gets wrong.
	t.Run("a later refusal rolls back what an earlier child did", func(t *testing.T) {
		rovers := w.team(t, "Rovers III")
		athletic := w.team(t, "Athletic III")
		derby := w.fixture(t, rovers.ID, athletic.ID)

		boom := errors.New("no")
		err := w.repos.Teams.Delete(w.ctx, dbhook.Delete[model.TeamDeleteInput, model.Team]{
			Input: model.TeamDeleteInput{ID: rovers.ID, Hard: true},
			Hooks: deleteHooks{Children: []childDelete{
				{
					Child: "fixture",
					Deleting: func(ctx context.Context, _ tenancy.Claims, _ *model.Team, _ model.TeamDeleteInput) error {
						return w.repos.Fixtures.Delete(ctx,
							dbhook.Delete[model.FixtureDeleteInput, model.Fixture]{
								Input: model.FixtureDeleteInput{ID: derby.ID},
							})
					},
				},
				{
					Child: "player",
					Deleting: func(context.Context, tenancy.Claims, *model.Team, model.TeamDeleteInput) error {
						return boom
					},
				},
			}},
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the second child's error", err)
		}
		if _, err := w.repos.Fixtures.Get(w.ctx, derby.ID); err != nil {
			t.Errorf("the first child's delete should have been rolled back: %v", err)
		}
	})

	// Five steps, and the two halves land on either side of the commit.
	t.Run("the deleted half runs after the commit, in the same order", func(t *testing.T) {
		spare := w.team(t, "Spare")

		var order []string
		err := w.repos.Teams.Delete(w.ctx, dbhook.Delete[model.TeamDeleteInput, model.Team]{
			Input: model.TeamDeleteInput{ID: spare.ID, Hard: true},
			Hooks: deleteHooks{
				Before: func(context.Context, tenancy.Claims, *model.TeamDeleteInput, *model.Team) error {
					order = append(order, "before")
					return nil
				},
				After: func(context.Context, tenancy.Claims, *model.Team) error {
					order = append(order, "after")
					return nil
				},
				Children: []childDelete{
					{
						Child: "fixture",
						Deleting: func(context.Context, tenancy.Claims, *model.Team, model.TeamDeleteInput) error {
							order = append(order, "deleting fixture")
							return nil
						},
						Deleted: func(context.Context, tenancy.Claims, *model.Team, model.TeamDeleteInput) {
							order = append(order, "deleted fixture")
						},
					},
					{
						Child: "player",
						Deleted: func(context.Context, tenancy.Claims, *model.Team, model.TeamDeleteInput) {
							order = append(order, "deleted player")
						},
					},
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}

		// The parent's own veto first: the cheapest and most specific rule
		// should not have to wait for every child's cleanup to get to say no.
		want := "before,deleting fixture,after,deleted fixture,deleted player"
		if got := strings.Join(order, ","); got != want {
			t.Errorf("order = %s\nwant  = %s", got, want)
		}
	})

	// The regression test for every project that does not want this feature: no
	// hooks at all, and the foreign key answers the way it always has.
	t.Run("no hooks leaves the 23503 answering a conflict", func(t *testing.T) {
		rovers := w.team(t, "Rovers IV")
		athletic := w.team(t, "Athletic IV")
		w.fixture(t, rovers.ID, athletic.ID)

		err := w.repos.Teams.Delete(w.ctx, dbhook.Delete[model.TeamDeleteInput, model.Team]{
			Input: model.TeamDeleteInput{ID: rovers.ID, Hard: true},
		})
		if !rigerr.Is(err, rigerr.CodeConflict) {
			t.Errorf("err = %v, want a conflict from the foreign key", err)
		}
	})

	// A child that deletes its own rows triggers their children the same way, so
	// depth is the call stack — and a table that reaches itself would exhaust
	// it. The visited set is what makes that terminate: the second visit to a
	// row already going in this transaction is a no-op, not a loop.
	t.Run("a cycle terminates instead of exhausting the stack", func(t *testing.T) {
		rovers := w.team(t, "Rovers V")

		var visits int
		var hooks deleteHooks
		hooks = deleteHooks{Children: []childDelete{{
			Child: "fixture",
			Deleting: func(ctx context.Context, _ tenancy.Claims, parent *model.Team, _ model.TeamDeleteInput) error {
				visits++
				// Straight back to the row being deleted, which is what a
				// cycle in the schema would produce one hop further out.
				return w.repos.Teams.Delete(ctx, dbhook.Delete[model.TeamDeleteInput, model.Team]{
					Input: model.TeamDeleteInput{ID: parent.ID, Hard: true},
					Hooks: hooks,
				})
			},
		}}}

		err := w.repos.Teams.Delete(w.ctx, dbhook.Delete[model.TeamDeleteInput, model.Team]{
			Input: model.TeamDeleteInput{ID: rovers.ID, Hard: true},
			Hooks: hooks,
		})
		if err != nil {
			t.Fatalf("a cycle should stop, not fail: %v", err)
		}
		if visits != 1 {
			t.Errorf("the hook ran %d times; the second visit to the same row is a no-op", visits)
		}
	})
}
