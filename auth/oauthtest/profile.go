package oauthtest

import (
	"fmt"
	"strconv"

	"github.com/simonjanss/rig/auth/oauth"
)

// spelling turns a profile into the JSON a named provider would have answered
// with, which is the half of this package a project cannot get right on its own.
//
// Every provider says the same three facts differently, and [oauth.Provider.Parse]
// is where rig ends that. A stand-in has to speak the other direction, and getting
// it wrong does not fail: a camelCase key parses to an empty profile and surfaces
// two layers away as [oauth.ReasonInternal], blaming rig for a typo in a test.
// So the two directions are kept in one place and checked against each other —
// see TestEveryProviderReadsWhatTheStandInWrites in auth/oauth.
//
// A name this does not know is served Google's spelling, which is OpenID Connect's
// and therefore the right guess: [Server.Custom] and anything wearing an in-house
// provider land here.
func spelling(provider string, p oauth.Profile) (any, error) {
	switch provider {
	case oauth.ProviderGitHub:
		// The numeric id, because that is what GitHub's Parse reads and what a
		// rig identity is matched on. A non-numeric subject is a mistake in the
		// test rather than something to paper over: it would parse to 0 and
		// quietly make every GitHub profile the same person.
		id, err := strconv.ParseInt(p.Subject, 10, 64)
		if err != nil {
			return nil, fmt.Errorf(
				"oauthtest: a GitHub subject is the numeric user id, and %q is not one", p.Subject)
		}
		// No address here, ever. GitHub's user endpoint returns null for anybody
		// who kept theirs private and never says whether one is verified, which
		// is the whole reason that provider has an Extra at all — emails is what
		// answers both. Writing the address here too would let a stand-in pass
		// while Extra was broken.
		return map[string]any{"id": id, "name": p.DisplayName, "login": p.DisplayName}, nil

	case oauth.ProviderMicrosoft:
		// No email_verified: Microsoft's userinfo does not return one, and rig's
		// Parse infers verification from the address being present at all.
		return map[string]any{"sub": p.Subject, "email": p.EmailAddress, "name": p.DisplayName}, nil

	default:
		return map[string]any{
			"sub":            p.Subject,
			"email":          p.EmailAddress,
			"email_verified": p.EmailVerified,
			"name":           p.DisplayName,
		}, nil
	}
}

// emails is the second call GitHub's Extra makes, in that endpoint's own shape.
//
// An unverified profile still gets a row, unverified — so the refusal path is
// reachable rather than only the happy one.
func emails(p oauth.Profile) any {
	if p.EmailAddress == "" {
		return []any{}
	}
	return []map[string]any{{
		"email": p.EmailAddress, "primary": true, "verified": p.EmailVerified,
	}}
}
