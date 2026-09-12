// Package identity is the one gate on who may become a person in a deployment.
//
// Before this there were three answers in three places and none of them was a
// predicate: a bool for the provider door, a bool for the mailed-code door, and
// a permission on whoever called Provision or Invite. So a deployment that wanted
// one email domain had to write the rule three times if it could write it at all,
// and on the provider path — which is how most people actually arrive — it could
// not: the two available answers were "anybody with an account at this provider"
// and "nobody new".
//
// The gate is on the identity rather than on the method, because "may this person
// become somebody here?" is not an OAuth question. One [Gate] is consulted by
// every path that would insert a rig_identity, so a deployment cannot acquire an
// ungated door by turning on a sign-in method it did not have before.
//
// It is a package of its own, and a leaf, so that auth/oauth and auth/account can
// both reach it without either reaching the other. Those two answer different
// halves of the same question and share nothing else; making one import the other
// to ask a predicate would pull sessions, throttling and the notifier into a
// provider sign-in.
package identity

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/runtime/rigerr"
)

// Source is how somebody is arriving, and it is on [Candidate] because the answer
// legitimately differs.
//
// The rule an application usually wants is "strangers must be at our domain, but
// an administrator may invite whoever they like" — a school inviting a supply
// teacher with a personal address is the ordinary case rather than an abuse. One
// predicate with no context cannot express that, and a separate hook per door is
// a separate place to forget.
//
// The set is closed and the values are stable, the way oauth.Reason's are: a
// value never changes meaning and never disappears, and a new way in adds one
// rather than reusing the nearest.
type Source string

const (
	// SourceProvider is a sign-in with an identity provider. The address has
	// been verified by that provider by the time the gate is asked.
	SourceProvider Source = "provider"
	// SourceEmailCode is somebody asking for a mailed sign-in code. The address
	// is unproven here, because the identity is created when the code is asked
	// for rather than when it is typed back — so [Candidate.EmailVerified] is
	// false, and a gate that insisted on it would close this door entirely.
	SourceEmailCode Source = "email_code"
	// SourceInvitation is an address an administrator typed into
	// POST /auth/invitations. Somebody here vouched for them.
	SourceInvitation Source = "invitation"
	// SourceProvision is an address an administrator or an import added
	// directly, as a member rather than as an invitation.
	SourceProvision Source = "provision"
)

// Candidate is somebody nobody here has ever heard of.
type Candidate struct {
	// EmailAddress is as it was given, and [Candidate.Domain] is the part a rule
	// is usually about.
	EmailAddress string
	// EmailVerified says whether the address has been proved by the time the
	// gate is asked, which depends on Via — see [SourceProvider] and
	// [SourceEmailCode].
	EmailVerified bool
	DisplayName   string

	// Via is how this person is arriving.
	Via Source

	// Provider is the provider's name when Via is [SourceProvider], and empty
	// otherwise.
	Provider string
	// TenantID is the tenant somebody is being invited or provisioned into, when
	// Via is [SourceInvitation] or [SourceProvision]. Nil otherwise: a sign-in
	// names no tenant on the paths that matter, because which one somebody lands
	// in is settled after the identity is resolved.
	TenantID *uuid.UUID
}

// Domain is the part of the address after the @, lowercased, or "".
func (c Candidate) Domain() string {
	_, host, found := strings.Cut(strings.ToLower(strings.TrimSpace(c.EmailAddress)), "@")
	if !found {
		return ""
	}
	return host
}

// Gate decides whether a stranger may become an identity. Returning an error
// refuses, and the error's own words are what the refusal says.
//
// It is asked about strangers and nobody else. By the time it runs, rig has
// established that this address has no provider link and no identity of its own —
// so an existing member, and somebody adding a second provider to an account they
// already have, have both been admitted without consulting it. An application
// writes one rule, not a chain of them.
//
// The consequence worth stating: this is a gate on *becoming* somebody, not a
// filter on signing in. An identity that already exists at a domain a gate would
// now refuse goes on signing in, because nobody is asking about them any more.
// Deactivating them is the answer to that, and it is a different question.
type Gate func(ctx context.Context, in Candidate) error

// AllowDomains refuses a stranger whose address is not in the list.
//
// It is what auth.allowed_identity_domains in rig.yaml generates, and it is a
// [Gate] like any other rather than a mechanism beside them: an application that
// wants the domain rule plus something else writes its own gate and calls
// [DomainAllowed] inside it.
//
// An administrator is not asked. [SourceInvitation] and [SourceProvision] are
// allowed whatever the list says, because somebody who is already here typed that
// address in and a deployment that could not invite an auditor, a contractor or a
// supply teacher would be one nobody could run. What it closes is the door a
// stranger walks through on their own.
//
// An empty list allows everybody, which makes this the same as no gate at all.
func AllowDomains(domains ...string) Gate {
	return func(_ context.Context, in Candidate) error {
		switch in.Via {
		case SourceInvitation, SourceProvision:
			return nil
		}
		if DomainAllowed(in.EmailAddress, domains) {
			return nil
		}
		return rigerr.Forbidden("this deployment is for %s addresses", strings.Join(domains, ", "))
	}
}

// DomainAllowed reports whether an address is in a list of domains.
//
// An empty list allows anything, because most deployments are not one company and
// a list that had to be filled in before anybody could be added would be a worse
// default than no list at all. A listed domain matches its subdomains too:
// somebody who allows example.com means the company, and mail.example.com is the
// company.
func DomainAllowed(emailAddress string, domains []string) bool {
	if len(domains) == 0 {
		return true
	}

	_, host, found := strings.Cut(strings.ToLower(strings.TrimSpace(emailAddress)), "@")
	if !found || host == "" {
		return false
	}

	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(d, "@")))
		if d == "" {
			continue
		}
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// Allow asks a gate, and answers nil when there is none.
//
// Every call site is "if the gate refuses, refuse", and the nil check belongs in
// one place rather than in four — a path that forgot it would be an ungated door,
// which is the exact failure this package exists to prevent.
func Allow(ctx context.Context, gate Gate, in Candidate) error {
	if gate == nil {
		return nil
	}
	return gate(ctx, in)
}
