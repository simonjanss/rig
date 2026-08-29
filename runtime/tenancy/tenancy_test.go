package tenancy_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// A repository that silently proceeded with an empty tenant would return every
// tenant's rows, so a missing scope has to be an error rather than a default.
func TestFromContextRequiresClaims(t *testing.T) {
	t.Parallel()

	_, err := tenancy.FromContext(context.Background())
	if err == nil {
		t.Fatal("no claims should be an error, not an empty tenant")
	}
	if !rigerr.Is(err, rigerr.CodeUnauthorized) {
		t.Errorf("code = %q, want Unauthorized", rigerr.CodeOf(err))
	}
}

func TestFromContextRequiresATenant(t *testing.T) {
	t.Parallel()

	ctx := tenancy.NewContext(context.Background(), tenancy.Claims{AccountID: uuid.New()})
	if _, err := tenancy.FromContext(ctx); err == nil {
		t.Fatal("claims with no tenant cannot scope a query")
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	want := tenancy.Claims{
		TenantID:    uuid.New(),
		AccountID:   uuid.New(),
		Subject:     tenancy.SubjectAccount,
		Permissions: []string{"lesson.publish"},
		Roles:       []string{"editor"},
	}

	got, err := tenancy.FromContext(tenancy.NewContext(context.Background(), want))
	if err != nil {
		t.Fatal(err)
	}
	if got.TenantID != want.TenantID || got.AccountID != want.AccountID {
		t.Errorf("claims changed in transit: %+v", got)
	}
	if !got.Can("lesson.publish") || got.Can("lesson.delete") {
		t.Error("Can misread the permissions")
	}
	if !got.HasRole("editor") || got.HasRole("admin") {
		t.Error("HasRole misread the roles")
	}
}

func TestActor(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	if got := (tenancy.Claims{AccountID: id}).Actor(); got == nil || *got != id {
		t.Errorf("Actor = %v, want %v", got, id)
	}

	// rig's own work has a tenant but no person behind it, and the audit
	// column has to record that rather than a zero identifier.
	if got := tenancy.System(uuid.New()).Actor(); got != nil {
		t.Errorf("a system actor is nobody, got %v", got)
	}
}
