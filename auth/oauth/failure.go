package oauth

import (
	"errors"

	"github.com/google/uuid"
)

// Reason names which way a sign-in did not finish.
//
// It exists because [github.com/simonjanss/rig/runtime/rigerr.Code] cannot answer this question. Nine of the ways
// a provider sign-in fails are a 400, so a caller switching on the code learns
// only that somebody's sign-in did not work — and an application that wants to
// say "you cancelled" rather than "something went wrong" has nothing to switch
// on but the prose of rig's messages, which is a test that passes until the day
// somebody rewords a sentence.
//
// The set is closed and the values are stable. An application maps them to its
// own copy, so a value here is a contract rather than an implementation detail:
// one reason per distinct thing a person or an operator could do about it, and
// never two reasons for one branch.
//
// Closed does not mean it never grows. A value never changes meaning and never
// disappears; a branch that turns out to be two branches gains a second value,
// which is a switch that falls through to a default rather than one that starts
// lying — and one branch answering for two facts is the failure this type
// exists to prevent.
type Reason string

// The ways a provider sign-in does not finish.
const (
	// ReasonCancelled is the consent screen's cancel button: the provider sent
	// back error=access_denied and nothing else. It is the one failure that is
	// not a failure, and the only one worth wording as a decision rather than a
	// problem.
	ReasonCancelled Reason = "cancelled"
	// ReasonProviderRefused is any other error the provider reported — a policy
	// the account's administrator enforces, most often.
	// [Failure.ProviderError] says which, for a log line.
	ReasonProviderRefused Reason = "provider_refused"
	// ReasonUnknownProvider is a request for a provider this deployment does not
	// have. It is what a sign-in button looks like against a process started
	// without that provider's credentials.
	ReasonUnknownProvider Reason = "unknown_provider"
	// ReasonTenant is [Config.Tenant] refusing to say which tenant is signing
	// in. The application wrote that resolver, so it is the one failure here
	// whose cause is not rig's.
	ReasonTenant Reason = "tenant"
	// ReasonReturnTo is a returnTo that is not a valid URL or is not an allowed
	// destination. It is reachable only at the start, and it is a caller
	// mistake rather than anything the person did.
	ReasonReturnTo Reason = "return_to"
	// ReasonState is the state cookie: missing, expired, forged, belonging to
	// another sign-in, or naming a different provider than the route did.
	//
	// One reason for five branches, deliberately, and for the same argument the
	// single message behind it is one message: the commonest cause by a wide
	// margin is a cookie that was never sent — a callback that landed on
	// another host, or a sign-in finished in a different browser than it
	// started in — and "start again" is the whole of the advice for every one of
	// them.
	ReasonState Reason = "state"
	// ReasonNoCode is a callback with no authorization code on it, which is not
	// something a provider does when the flow is intact.
	ReasonNoCode Reason = "no_code"
	// ReasonExchange is the provider refusing to trade the code for a token.
	// A wrong client secret is the usual cause, and it is the reason
	// [Failure.Err] keeps the provider's own error underneath.
	ReasonExchange Reason = "exchange"
	// ReasonProfile is the provider not returning a profile, or returning one
	// rig could not read.
	ReasonProfile Reason = "profile"
	// ReasonNoAddress is a provider that authenticated somebody without sharing
	// an email address, so there is no account to reach.
	ReasonNoAddress Reason = "no_address"
	// ReasonUnverifiedAddress is the check this package turns on: an address the
	// provider has not verified is never enough to link an account that already
	// exists. Trying again changes nothing.
	ReasonUnverifiedAddress Reason = "unverified_address"
	// ReasonNoAccount is somebody the provider authenticated and this
	// application has never heard of: no identity has that address, and
	// provisioning is off, so there is nobody to sign in as. Also not worth
	// suggesting a retry for.
	ReasonNoAccount Reason = "no_account"
	// ReasonNoTenantAccess is the other half of that, and the difference is
	// worth two sentences of copy rather than one: this application does know
	// them, they are simply not in the tenant this sign-in named, and joining
	// is off. "We have never heard of you" and "you are not in this workspace"
	// are answers a person can act on differently — the second one has somebody
	// to ask.
	//
	// It is the same refusal a password login gives for the same fact, word for
	// word, because a person who signs in two ways should not be told two
	// different things about one situation.
	ReasonNoTenantAccess Reason = "no_tenant_access"
	// ReasonEnding is [Config.OnSignIn] returning an error — the provider said
	// who this is and the last step refused. A tenant they turn out not to
	// belong to is the usual one.
	ReasonEnding Reason = "ending"
	// ReasonInternal is a failure on this side: sealing the state, reading a
	// store, a provider profile with no subject in it.
	ReasonInternal Reason = "internal"
)

