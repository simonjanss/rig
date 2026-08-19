package tenancy_test

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/runtime/tenancy"
)

var (
	tenantID  = uuid.MustParse("018f2c9e-4a1b-7c3d-9e5f-000000000001")
	accountID = uuid.MustParse("018f2c9e-4a1b-7c3d-9e5f-000000000002")
)

// Every generated handler asks this before it does anything, so a missing
// permission is a refusal rather than a query that quietly runs. The message
// names the permission, because a caller who legitimately lacks it needs to know
// what to ask their administrator for.
func ExampleRequire() {
	c := tenancy.Claims{
		TenantID:    tenantID,
		AccountID:   accountID,
		Permissions: []string{"todo:read"},
	}

	fmt.Println(tenancy.Require(c, "todo:read"))
	fmt.Println(tenancy.Require(c, "todo:delete"))
	fmt.Println(tenancy.Require(tenancy.Claims{}, "todo:read"))

	// Output:
	// <nil>
	// Forbidden: this action requires the "todo:delete" permission
	// Unauthorized: this request is not authenticated
}

// An endpoint with no permission needs only a valid caller — which is what
// `public:` produces, and what a project that turned derivation off has.
func ExampleRequire_noPermission() {
	c := tenancy.Claims{TenantID: tenantID, AccountID: accountID}

	fmt.Println(tenancy.Require(c, ""))

	// Output:
	// <nil>
}

// Extra reads the application's own session context out of the claims. It is a
// function rather than a type parameter on Claims, so generated code can name
// tenancy.Claims everywhere without threading a type through it.
func ExampleExtra() {
	type sessionContext struct {
		Region string `json:"region"`
		Plan   string `json:"plan"`
	}

	payload, _ := json.Marshal(sessionContext{Region: "eu-north-1", Plan: "team"})
	c := tenancy.Claims{TenantID: tenantID, Extra: payload}

	ctx, err := tenancy.Extra[sessionContext](c)
	fmt.Println(ctx.Region, ctx.Plan, err)

	// A caller with no payload gets the zero value and no error — an API key, or
	// a session issued before the application started setting one.
	empty, err := tenancy.Extra[sessionContext](tenancy.Claims{TenantID: tenantID})
	fmt.Printf("%+v %v\n", empty, err)

	// Output:
	// eu-north-1 team <nil>
	// {Region: Plan:} <nil>
}

// Claims carrying no tenant cannot scope a query, and running one anyway would
// return every tenant's rows. That is why a repository takes claims as an
// argument rather than reading them from somewhere optional.
func ExampleClaims_Valid() {
	fmt.Println(tenancy.Claims{TenantID: tenantID}.Valid())
	fmt.Println(tenancy.Claims{AccountID: accountID}.Valid())

	// Output:
	// true
	// false
}
