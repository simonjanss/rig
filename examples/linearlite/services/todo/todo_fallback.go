package todo

import (
	"context"
	"fmt"
	"net/http"

	internalelectric "github.com/simonjanss/rig/examples/linearlite/internal/electric"
	"github.com/simonjanss/rig/examples/linearlite/internal/model"
	"github.com/simonjanss/rig/examples/linearlite/internal/store"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// snapshotLimit is how much of the board a fallback will send.
//
// A shape is not paginated and this read is, so a number has to come from
// somewhere — and the repository's own ceiling is the only honest one, because
// a larger number is clamped to it and nothing says so. Asking for 2000 and
// being given 500 is how this was wrong before.
const snapshotLimit = store.MaxLimit

// Fallback answers the live Todo shape from this application's own read while
// the sync service cannot be reached.
//
// It is the whole demonstration of the feature: the board is the one thing this
// example cannot render without, so a subscriber that gets nothing gets a blank
// page. With this wired, a sync outage costs live updates and not the board.
//
// It is a constructor rather than a plain function because the read needs a
// repository and a shape endpoint has no way to hand it one. main.go calls this
// where it registers the shapes.
//
// The scope this pairs with — Shape, in the file beside this one — adds nothing,
// which is what makes a plain List the right answer here. If it ever narrows the
// shape, this has to narrow the same way: the proxy applies a scope itself and
// cannot apply anything to a read it does not perform.
func Fallback(repo store.TodoRepository) internalelectric.TodoFallback {
	return func(ctx context.Context, _ *http.Request, _ tenancy.Claims, _ internalelectric.TodoShapeParams) ([]*model.Todo, error) {
		// The repository's defaults are the live shape's filter: this tenant,
		// from the claims already on the context, not deleted and not a
		// snapshot. So there is nothing to say here, and nothing that could
		// disagree with the filter the proxy would have sent.
		rows, total, err := repo.List(ctx, model.TodoFilter{}, model.TodoPage{Limit: snapshotLimit})
		if err != nil {
			return nil, err
		}
		// The count, because a page is the one thing that looks the same whether
		// it is the whole answer or the first part of one. Refusing is what the
		// runtime does past its own bound and for the same reason: a subscriber
		// cannot tell a short collection from a table that lost half its rows,
		// so a board this read cannot answer whole is a 502 instead.
		if total > int64(len(rows)) {
			return nil, fmt.Errorf("the board holds %d todos, past the %d a snapshot may send", total, snapshotLimit)
		}
		return rows, nil
	}
}
