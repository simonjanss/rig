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

// Record is one stored entry, with the identifier it was stored under.
//
// A writer never needs the identifier — it mints one and forgets it — and a
// reader always does, because a row somebody is looking at is a row they may
// want to name. Hence a type of its own rather than an [Entry] with a mostly
// empty field.
type Record struct {
	ID uuid.UUID
	Entry
}

// Query selects what to read.
//
// The tenant is not optional and there is no way to ask for more than one. The
// rows with no tenant at all — an attempt that named none, or one against an
// address with no account anywhere — are invisible to every reader here, and
// that is deliberate: matching on the email address instead would hand one
// tenant a record of another tenant's people typing their own addresses into a
// login form. A global view of those is an operator's need, answered by a query
// against the table, not by an endpoint.
//
// Every other field narrows. The zero value of each means "do not narrow by
// this", which is why Outcome has no third value for "either".
type Query struct {
	TenantID uuid.UUID

	// AccountID limits the read to one account's events, which is what makes
	// "where have I signed in from, and did anything fail" the same endpoint as
	// the tenant-wide trail rather than a second one with its own bugs.
	AccountID *uuid.UUID

	Event   string
	Outcome Outcome

	// Since and Until bound created_at, inclusive and exclusive respectively.
	Since time.Time
	Until time.Time

	Limit  int
	Offset int
}

// Reader answers what was recorded.
//
// Separate from [Log], and not a method on it, because the two cannot share a
// contract. Write swallows its failures on purpose — an entry describing a
// failed login must never fail the login — and a read that could not reach the
// database has to say so, or a screen shows an empty trail for a tenant that has
// one. An interface where half the methods report failure and half discard it is
// one somebody will eventually use the wrong way round.
type Reader interface {
	// Read returns one page, newest first, and the total number of rows matching
	// the query with the page bounds ignored.
	Read(ctx context.Context, q Query) ([]Record, int64, error)
}

// Pruner removes old entries.
//
// **The window has a floor and it is not a matter of taste.** This table is what
// the rate limiter counts, so deleting rows inside a limit's window clears the
// lockout those rows were adding up to — a limiter that silently stops limiting.
// Whoever calls this is responsible for passing an instant older than the longest
// window in force; [github.com/simonjanss/rig/runtime/throttle.Defaults.LongestWindow]
// is where that number comes from, and rig's own wiring refuses a shorter one
// before the server starts.
type Pruner interface {
	// Prune deletes entries recorded before the given instant and reports how
	// many went.
	Prune(ctx context.Context, olderThan time.Time) (int, error)
}

// Noop discards entries.
//
// It exists so a test can construct a manager in one line. It is not a
// reasonable production choice: with no log there are no rate limits, because
// there is nothing to count.
type Noop struct{}

// Write implements [Log].
func (Noop) Write(context.Context, Entry) {}
