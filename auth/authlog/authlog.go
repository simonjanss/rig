// Package authlog records what happened to an authentication attempt.
//
// The log is not only an audit trail. It is also what the rate limiter counts,
// which is the point: a limit that reads the same rows the audit does cannot
// drift from it, and there is no second store to deploy, monitor, or explain
// during an incident.
//
// Writing an entry must never fail a request. A login that succeeded and then
// returned 500 because the log was unreachable has turned an observability
// problem into an outage.
package authlog

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Events. They are the same strings [github.com/simonjanss/rig/runtime/throttle]
// counts, so a limit and the trail it reads cannot disagree about what an
// event is called.
const (
	EventLoginAttempted         = "LoginAttempted"
	EventLoginSucceeded         = "LoginSucceeded"
	EventLoginFailed            = "LoginFailed"
	EventAccountLocked          = "AccountLocked"
	EventLogout                 = "Logout"
	EventTokenRefreshed         = "TokenRefreshed"
	EventTokenReuseDetected     = "TokenReuseDetected"
	EventPasswordResetRequested = "PasswordResetRequested"
	EventPasswordResetCompleted = "PasswordResetCompleted"
	EventPasswordChanged        = "PasswordChanged"
	EventEmailVerified          = "EmailVerified"
	EventVerificationResent     = "VerificationResent"
	EventAPIKeyAuthSucceeded    = "ApiKeyAuthSucceeded"
	EventAPIKeyAuthFailed       = "ApiKeyAuthFailed"
	EventImpersonationStarted   = "ImpersonationStarted"
	EventImpersonationEnded     = "ImpersonationEnded"
	EventOAuthSignIn            = "OAuthSignIn"
	EventAccountProvisioned     = "AccountProvisioned"
	EventInvitationSent         = "InvitationSent"
	EventInvitationAccepted     = "InvitationAccepted"
	EventInvitationRevoked      = "InvitationRevoked"
	EventTenantSwitched         = "TenantSwitched"
)

// Events is every event this package can write.
//
// It exists to be checked against the database. The auth_event column is an enum,
// and [Log.Write] swallows its error on purpose — an entry describing a failed
// login must not fail the login — so an event the enum does not have is not an
// error anywhere: it is a row that silently never appears. Listing them here
// gives a test something to compare the live type against.
func Events() []string {
	return []string{
		EventLoginAttempted,
		EventLoginSucceeded,
		EventLoginFailed,
		EventAccountLocked,
		EventLogout,
		EventTokenRefreshed,
		EventTokenReuseDetected,
		EventPasswordResetRequested,
		EventPasswordResetCompleted,
		EventPasswordChanged,
		EventEmailVerified,
		EventVerificationResent,
		EventAPIKeyAuthSucceeded,
		EventAPIKeyAuthFailed,
		EventImpersonationStarted,
		EventImpersonationEnded,
		EventOAuthSignIn,
		EventAccountProvisioned,
		EventInvitationSent,
		EventInvitationAccepted,
		EventInvitationRevoked,
		EventTenantSwitched,
	}
}

// Outcome is whether the attempt worked.
type Outcome string

// The outcomes. Two and no third, because the rate limiter counts these: an
// "unknown" or "pending" would be a row that is neither a failure to count
// against a key nor a success that clears the window, and it would age into the
// audit trail as a question nobody can answer later.
const (
	Succeeded Outcome = "Succeeded"
	Failed    Outcome = "Failed"
)

// Entry is one row.
//
// Almost every field is optional because almost every field is unknown in some
// case worth recording: a login for an address with no account has no tenant
// and no account, and it is exactly the entry a rate limiter needs.
type Entry struct {
	At      time.Time
	Event   string
	Outcome Outcome

	TenantID  *uuid.UUID
	AccountID *uuid.UUID
	APIKeyID  *uuid.UUID

	// EmailAddress is stored lowercased, because that is how it is counted.
	EmailAddress string
	// APIKeyRef is the public half of a key as presented, whether or not it
	// resolved to a row. A limit on failed key authentication is useless if it
	// can only count keys that exist.
	APIKeyRef string

	IPAddress string
	UserAgent string

	// TokenRootID identifies the session family, so refresh limits and reuse
	// investigations have something to group by.
	TokenRootID *uuid.UUID

	// Detail carries whatever else is worth knowing. Reuse detection puts the
	// original and current address and user agent here, which is what turns
	// "somebody replayed a token" into "somebody replayed it from Frankfurt".
	Detail map[string]any
}

// Log receives entries.
type Log interface {
	// Write records an entry. An implementation that cannot must not return
	// an error the caller will propagate; see the package comment.
	Write(ctx context.Context, e Entry)
}

// Noop discards entries.
//
// It exists so a test can construct a manager in one line. It is not a
// reasonable production choice: with no log there are no rate limits, because
// there is nothing to count.
type Noop struct{}

// Write implements [Log].
func (Noop) Write(context.Context, Entry) {}
