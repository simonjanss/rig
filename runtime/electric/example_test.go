package electric_test

import (
	"fmt"

	"github.com/simonjanss/rig/runtime/electric"
)

// A shape's filter is assembled here and sent to the sync service as a
// parameterized WHERE. The zero value is ready to use, which is what a
// generated scoping function returns before adding the application's own
// conditions to it.
//
// Column names are quoted and values are numbered parameters — never
// interpolated. A shape's filter is built from a tenant identifier and whatever
// an application's scoping function adds, and interpolating either would make a
// live-sync endpoint an injection point with a streaming response attached.
func ExampleWhere() {
	w := &electric.Where{}
	w.Eq("tenant_id", "018f2c9e-4a1b-7c3d-9e5f-000000000001")
	w.IsNull("deleted_at")
	w.In("status", "draft", "review")

	fmt.Println(w.SQL())
	fmt.Println(w.Params())

	// Output:
	// "tenant_id" = $1 AND "deleted_at" IS NULL AND "status" IN ($2, $3)
	// [018f2c9e-4a1b-7c3d-9e5f-000000000001 draft review]
}

// An empty set adds `false`, because "in nothing" matches nothing. Omitting the
// condition instead would widen the shape to everything — the opposite of what
// the caller asked for, on an endpoint that streams.
func ExampleWhere_In_empty() {
	w := &electric.Where{}
	w.Eq("tenant_id", "t-1")
	w.In("status")

	fmt.Println(w.SQL())
	fmt.Println(w.Params())

	// Output:
	// "tenant_id" = $1 AND (false)
	// [t-1]
}
