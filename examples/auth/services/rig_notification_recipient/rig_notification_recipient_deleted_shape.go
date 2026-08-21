package rig_notification_recipient

import (
	"context"
	"net/http"

	internalelectric "github.com/simonjanss/rig/examples/auth/internal/electric"
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
// Wiring this into Handlers takes the place of the live shape's scope, which
// is what this route uses while the field is nil. Whatever that one adds, add
// here too unless the reason it added it stops applying to these rows —
// otherwise this shape shows more than the live one does.
//
// Unlike the .gen.go files, this one is yours: rig writes it once and never
// touches it again.
func DeletedShape(ctx context.Context, r *http.Request, claims tenancy.Claims, p internalelectric.RigNotificationRecipientShapeParams, w *electric.Where) error {
	// Nothing to add. Delete this function and leave the field nil to keep the
	// live shape's scope on this route.
	return nil
}

// DeletedShape satisfies the generated signature. The check is here so that a
// parameter added to the configuration becomes a compile error rather than a
// value nobody reads.
var _ internalelectric.RigNotificationRecipientDeletedScope = DeletedShape
