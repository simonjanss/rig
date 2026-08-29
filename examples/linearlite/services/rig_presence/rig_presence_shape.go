package rig_presence

import (
	"context"
	"net/http"

	"github.com/simonjanss/rig/examples/linearlite/internal/api"
	"github.com/simonjanss/rig/runtime/electric"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// Shape narrows RigPresence's live-sync subscription.
//
// The filter it receives already carries the tenant and the lifecycle
// conditions, and every condition is joined with AND — so this can only ever
// show a subscriber less, never more.
//
// Add conditions through the Where methods rather than as text: they bind
// their values, and a shape filter built by concatenation is an injection
// point with a streaming response attached.
//
// **This is the one scope stub in the repository that is filled in**, and the
// reason is arithmetic rather than taste. A heartbeat is one row changed and
// every subscriber to this shape hears about it, so the cost of presence is the
// fan-out and not the writes: fifty people with two tabs each is about eight
// writes a second and about eight hundred shape messages a second on one
// tenant-wide scope. docs/presence.md carries the table. Narrowing is the only
// thing that moves the second number, and it moves it by roughly the ratio of
// the tenant to the screen.
//
// Both conditions stay optional, because a subscriber that wants the whole
// tenant — a diagnostic page, or a header bar listing everybody signed in —
// should not have to invent a scope to ask for it. web/ asks with a scope and
// no target: one subscription per tab, narrowed to the board.
//
// Unlike the .gen.go files, this one is yours: rig writes it once and never
// touches it again.
func Shape(ctx context.Context, r *http.Request, claims tenancy.Claims, p api.PresenceShapeParams, w *electric.Where) error {
	if p.HasScope {
		w.Eq("scope", p.Scope)
	}
	if p.HasTargetID {
		w.Eq("target_id", p.TargetID.String())
	}
	return nil
}

// Shape satisfies the generated signature. The check is here so that a
// parameter added to the configuration becomes a compile error rather than a
// value nobody reads.
var _ api.PresenceScope = Shape
