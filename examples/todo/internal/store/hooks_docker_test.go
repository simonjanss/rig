//go:build docker

package store_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/examples/todo/internal/model"
	"github.com/simonjanss/rig/examples/todo/internal/store"
	"github.com/simonjanss/rig/runtime/dbhook"
	"github.com/simonjanss/rig/runtime/patch"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// The hooks are the reason a write takes an envelope rather than an input, and
// what makes them worth having is that they are part of the transaction. This
// suite is about that: what they see, what they can stop, and when the last of
// them runs.
func TestHooks(t *testing.T) {
	repo, ctx := newRepo(t)

	t.Run("create hooks bracket the insert", func(t *testing.T) {
		var (
			sawBefore  *model.TodoCreateInput
			sawAfter   *model.Todo
			landed     *model.Todo
			afterFired bool
		)

		created, err := repo.Create(ctx, dbhook.Create[model.TodoCreateInput, model.Todo]{
			Input: model.TodoCreateInput{Title: "Water the plants"},
			Hooks: dbhook.CreateHooks[model.TodoCreateInput, model.Todo]{
				Before: func(_ context.Context, _ tenancy.Claims, in *model.TodoCreateInput) error {
					sawBefore = in
					// What Before writes is what gets inserted.
					in.Title = "Water the plants twice"
					return nil
				},
				After: func(_ context.Context, _ tenancy.Claims, row *model.Todo) error {
					sawAfter = row
					return nil
				},
				AfterCommit: func(_ context.Context, _ tenancy.Claims, row *model.Todo) {
					landed, afterFired = row, true
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}

		if sawBefore == nil {
			t.Error("Before should have run")
		}
		if created.Title != "Water the plants twice" {
			t.Errorf("title = %q: what Before sets is what is written", created.Title)
		}
		// After sees the row as stored, including what the database filled in.
		if sawAfter == nil || sawAfter.ID != created.ID || sawAfter.CreatedAt.IsZero() {
			t.Errorf("After should see the stored row: %+v", sawAfter)
		}
		if !afterFired || landed.ID != created.ID {
			t.Error("AfterCommit should have run with the created row")
		}
	})

	t.Run("a failing hook undoes the write", func(t *testing.T) {
		refused := errors.New("not today")
		var announced bool

		_, err := repo.Create(ctx, dbhook.Create[model.TodoCreateInput, model.Todo]{
			Input: model.TodoCreateInput{Title: "Should not survive"},
			Hooks: dbhook.CreateHooks[model.TodoCreateInput, model.Todo]{
				After:       func(context.Context, tenancy.Claims, *model.Todo) error { return refused },
				AfterCommit: func(context.Context, tenancy.Claims, *model.Todo) { announced = true },
			},
		})
		if !errors.Is(err, refused) {
			t.Fatalf("err = %v, want the hook's error", err)
		}
		if announced {
			t.Error("nothing committed, so nothing should have been announced")
		}

		// The row must be gone: After ran after the INSERT, so only the
		// rollback can have removed it.
		rows, _, err := repo.List(ctx, titled("Should not survive"), model.TodoPage{})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 0 {
			t.Errorf("the insert should have been rolled back, found %d rows", len(rows))
		}
	})

	t.Run("update hooks see the row as it was", func(t *testing.T) {
		created, err := repo.Create(ctx, dbhook.Create[model.TodoCreateInput, model.Todo]{
			Input: model.TodoCreateInput{Title: "Before"},
		})
		if err != nil {
			t.Fatal(err)
		}

		var beforeTitle, afterOld, afterNew string

		updated, err := repo.Update(ctx, created.ID, dbhook.Update[model.TodoUpdateInput, model.Todo]{
			Input: model.TodoUpdateInput{Title: patch.NewOptional("After")},
			Hooks: dbhook.UpdateHooks[model.TodoUpdateInput, model.Todo]{
				Before: func(_ context.Context, _ tenancy.Claims, _ *model.TodoUpdateInput, prev *model.Todo) error {
					beforeTitle = prev.Title
					return nil
				},
				After: func(_ context.Context, _ tenancy.Claims, updated, prev *model.Todo) error {
					afterOld, afterNew = prev.Title, updated.Title
					return nil
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}

		if beforeTitle != "Before" {
			t.Errorf("Before saw %q, want the row as it was", beforeTitle)
		}
		if afterOld != "Before" || afterNew != "After" {
			t.Errorf("After saw %q -> %q, want Before -> After", afterOld, afterNew)
		}
		if updated.Title != "After" {
			t.Errorf("title = %q", updated.Title)
		}
	})

	// An update that asks for nothing is not an update, and a hook that fires
	// on it is a notification about a change that did not happen.
	t.Run("an empty update runs no hooks", func(t *testing.T) {
		created, err := repo.Create(ctx, dbhook.Create[model.TodoCreateInput, model.Todo]{
			Input: model.TodoCreateInput{Title: "Untouched"},
		})
		if err != nil {
			t.Fatal(err)
		}

		fired := false
		if _, err := repo.Update(ctx, created.ID, dbhook.Update[model.TodoUpdateInput, model.Todo]{
			Hooks: dbhook.UpdateHooks[model.TodoUpdateInput, model.Todo]{
				After: func(context.Context, tenancy.Claims, *model.Todo, *model.Todo) error {
					fired = true
					return nil
				},
			},
		}); err != nil {
			t.Fatal(err)
		}
		if fired {
			t.Error("nothing changed, so After should not have run")
		}
	})

	// The validator is the same one the service layer wires up; here it is
	// handed to the repository directly.
	t.Run("the validator refuses before anything is written", func(t *testing.T) {
		v := model.TodoCreateValidator{
			Title: func(context.Context, *model.TodoValidatorContext, string) error {
				return rigerr.NewFieldError(rigerr.FieldCodeNotAllowed, "no")
			},
		}

		_, err := repo.Create(ctx, dbhook.Create[model.TodoCreateInput, model.Todo]{
			Input: model.TodoCreateInput{Title: "Refused"},
			Hooks: dbhook.CreateHooks[model.TodoCreateInput, model.Todo]{Validator: v},
		})

		var failure *model.TodoCreateInputError
		if !errors.As(err, &failure) {
			t.Fatalf("err = %v, want the typed failure", err)
		}
		if failure.Title == nil || failure.Title.Code != rigerr.FieldCodeNotAllowed {
			t.Errorf("the failure should land under title with its code: %+v", failure)
		}

		rows, _, err := repo.List(ctx, titled("Refused"), model.TodoPage{})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 0 {
			t.Error("a refused create should not have written anything")
		}
	})
}

// newRepo opens the repository against the example's database, in a tenant of
// its own so this suite neither sees nor disturbs anything else.
func newRepo(t *testing.T) (store.TodoRepository, context.Context) {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://rig:rig@localhost:55440/rig?sslmode=disable"
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

	claims := tenancy.Claims{TenantID: uuid.New(), AccountID: uuid.New()}
	return store.New(pool, store.Config{}).Todos, tenancy.NewContext(ctx, claims)
}

func titled(title string) model.TodoFilter {
	f := model.NewTodoFilter()
	f.Equals = model.NewTodoFilterEquals()
	f.Equals.Title = &title
	return f
}

// A rule that could not be run is not a rule that failed. One is the caller's
// problem and belongs in the 422; the other is ours, and telling a client that
// their title is "connection refused" would be a lie about whose fault it is.
func TestARuleThatCannotRunIsNotTheCallersFault(t *testing.T) {
	repo, ctx := newRepo(t)

	unreachable := errors.New("the naming service is down")
	v := model.TodoCreateValidator{
		Title: func(context.Context, *model.TodoValidatorContext, string) error {
			return unreachable
		},
	}

	_, err := repo.Create(ctx, dbhook.Create[model.TodoCreateInput, model.Todo]{
		Input: model.TodoCreateInput{Title: "Unlucky"},
		Hooks: dbhook.CreateHooks[model.TodoCreateInput, model.Todo]{Validator: v},
	})

	if !errors.Is(err, unreachable) {
		t.Fatalf("err = %v, want the failure that happened", err)
	}

	// Not the typed failure, so it is not a 422 and the client is not told to
	// fix their input.
	var failure *model.TodoCreateInputError
	if errors.As(err, &failure) {
		t.Error("a rule that could not run should not come back as a field error")
	}
	if !strings.Contains(err.Error(), "validate title") {
		t.Errorf("the error should name the rule that could not be run: %v", err)
	}
}
