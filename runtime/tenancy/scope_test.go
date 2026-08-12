package tenancy_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
)

func TestParseScope(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		raw  string
		want tenancy.Scope
	}{
		{"", tenancy.ScopeOwn},
		{"own", tenancy.ScopeOwn},
		{"all", tenancy.ScopeAll},
	} {
		got, err := tenancy.ParseScope(tc.raw)
		if err != nil {
			t.Fatalf("ParseScope(%q): %v", tc.raw, err)
		}
		if got != tc.want {
			t.Errorf("ParseScope(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// TestParseScopeRefusesNonsense is the reason the function exists. A typo that
// fell back to the narrow default would answer 200 with fewer rows than the caller
// expected, which reads as missing data rather than as a bad request.
func TestParseScopeRefusesNonsense(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"ALL", "everything", "tenant", "own ", "0"} {
		if _, err := tenancy.ParseScope(raw); err == nil {
			t.Errorf("ParseScope(%q) was accepted", raw)
		} else if code := rigerr.CodeOf(err); code != rigerr.CodeBadRequest {
			t.Errorf("ParseScope(%q) returned %s, want %s", raw, code, rigerr.CodeBadRequest)
		}
	}
}

func TestRequireScope(t *testing.T) {
	t.Parallel()

	narrow := tenancy.Claims{
		TenantID:    uuid.New(),
		AccountID:   uuid.New(),
		Subject:     tenancy.SubjectAccount,
		Permissions: []string{"note.read"},
	}
	wide := narrow
	wide.Permissions = []string{"note.read", "note.read.all"}

	// The narrow default needs nothing, held or otherwise.
	if err := tenancy.RequireScope(narrow, tenancy.ScopeOwn, "note.read.all"); err != nil {
		t.Errorf("own with no grant: %v", err)
	}
	if err := tenancy.RequireScope(wide, tenancy.ScopeAll, "note.read.all"); err != nil {
		t.Errorf("all with the grant: %v", err)
	}

	// The whole point: refused, loudly, rather than answered with a smaller set.
	err := tenancy.RequireScope(narrow, tenancy.ScopeAll, "note.read.all")
	if err == nil {
		t.Fatal("all without the grant was permitted")
	}
	if code := rigerr.CodeOf(err); code != rigerr.CodeForbidden {
		t.Errorf("refusal is %s, want %s", code, rigerr.CodeForbidden)
	}
}

// TestRequireScopeIgnoresSubject guards the decision that a key is not
// automatically wide. A person's own API key acts as that person; what makes an
// integration's key wide is the permission it was minted with.
func TestRequireScopeIgnoresSubject(t *testing.T) {
	t.Parallel()

	keyID := uuid.New()
	personalKey := tenancy.Claims{
		TenantID:    uuid.New(),
		AccountID:   uuid.New(),
		Subject:     tenancy.SubjectAPIKey,
		APIKeyID:    &keyID,
		Permissions: []string{"note.read"},
	}
	if err := tenancy.RequireScope(personalKey, tenancy.ScopeAll, "note.read.all"); err == nil {
		t.Error("a key without the permission widened the read")
	}

	human := tenancy.Claims{
		TenantID:    uuid.New(),
		AccountID:   uuid.New(),
		Subject:     tenancy.SubjectAccount,
		Permissions: []string{"note.read", "note.read.all"},
	}
	if err := tenancy.RequireScope(human, tenancy.ScopeAll, "note.read.all"); err != nil {
		t.Errorf("a person holding the permission was refused: %v", err)
	}
}

// TestRequireScopeWithNoKey covers a project that turned permissions off: the
// parameter still works and nothing authorizes it, which is what asking for no
// authorization means.
func TestRequireScopeWithNoKey(t *testing.T) {
	t.Parallel()

	c := tenancy.Claims{TenantID: uuid.New(), AccountID: uuid.New(), Subject: tenancy.SubjectAccount}
	if err := tenancy.RequireScope(c, tenancy.ScopeAll, ""); err != nil {
		t.Errorf("permissions off: %v", err)
	}
}

// Extra is how an application reads its own session context back. The zero cases
// matter more than the happy one: a session issued before anybody set a payload,
// and a request that arrived on an API key, both land on claims with none.
func TestExtra(t *testing.T) {
	t.Parallel()

	type sessionContext struct {
		Device    string `json:"device"`
		SteppedUp bool   `json:"steppedUp"`
	}

	got, err := tenancy.Extra[sessionContext](tenancy.Claims{
		Extra: []byte(`{"device":"laptop","steppedUp":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Device != "laptop" || !got.SteppedUp {
		t.Errorf("Extra = %+v", got)
	}

	// No payload is not an error. Most claims have none.
	empty, err := tenancy.Extra[sessionContext](tenancy.Claims{})
	if err != nil {
		t.Errorf("claims with no payload: %v", err)
	}
	if empty != (sessionContext{}) {
		t.Errorf("want the zero value, got %+v", empty)
	}

	// Something that is not the expected shape is. An application changing its
	// payload type wants to hear about the sessions still carrying the old one.
	if _, err := tenancy.Extra[sessionContext](tenancy.Claims{Extra: []byte(`["a"]`)}); err == nil {
		t.Error("a payload of the wrong shape should be an error")
	}
}
