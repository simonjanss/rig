package rig_notification_recipient

import (
	"context"
	"net/http"

	internalelectric "github.com/simonjanss/rig/examples/todo/internal/electric"
	"github.com/simonjanss/rig/runtime/electric"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// DeletedShape narrows RigNotificationRecipient's trash: the rows somebody
// retired.
//
// The filter it receives already carries the tenant and the lifecycle
// conditions, and every condition is joined with AND — so this can only ever
// show a subscriber less, never more.
//
// Add conditions through the Where methods rather than as text: they bind
// their values, and a shape filter built by concatenation is an injection
// point with a streaming response attached.
//
// Unlike the .gen.go files, this one is yours: rig writes it once and never
// touches it again.
func DeletedShape(ctx context.Context, r *http.Request, claims tenancy.Claims, p internalelectric.RigNotificationRecipientShapeParams, w *electric.Where) error {
	// Nothing to add. Delete this function and pass nil if it stays that way.
	return nil
}

// DeletedShape satisfies the generated signature. The check is here so that a
// parameter added to the configuration becomes a compile error rather than a
// value nobody reads.
var _ internalelectric.RigNotificationRecipientDeletedScope = DeletedShape