// Failure is why a provider sign-in did not finish.
//
// It is what [Config.OnError] receives, and it is an error: it wraps the
// [rigerr.Error] the failure is answered with, so [github.com/simonjanss/rig/runtime/rigerr.CodeOf], an
// errors.As for a *rigerr.Error, and
// [github.com/simonjanss/rig/runtime/httpx.WriteError] all behave exactly as
// they do for the error rig would have written on its own. What it adds is the
// part none of them can carry: which of the fourteen ways this was.
type Failure struct {
	// Reason is which way, and it is the field an application switches on.
	Reason Reason
	// Provider is whose sign-in this was, lowercased as the route named it.
	// Empty when the request named no provider rig knows.
	Provider string

	// ProviderError is the raw error value the provider put on the query
	// string, for example access_denied.
	//
	// **For a log line, never for a page.** It is text anybody can write, and
	// putting it in a redirect to your own origin is reflecting
	// attacker-controlled input into your application. [Failure.Reason] already
	// says what it meant, from a set rig chose.
	ProviderError string

	// ReturnTo is where this sign-in asked to be sent afterwards, so a failure
	// can still return somebody to the page that started it rather than to a
	// sign-in page's own default.
	//
	// It lives in the sealed state cookie, so it is known only for failures
	// raised after that cookie was opened: always empty for
	// [ReasonUnknownProvider], [ReasonState] and every failure at the start,
	// and it is already checked against [Config.AllowedReturnTo].
	ReturnTo string

	// EmailAddress is who the provider said this was, lowercased. Empty until a
	// profile has come back, which is most of these.
	EmailAddress string
	// TenantID is the tenant the sign-in was for, or uuid.Nil when it named
	// none — which is an ordinary answer here rather than a missing one.
	TenantID uuid.UUID

	// Err is the error this is answered with when there is no hook, and the
	// place a cause underneath it survives: the provider's own refusal on
	// [ReasonExchange], the store's error on [ReasonInternal].
	//
	// Its message is written for the person who tried to sign in, **except on
	// [ReasonInternal]**, where it is a seal or a store failure and rig's own
	// default answers "something went wrong" instead of showing it. A hook that
	// renders this message has to do the same, or it publishes on a sign-in page
	// the one thing rig refuses to.
	Err error
}

// Error is [Failure.Err]'s message, so that a Failure can be returned and
// logged as the error it is.
//
// A Failure rig built always carries one. The reason alone is the answer for a
// zero value, because a type whose String panics is a type that turns a bug in
// a hook into a crash in the sign-in it was handling.
func (f *Failure) Error() string {
	if f.Err == nil {
		return "oauth: sign-in failed: " + string(f.Reason)
	}
	return f.Err.Error()
}

// Unwrap gives up the error underneath, which is what makes
// [github.com/simonjanss/rig/runtime/rigerr.CodeOf] and an errors.As for a
// *rigerr.Error answer the same for a Failure as for the error it carries.
func (f *Failure) Unwrap() error { return f.Err }

// failure is the Failure err already is, or a new one carrying it.
//
// The fallback reason is for an error rig did not raise itself — a Store
// refusing, a provider client failing — where the reason has to be decided by
// the caller that knows what it was doing rather than by reading the message.
func failure(err error, fallback Reason) *Failure {
	var f *Failure
	if errors.As(err, &f) {
		return f
	}
	return &Failure{Reason: fallback, Err: err}
}
