package rig_notification_recipient

import (
	"context"
	"fmt"
	"net/http"

	internalelectric "github.com/simonjanss/rig/examples/linearlite/internal/electric"
	"github.com/simonjanss/rig/examples/linearlite/internal/model"
	"github.com/simonjanss/rig/examples/linearlite/internal/store"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// snapshotLimit is how much of an inbox a fallback will send, and it is the
// repository's own ceiling for the reason services/todo/todo_fallback.go gives:
// a larger number is clamped to it without saying so.
const snapshotLimit = store.MaxLimit

// Fallback answers the inbox shape from this application's own read while the
// sync service cannot be reached, so the bell keeps its lines through an outage.
//
// **The scoping is the whole question here, and it is worth being explicit
// about why a plain List is right.** This shape is narrowed further than the
// board's: the generated handler adds `account_id = <the caller>` on top of the
// tenant, because an inbox line belongs to one person. A fallback that listed
// the tenant would hand a subscriber everybody else's inbox — and only while
// something else was broken, which is the worst time to find out.
//
// It does not, and the reason is not this file being careful. `access: {scope:
// own, owner: account_id}` means the *repository* narrows on the account too, from
// the same claims, before anything here runs — so `List` with an empty filter
// produces tenant + account + not-deleted, which is exactly the three conditions
// the shape's own filter carries. The two agree because they are derived from the
// same declaration, not because they were written to match.
//
// Anything that changes that declaration has to change this read. Nothing checks
// it.
func Fallback(repo store.RigNotificationRecipientRepository) internalelectric.RigNotificationRecipientFallback {
	return func(ctx context.Context, _ *http.Request, _ tenancy.Claims, _ internalelectric.RigNotificationRecipientShapeParams) ([]*model.RigNotificationRecipient, error) {
		rows, total, err := repo.List(ctx, model.RigNotificationRecipientFilter{}, model.RigNotificationRecipientPage{Limit: snapshotLimit})
		if err != nil {
			return nil, err
		}
		// The count, for the reason the board's fallback checks it: a subscriber
		// cannot tell a first page from a whole collection, so an inbox this read
		// cannot answer whole is a refusal instead.
		if total > int64(len(rows)) {
			return nil, fmt.Errorf("the inbox holds %d lines, past the %d a snapshot may send", total, snapshotLimit)
		}
		return rows, nil
	}
}
