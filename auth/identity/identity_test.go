package identity_test

import (
	"context"
	"testing"

	"github.com/simonjanss/rig/auth/identity"
	"github.com/simonjanss/rig/runtime/rigerr"
)

func TestDomainAllowed(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		address string
		domains []string
		want    bool
	}{
		{"ada@example.com", nil, true},
		{"ada@example.com", []string{}, true},
		{"ada@example.com", []string{"example.com"}, true},
		{"ADA@Example.COM", []string{"example.com"}, true},
		{"  ada@example.com  ", []string{"example.com"}, true},
		{"ada@example.com", []string{"@example.com"}, true},
		{"ada@example.com", []string{" example.com "}, true},
		// A listed domain means the company, so a subdomain of it is the
		// company too.
		{"ada@mail.example.com", []string{"example.com"}, true},
		{"ada@example.com", []string{"other.com", "example.com"}, true},

		{"ada@other.com", []string{"example.com"}, false},
		// Not a suffix match on the string: notexample.com is somebody else.
		{"ada@notexample.com", []string{"example.com"}, false},
		{"ada@example.com.evil.test", []string{"example.com"}, false},
		{"not-an-address", []string{"example.com"}, false},
		{"ada@", []string{"example.com"}, false},
		{"ada@example.com", []string{""}, false},
	} {
		if got := identity.DomainAllowed(tc.address, tc.domains); got != tc.want {
			t.Errorf("DomainAllowed(%q, %v) = %v", tc.address, tc.domains, got)
		}
	}
}

func TestAllowDomains(t *testing.T) {
	t.Parallel()

	gate := identity.AllowDomains("example.com")

	for _, tc := range []struct {
		name    string
		in      identity.Candidate
		refused bool
	}{
		{"a stranger at the domain", identity.Candidate{
			EmailAddress: "ada@example.com", Via: identity.SourceProvider}, false},
		{"a stranger elsewhere", identity.Candidate{
			EmailAddress: "ada@other.com", Via: identity.SourceProvider}, true},
		{"a code request elsewhere", identity.Candidate{
			EmailAddress: "ada@other.com", Via: identity.SourceEmailCode}, true},

		// The rule an application actually wants: somebody here typed this
		// address in, so the list is not about them. A deployment that could not
		// invite an auditor or a supply teacher would be one nobody could run.
		{"an invitation elsewhere", identity.Candidate{
			EmailAddress: "ada@other.com", Via: identity.SourceInvitation}, false},
		{"a provision elsewhere", identity.Candidate{
			EmailAddress: "ada@other.com", Via: identity.SourceProvision}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := gate(context.Background(), tc.in)
			if tc.refused && err == nil {
				t.Fatal("allowed")
			}
			if !tc.refused && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if tc.refused && rigerr.CodeOf(err) != rigerr.CodeForbidden {
				t.Errorf("refused with %s, want %s", rigerr.CodeOf(err), rigerr.CodeForbidden)
			}
		})
	}
}

// TestAnEmptyListIsNoGate is worth pinning because it is what an unset rig.yaml
// key generates: a gate that refuses nobody, rather than one that refuses
// everybody.
func TestAnEmptyListIsNoGate(t *testing.T) {
	t.Parallel()

	gate := identity.AllowDomains()
	if err := gate(context.Background(), identity.Candidate{
		EmailAddress: "ada@anywhere.test", Via: identity.SourceProvider,
	}); err != nil {
		t.Fatalf("an empty list refused somebody: %v", err)
	}
}

// TestAllowWithNoGate covers the nil check that exists once so that four call
// sites cannot each forget it.
func TestAllowWithNoGate(t *testing.T) {
	t.Parallel()

	if err := identity.Allow(context.Background(), nil, identity.Candidate{}); err != nil {
		t.Fatalf("no gate refused somebody: %v", err)
	}

	want := rigerr.Forbidden("no")
	got := identity.Allow(context.Background(), func(context.Context, identity.Candidate) error {
		return want
	}, identity.Candidate{})
	if got != want {
		t.Errorf("Allow answered %v, want the gate's own error", got)
	}
}

func TestCandidateDomain(t *testing.T) {
	t.Parallel()

	for address, want := range map[string]string{
		"ada@example.com":     "example.com",
		"ADA@Example.COM":     "example.com",
		"  ada@example.com  ": "example.com",
		"not-an-address":      "",
		"":                    "",
	} {
		if got := (identity.Candidate{EmailAddress: address}).Domain(); got != want {
			t.Errorf("Candidate{%q}.Domain() = %q, want %q", address, got, want)
		}
	}
}
