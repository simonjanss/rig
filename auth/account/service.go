// Package account implements the sign-in flows.
//
// There are two ways in and rig stores no passwords. A provider vouches for
// somebody (see [github.com/simonjanss/rig/auth/oauth]), or rig mails a short
// code to the address they are signing in with and they type it back — which is
// this package's own door, and the one anybody with no provider account uses.
//
// Everything here exists to get a handful of details right that are easy to get
// wrong and expensive to get wrong:
//
//   - The lockout is checked before the code is compared, so a locked request
//     neither does the work nor extends its own window.
//   - A sign-in for an address rig has never seen takes the same time as one
//     for an address it has, so response time is not a membership oracle.
//   - A wrong code and a disabled account are told apart only after the code is
//     compared, so "this account is disabled" cannot be used to enumerate
//     accounts.
//   - Asking for a code answers the same way whether or not the address exists.
//   - A code dies after a few wrong guesses rather than merely being slowed
//     down, because six digits are guessable in a way a token is not.
package account

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/runtime/outbox"

	"github.com/simonjanss/rig/auth/authlog"
	"github.com/simonjanss/rig/auth/session"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/throttle"
)

// Defaults.
const (
	// DefaultMinDuration pads a sign-in, and it is the whole of what keeps an
	// address rig knows and one it does not indistinguishable.
	//
	// Comparing a code costs one sha256 whether or not the address exists, which
	// is far too cheap to hide the reads around it: the lookup that finds
	// nobody returns sooner than the one that finds a person and their live
	// code. A floor over both makes the difference noise. It used to be
	// belt-and-braces over an argon2 hash that did most of this work; it is not
	// any more, so do not read it as a tax and remove it.
	DefaultMinDuration = 750 * time.Millisecond
	// DefaultVerificationTTL is longer, because confirming an address is not
	// urgent and a link that expires while somebody is at lunch is a support
	// ticket.
	DefaultVerificationTTL = 24 * time.Hour
	// DefaultInvitationTTL is longer again: an invitation sent on a Friday should
	// still work on Monday, and the cost of it expiring is somebody having to ask
	// a colleague to send another.
	DefaultInvitationTTL = 7 * 24 * time.Hour

	tokenBytes = 32
)

var tokenEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// Config builds a service.
type Config struct {
	Store    Store
	Sessions *session.Manager
	// Identities issues the tenant-less credential a tenant picker runs on.
	// Required: a sign-in always produces one, including for somebody who lands
	// straight in a tenant, because switching later is the same flow.
	Identities *session.IdentityManager
	Log        authlog.Log
	Notifier   Notifier

	// EmailCode is the mailed-code sign-in. The zero value is off, and a
	// deployment with no provider configured either has no way in at all —
	// which [New] does not refuse, because a service that only provisions and
	// invites is a legitimate thing to build.
	EmailCode EmailCodeOptions

	// Outbox turns mail queueing on, and its presence is the whole switch.
	//
	// Nil is the inline path this package shipped with, byte for byte: a link is
	// minted in the request that asked for it and handed straight to the
	// Notifier. That is the default, and it has to be — queueing by default would
	// mean every existing deployment upgrades, keeps passing its Notifier, does
	// not add the cron job, and silently stops sending mail. That is a worse
	// outage than the one the queue fixes.
	//
	// Set it and a link is written to rig_identity_verification_delivery instead,
	// in the transaction that asked for it, and sent by [Service.DispatchMail] —
	// which something has to run. See [Outbox].
	//
	// The trade to know about before turning it on: a reset mail now arrives up
	// to one dispatch interval late, where inline it was sent inside the request.
	// What is bought is that a provider having a bad hour no longer fails the
	// request, spends the caller's rate-limit budget, and kills a token that was
	// already minted.
	Outbox Outbox
	// Mail is what the queue runs on. Every field is optional, and it is read
	// only when Outbox is set.
	Mail MailOptions

	// Tenants is what an application decides about making tenants: who may, what
	// a name may be, and what else a new one needs. Every field is optional; the
	// zero value lets anybody signed in make one called anything.
	Tenants TenantOptions

	// OnRegistered runs inside the transaction that creates a person rig has
	// never seen — asking for a sign-in code with a new address, where
	// [EmailCodeOptions.AllowProvisioning] allows it. Returning an error rolls
	// the whole thing back, so a retry is a clean retry rather than a conflict
	// with a half-made identity.
	//
	// It receives the service because the ordinary body is a call back into it —
	// [Service.Provision], bringing the newcomer into a starter tenant, or
	// [Service.Invite] to leave one waiting in their picker — and the closure is
	// handed to [New] before the service exists. Reach the transaction itself
	// with dbx.Tx(ctx), the same way a generated repository does.
	//
	// One thing to know before using it for anything expensive: it runs when the
	// code is *asked for*, not when it is typed back, because the row that holds
	// the code references the person. So a starter tenant made here may belong
	// to somebody who never finishes signing in.
	//
	// Nil, the default, creates the person and nothing else.
	OnRegistered func(ctx context.Context, accounts *Service, in Registered) error

	// OnJoined runs inside the transaction that accepts an invitation — after
	// the account exists, before the session is issued. Returning an error rolls
	// the whole acceptance back, so a retry is a clean retry rather than a
	// member with half a setup.
	//
	// It exists because of where an account now comes from. [Service.Provision]
	// is called by the application, so whatever else a new member needs — a
	// role's grants, a preferences row — is the caller's next line. Accepting an
	// invitation is called by rig's own handler, and this is that line.
	//
	// Reach the transaction with dbx.Tx(ctx). It does not receive the service:
	// the ordinary body is a write against the application's own tables rather
	// than a call back into this package.
	OnJoined func(ctx context.Context, in Joined) error

	// Limiter and Limits are what stop somebody guessing. A service built
	// without a limiter refuses to exist: a login endpoint with no lockout is
	// not a sign-in endpoint, it is a guessing oracle with a queue.
	Limiter *throttle.Limiter
	Limits  throttle.Defaults

	MinDuration     time.Duration
	VerificationTTL time.Duration
	// InvitationTTL bounds an invitation. It is the longest of the three by
	// default: somebody invited on a Friday should still be able to join on
	// Monday, and an expired invitation means asking a colleague to send another.
	InvitationTTL time.Duration

	// RequireVerifiedEmail refuses a sign-in until the address is confirmed.
	RequireVerifiedEmail bool

	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration)
}

// Service is the sign-in flows.
type Service struct {
	cfg   Config
	now   func() time.Time
	sleep func(context.Context, time.Duration)

	// The mail queue's own state, resolved once at construction so that a
	// dispatch pass reads no configuration. Zero-valued and unused on the inline
	// path.
	mail MailOptions
	// mailClaimedBy is one identifier per process, so a stuck lease traces to a
	// pod rather than to a mystery.
	mailClaimedBy uuid.UUID

	mailMu       sync.Mutex
	mailClaiming bool
	// mailLeases are the claims this process currently owns, so a clean shutdown
	// can give them back rather than leaving them to expire. Its own lock, not
	// mailMu: whether this service is still claiming and what it is holding are
	// never read together.
	mailLeases outbox.Leases
}

// New builds a service.
func New(cfg Config) (*Service, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("account: a Store is required")
	case cfg.Sessions == nil:
		return nil, errors.New("account: a session manager is required")
	case cfg.Identities == nil:
		return nil, errors.New("account: an identity session manager is required; " +
			"a sign-in issues one whether or not it lands in a tenant")
	case cfg.Limiter == nil:
		return nil, errors.New("account: a throttle.Limiter is required; " +
			"a sign-in endpoint with no lockout is a guessing oracle with a queue")
	}

	if cfg.Log == nil {
		cfg.Log = authlog.Noop{}
	}
	if cfg.Notifier == nil {
		cfg.Notifier = NoNotifier{}
	}
	if cfg.Limits == (throttle.Defaults{}) {
		cfg.Limits = throttle.Standard()
	}
	if cfg.VerificationTTL == 0 {
		cfg.VerificationTTL = DefaultVerificationTTL
	}
	if cfg.InvitationTTL == 0 {
		cfg.InvitationTTL = DefaultInvitationTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	code, err := resolveEmailCode(cfg)
	if err != nil {
		return nil, err
	}
	cfg.EmailCode = code

	mail, err := resolveMail(cfg)
	if err != nil {
		return nil, err
	}

	// UTC at the source, for the same reason the session manager does it: a
	// verification's expiry and an account this service builds are answered
	// without being read back, so nothing else would settle their zone.
	utc := func() time.Time { return cfg.Now().UTC() }
	s := &Service{
		cfg: cfg, now: utc, sleep: cfg.Sleep,
		mail:          mail,
		mailClaimedBy: uuid.New(),
		mailClaiming:  true,
	}
	if s.sleep == nil {
		s.sleep = sleepUntil
	}
	return s, nil
}

// resolveMail fills in the queue's numbers and refuses the pairs that cannot both
// be true.
//
// Refused at construction rather than found later, which is the argument
// notify.NewEngine makes for the same three checks: the failure they prevent is
// duplicate mail six weeks from now, and there is nothing in a running system
// that would point at the configuration. Both numbers appear in every message,
// because the fix is to change one of them and the reader should not have to work
// out which two disagreed.
func resolveMail(cfg Config) (MailOptions, error) {
	m := cfg.Mail
	if cfg.Outbox == nil {
		// Nothing reads these on the inline path, and resolving them anyway
		// would mean refusing a configuration nobody is using.
		return m, nil
	}

	if _, ok := cfg.Notifier.(NoNotifier); ok || cfg.Notifier == nil {
		return m, errors.New("account: an Outbox is set but no Notifier is, so every " +
			"queued link would be written and then dropped; set a Notifier, or leave " +
			"the Outbox nil to keep sending inline")
	}

	if m.ClaimTTL == 0 {
		m.ClaimTTL = DefaultMailClaimTTL
	}
	if m.ClaimTTL < MinMailClaimTTL {
		return m, fmt.Errorf("account: Mail.ClaimTTL is %s, and under %s every mail a "+
			"slow provider is still sending is claimed twice; set it longer than that "+
			"provider's own timeout", m.ClaimTTL, MinMailClaimTTL)
	}

	if m.SendTimeout == 0 {
		m.SendTimeout = DefaultMailSendTimeout
	}
	if m.SendTimeout >= m.ClaimTTL {
		// Equal is refused with longer, because a lease is stamped before the
		// send it protects starts: a send allowed to run the whole lease ends
		// after it.
		return m, fmt.Errorf("account: Mail.SendTimeout is %s and Mail.ClaimTTL is %s, "+
			"so a send may still be running when its own lease expires and another "+
			"dispatcher takes the row; set SendTimeout below ClaimTTL",
			m.SendTimeout, m.ClaimTTL)
	}

	if m.MaxAttempts <= 0 {
		m.MaxAttempts = DefaultMailMaxAttempts
	}
	if m.BackoffBase <= 0 {
		m.BackoffBase = DefaultMailBackoffBase
	}
	if m.BackoffCap <= 0 {
		m.BackoffCap = DefaultMailBackoffCap
	}
	if m.BackoffCap < m.BackoffBase {
		return m, fmt.Errorf("account: Mail.BackoffCap is %s and Mail.BackoffBase is %s, "+
			"so the cap binds before the first doubling and every retry waits the same "+
			"%s; set BackoffCap above BackoffBase", m.BackoffCap, m.BackoffBase, m.BackoffCap)
	}
	if m.Jitter == nil {
		m.Jitter = rand.Int64N
	}
	return m, nil
}

// SignInResult is what a sign-in produced.
//
// Two credentials, because there are two states a signed-in person can be in.
// Session is the ordinary one, scoped to a tenant, and is what every generated
// endpoint accepts. Identity is the other: it proves who somebody is and carries
// no tenant, which is what somebody who belongs to no tenant yet has — and
// what the tenant picker runs on.
//
// Session is nil exactly when Tenants is empty. That is not a failure any
// more: an invitation waiting to be accepted is a perfectly good reason to have
// an account and no tenant, and answering 403 to it made the flow impossible.
type SignInResult struct {
	IdentityID uuid.UUID
	// TenantID is the tenant the session landed in, and Nil when there was
	// none to land in.
	TenantID uuid.UUID

	// Session is the tenant session, or nil when there is no tenant to be
	// in. When a tenant was named it is that one; otherwise wherever they were
	// last — see [Service.SignInIdentity].
	Session *session.Pair
	// Identity is always issued, including alongside a session: signing in and
	// then switching tenants is one flow, and the picker needs a credential
	// that outlives whichever tenant was landed on.
	Identity session.Issued
	// Tenants are every one this person belongs to, so a client can draw the
	// picker without a second call.
	Tenants []Membership
}

// SignInIdentityInput is a sign-in that already knows who somebody is.
type SignInIdentityInput struct {
	IdentityID uuid.UUID
	// TenantID says which tenant the session is for, and may be uuid.Nil, which
	// means "wherever they belong" — see [Service.SignInIdentity].
	TenantID uuid.UUID

	Remember  bool
	Client    session.Client
	IPAddress string
	UserAgent string
	// Method is how the person proved who they are — a provider name, "Google",
	// or "EmailCode" — and lands in the audit entry's detail. Empty means
	// nothing is claimed, which is what an invitation being accepted says.
	Method string
}

// SignInIdentity is everything a sign-in does once it knows who somebody is.
//
// It is the whole tail of a sign-in: which of the person's accounts this session
// is for, the tenant list the picker draws from, the identity token, the session
// itself, and the audit entry. [Service.VerifyEmailCode] calls it once the code
// has been compared, and a provider sign-in calls it once the provider has said
// who this is — which is what stops the two paths answering "where does this
// person go" differently.
//
// Input.TenantID may be uuid.Nil, and that is an ordinary answer rather than a
// missing one. Named: that tenant or a refusal. Nil: wherever they belong —
// which is where they were last, or their oldest tenant, or, for somebody who
// belongs nowhere yet, nowhere at all. The last case is a **success**:
// [SignInResult.Session] is nil, [SignInResult.Identity] is issued anyway, and
// where they go next is the picker's problem.
//
// What it does not do, because its caller does: no rate-limit check, and no
// credential of any kind. What it does do is every refusal that is about the
// person rather than about how they proved it — an identity that is gone or
// disabled, because a provider link outlives both and the row in
// rig_identity_oauth carries no deleted_at and no is_active, and
// RequireVerifiedEmail.
//
// That last one is what this door is for. oauth's LinkIdentity records the
// verified address it took as evidence, so a provider sign-in can be held to
// the rule rather than exempted from it on a column nobody filled in.
//
// The code flow is not held to it, and that is deliberate rather than an
// oversight: it calls the unexported half of this, because confirming the
// address *is* what typing the code did, and gating the tail would refuse
// somebody on a column their own request had just filled in. Accepting an
// invitation and creating a tenant are ungated for the same reason and always
// have been.
//
// Two things about the audit trail are worth knowing before wiring it to
// something new. It writes EventLoginSucceeded and EventLoginFailed, which is
// what [github.com/simonjanss/rig/runtime/throttle.Standard] counts and clears
// — so a provider sign-in lifts the address's code lockout, which is safe
// because it takes control of the provider account, and a refusal in here counts
// towards that lockout. And it writes what the limiter counts without consulting
// the limiter, which is right when there is nothing being guessed but means the
// bound on calling this has to come from the caller.
func (s *Service) SignInIdentity(ctx context.Context, in SignInIdentityInput) (SignInResult, error) {
	// Read rather than accepted. The input carries an identifier and not a row,
	// so that nobody can hand in an identity that says it is active.
	ident, err := s.cfg.Store.FindIdentityByID(ctx, in.IdentityID)
	if err != nil {
		return SignInResult{}, err
	}
	if ident == nil {
		// Unreachable from the code flow, which returns before this having read
		// the identity to find the code. It is here for the provider path: a
		// link in rig_identity_oauth survives the soft delete of the identity it
		// hangs off.
		s.failSignIn(ctx, signInAttempt{
			tenantID:  in.TenantID,
			ipAddress: in.IPAddress,
			userAgent: in.UserAgent,
			method:    in.Method,
		}, nil, "no such identity")
		return SignInResult{}, rigerr.Forbidden("this account is no longer active")
	}
	if err := s.refuseUnverified(ctx, signInAttempt{
		tenantID:  in.TenantID,
		email:     normalizeEmail(ident.EmailAddress),
		ipAddress: in.IPAddress,
		userAgent: in.UserAgent,
		method:    in.Method,
	}, ident); err != nil {
		return SignInResult{}, err
	}
	return s.signInIdentity(ctx, ident, in)
}

// refuseUnverified is the RequireVerifiedEmail gate, and it applies to one door:
// an identity somebody else vouched for, through [Service.SignInIdentity].
//
// Not in the shared tail, and that is what makes the code flow work at all.
// [Service.VerifyEmailCode] confirms the address as part of signing somebody in
// — the code went to that address and came back, which is the same proof an
// invitation link is — so gating the tail would refuse somebody on a column
// their own request had just filled in. It calls the unexported half for that
// reason, the way accepting an invitation does.
func (s *Service) refuseUnverified(ctx context.Context, at signInAttempt, ident *Identity) error {
	if !s.cfg.RequireVerifiedEmail || ident.Verified() {
		return nil
	}
	s.failSignIn(ctx, at, nil, "email not verified")
	return rigerr.Forbidden("confirm your email address before signing in")
}

// signInIdentity is [Service.SignInIdentity] with the identity already read.
//
// The seam exists for two reasons. A caller that has the row in hand should not
// read it twice — the code flow does, having looked the address up to find the
// code. And it skips RequireVerifiedEmail, which is what the code flow needs:
// see [Service.refuseUnverified].
func (s *Service) signInIdentity(
	ctx context.Context, ident *Identity, in SignInIdentityInput,
) (SignInResult, error) {
	at := signInAttempt{
		tenantID:  in.TenantID,
		email:     normalizeEmail(ident.EmailAddress),
		ipAddress: in.IPAddress,
		userAgent: in.UserAgent,
		method:    in.Method,
	}

	// The identity's own state, checked here rather than only in login, because
	// a provider sign-in never reads rig_identity on its way in.
	if !ident.IsActive {
		s.failSignIn(ctx, at, nil, "identity disabled")
		return SignInResult{}, rigerr.Forbidden("this account has been disabled")
	}

	// Which of the person's accounts this session is for. Answering plainly is
	// safe here and nowhere earlier: they have proved who they are, so learning
	// which tenants are theirs tells them nothing about anybody else.
	acct, err := s.accountFor(ctx, in.TenantID, ident.ID)
	if err != nil {
		return SignInResult{}, err
	}
	if acct == nil && in.TenantID != uuid.Nil {
		// A named tenant they are not in. Still a refusal: they asked for
		// somewhere specific and the answer is no.
		s.failSignIn(ctx, at, nil, "no account in this tenant")
		return SignInResult{}, rigerr.Forbidden("you do not have access to this tenant")
	}
	if acct != nil && !acct.IsActive {
		s.failSignIn(ctx, at, acct, "account disabled")
		return SignInResult{}, rigerr.Forbidden("this account has been disabled")
	}

	// A backstop rather than the rule. A service account has no identity, so the
	// lookup above cannot reach one in the schema rig ships — the CHECK on
	// account makes sure of it, and an integration's address resolves to nobody
	// at all, which is a better answer than this one. This stays for a schema
	// where that constraint was dropped, because the consequence of being wrong
	// is a way in that nobody is watching.
	if acct != nil && acct.Kind == KindService {
		s.failSignIn(ctx, at, acct, "service account")
		return SignInResult{}, rigerr.Forbidden("a service account cannot sign in; use its API key")
	}

	out := SignInResult{IdentityID: ident.ID}

	// The tenant list first, because it is the answer even when there is no
	// session to go with it: somebody with an invitation waiting belongs nowhere
	// yet, and telling them so with an empty list beats refusing the sign-in.
	out.Tenants, err = s.cfg.Store.TenantsForIdentity(ctx, ident.ID)
	if err != nil {
		return SignInResult{}, err
	}

	// Issued whether or not a tenant was found. Signing in and then switching
	// is one flow, and the picker needs a credential that outlives whichever
	// tenant was landed on.
	out.Identity, err = s.cfg.Identities.Issue(ctx, session.IdentityIssueInput{
		IdentityID: ident.ID,
		IPAddress:  in.IPAddress,
		UserAgent:  in.UserAgent,
	})
	if err != nil {
		return SignInResult{}, err
	}

	if acct == nil {
		// Signed in and nowhere to be. Recorded as a success, because it is one:
		// they proved who they are, and where they go next is the picker's problem.
		s.write(ctx, at.entry(authlog.Entry{
			Event: authlog.EventLoginSucceeded, Outcome: authlog.Succeeded,
			Detail: map[string]any{"tenants": 0},
		}))
		return out, nil
	}

	pair, err := s.cfg.Sessions.Issue(ctx, session.IssueInput{
		TenantID:  acct.TenantID,
		AccountID: acct.ID,
		Client:    in.Client,
		IPAddress: in.IPAddress,
		UserAgent: in.UserAgent,
		Remember:  in.Remember,
	})
	if err != nil {
		return SignInResult{}, err
	}
	out.Session, out.TenantID = &pair, acct.TenantID

	e := at.entry(authlog.Entry{
		Event: authlog.EventLoginSucceeded, Outcome: authlog.Succeeded,
		TokenRootID: &pair.RootTokenID,
	})
	e.TenantID, e.AccountID = &acct.TenantID, &acct.ID
	s.write(ctx, e)
	return out, nil
}

// accountFor is the account a sign-in is for.
//
// Named tenant: that one, or nothing. No tenant: wherever they were last, and
// failing that whichever tenant they joined first, skipping any they have been
// removed from.
//
// Last rather than oldest because it is what somebody expects: the tenant they
// were in is the tenant they meant, and it only ever changes because they
// changed it. Oldest is the fallback rather than the rule because it is the
// answer for a first sign-in, where there is no "last" — and it is stable,
// which is all the answer has to be when nobody has expressed a preference yet.
//
// Neither is the order the picker draws in. [Store.TenantsForIdentity] sorts by
// name because that is how somebody scans a list, and which one was landed in is
// *marked* rather than sorted first — so there is nothing here for the two
// orderings to disagree about.
func (s *Service) accountFor(ctx context.Context, tenantID, identityID uuid.UUID) (*Account, error) {
	if tenantID != uuid.Nil {
		return s.cfg.Store.AccountForIdentity(ctx, tenantID, identityID)
	}

	// One query for the common case, and it is the common case: everybody past
	// their first sign-in has a session in their history.
	last, err := s.cfg.Store.LastAccountForIdentity(ctx, identityID)
	if err != nil {
		return nil, err
	}
	if last != nil {
		return last, nil
	}

	accounts, err := s.cfg.Store.AccountsForIdentity(ctx, identityID)
	if err != nil {
		return nil, err
	}
	for _, a := range accounts {
		// A disabled account is not somewhere to land. Skipping it beats signing
		// somebody in and refusing everything they then try.
		if a.IsActive {
			return a, nil
		}
	}
	return nil, nil
}

// signInAttempt is what an audit entry needs about a sign-in regardless of how
// the person proved who they are.
//
// It exists because the tail of a sign-in is shared between a code, a provider
// and an invitation, and threading any one of their inputs through it would have
// made the shared half of the flow depend on that one's shape.
type signInAttempt struct {
	tenantID  uuid.UUID
	email     string
	ipAddress string
	userAgent string
	method    string
}

// entry fills in what every sign-in entry carries.
func (at signInAttempt) entry(e authlog.Entry) authlog.Entry {
	e.EmailAddress = at.email
	e.IPAddress, e.UserAgent = at.ipAddress, at.userAgent
	// Only when there is one. A sign-in that named no tenant has no tenant to
	// record, and the entry still has to be written: it is what the lockout counts.
	if at.tenantID != uuid.Nil {
		e.TenantID = &at.tenantID
	}
	if at.method != "" {
		if e.Detail == nil {
			e.Detail = map[string]any{}
		}
		e.Detail["method"] = at.method
	}
	return e
}

// failSignIn records a refused sign-in.
//
// The reason goes in the detail, never in the response. An operator reading the
// log needs to know the difference between a wrong code and a disabled account;
// the person at the keyboard is told the same thing either way.
func (s *Service) failSignIn(ctx context.Context, at signInAttempt, acct *Account, reason string) {
	e := at.entry(authlog.Entry{
		Event: authlog.EventLoginFailed, Outcome: authlog.Failed,
		Detail: map[string]any{"reason": reason},
	})
	if acct != nil {
		e.AccountID = &acct.ID
	}
	s.write(ctx, e)
}

// Logout ends one session.
func (s *Service) Logout(ctx context.Context, rootTokenID uuid.UUID) error {
	return s.cfg.Sessions.Revoke(ctx, rootTokenID)
}

// Refresh exchanges a refresh token for a new pair.
//
// The rate limit is on the session family rather than the account: a client
// looping on refresh is one client misbehaving, and throttling the account
// would take down the person's other devices along with it.
func (s *Service) Refresh(ctx context.Context, presented string) (session.Pair, error) {
	return s.cfg.Sessions.Rotate(ctx, presented)
}

// SendEmailVerification mints a confirmation link for the person behind an
// account.
//
// The account is what a caller has — it is what their claims name — and the
// address being confirmed is the identity's, so confirming it once counts in
// every tenant they belong to.
func (s *Service) SendEmailVerification(ctx context.Context, tenantID, accountID uuid.UUID) error {
	decision, err := s.cfg.Limiter.Allow(ctx,
		throttle.Check{Limit: s.cfg.Limits.VerificationResend, Key: throttle.Account(accountID.String())})
	if err != nil {
		return err
	}
	if !decision.Allowed {
		return decision.Err()
	}

	acct, ident, err := s.person(ctx, tenantID, accountID)
	if err != nil {
		return err
	}
	if ident.Verified() {
		// Not an error. Somebody clicking "resend" on an address that is
		// already confirmed has got what they wanted.
		return nil
	}

	s.write(ctx, authlog.Entry{
		Event: authlog.EventVerificationResent, Outcome: authlog.Succeeded,
		TenantID: &acct.TenantID, AccountID: &acct.ID,
		EmailAddress: normalizeEmail(ident.EmailAddress),
	})
	_, err = s.deliver(ctx, ident, nil, KindEmailVerification, s.cfg.VerificationTTL)
	return err
}

// VerifyEmail confirms an address from a link.
func (s *Service) VerifyEmail(ctx context.Context, token string) error {
	v, ident, err := s.redeem(ctx, token, KindEmailVerification)
	if err != nil {
		return err
	}

	now := s.now()
	if err := s.cfg.Store.InTx(ctx, func(ctx context.Context) error {
		consumed, err := s.consume(ctx, v)
		if err != nil {
			return err
		}
		if !consumed {
			return rigerr.BadRequest("this link has already been used")
		}
		return s.cfg.Store.MarkIdentityVerified(ctx, ident.ID, now)
	}); err != nil {
		return err
	}

	s.write(ctx, authlog.Entry{
		Event: authlog.EventEmailVerified, Outcome: authlog.Succeeded,
		EmailAddress: normalizeEmail(ident.EmailAddress),
	})
	return nil
}

// ImpersonateInput starts a session as somebody else.
type ImpersonateInput struct {
	TenantID uuid.UUID
	// AdministratorID is who is really doing this. It travels with the session
	// through every rotation and lands in every audit entry the session writes.
	AdministratorID uuid.UUID
	AccountID       uuid.UUID

	IPAddress string
	UserAgent string
}

// Impersonate issues a session for another account, marked as what it is.
//
// The permission check belongs to the caller: this package does not know what
// an administrator is. What it guarantees is that the session cannot pretend to
// be an ordinary one, and that both ends are recorded.
func (s *Service) Impersonate(ctx context.Context, in ImpersonateInput) (session.Pair, error) {
	acct, err := s.cfg.Store.FindByID(ctx, in.TenantID, in.AccountID)
	if err != nil {
		return session.Pair{}, err
	}
	if acct == nil {
		return session.Pair{}, rigerr.NotFound("no account with that identifier")
	}

	admin := in.AdministratorID
	pair, err := s.cfg.Sessions.Issue(ctx, session.IssueInput{
		TenantID:                acct.TenantID,
		AccountID:               acct.ID,
		Client:                  session.ClientWeb,
		IPAddress:               in.IPAddress,
		UserAgent:               in.UserAgent,
		ImpersonatedByAccountID: &admin,
	})
	if err != nil {
		return session.Pair{}, err
	}

	s.write(ctx, authlog.Entry{
		Event: authlog.EventImpersonationStarted, Outcome: authlog.Succeeded,
		TenantID: &acct.TenantID, AccountID: &acct.ID,
		IPAddress: in.IPAddress, UserAgent: in.UserAgent,
		TokenRootID: &pair.RootTokenID,
		Detail:      map[string]any{"administrator_account_id": admin.String()},
	})
	return pair, nil
}

// EndImpersonation revokes an impersonating session and records that it ended.
func (s *Service) EndImpersonation(ctx context.Context, tok *session.Token) error {
	if tok.ImpersonatedByAccountID == nil {
		return rigerr.BadRequest("this session is not an impersonation")
	}
	if err := s.cfg.Sessions.Revoke(ctx, tok.RootTokenID); err != nil {
		return err
	}

	s.write(ctx, authlog.Entry{
		Event: authlog.EventImpersonationEnded, Outcome: authlog.Succeeded,
		TenantID: &tok.TenantID, AccountID: &tok.AccountID,
		TokenRootID: &tok.RootTokenID,
		Detail:      map[string]any{"administrator_account_id": tok.ImpersonatedByAccountID.String()},
	})
	return nil
}

// person resolves an account and the identity behind it.
//
// Every flow that is about the person rather than the membership needs both:
// the account is what a caller names and what a log entry records, and the
// identity is what an address belongs to. A service account has no identity, so
// asking for one is a mistake worth naming rather than a nil to trip over
// later.
func (s *Service) person(ctx context.Context, tenantID, accountID uuid.UUID) (*Account, *Identity, error) {
	acct, err := s.cfg.Store.FindByID(ctx, tenantID, accountID)
	if err != nil {
		return nil, nil, err
	}
	if acct == nil {
		return nil, nil, rigerr.NotFound("no account with that identifier")
	}
	if acct.IdentityID == nil {
		return nil, nil, rigerr.BadRequest("a service account is not a person; revoke its key instead")
	}

	ident, err := s.cfg.Store.FindIdentityByID(ctx, *acct.IdentityID)
	if err != nil {
		return nil, nil, err
	}
	if ident == nil {
		// The account points at somebody who is not there. Not a request
		// problem, and not something to paper over: it means a delete went
		// through that should not have.
		return nil, nil, rigerr.Internal(nil, "account %s has no identity", acct.ID)
	}
	return acct, ident, nil
}

// RevokeEverySession ends every session a person has, in every tenant they
// belong to.
//
// What "sign me out everywhere" means, and the only way to reach it: a person is
// global and their sessions are not, so anything narrower leaves somebody signed
// in to the tenants they were not looking at. [github.com/simonjanss/rig/auth/session.Manager.RevokeAll]
// wants a tenant and an account, and finding every pair a person has is exactly
// what this does.
//
// Exported because the decision to use it is an application's. rig calls it
// nowhere: there is no credential left to change, and ending every session
// because somebody signed in with a code would make the second device sign the
// first one out.
func (s *Service) RevokeEverySession(ctx context.Context, identityID uuid.UUID) error {
	accts, err := s.cfg.Store.AccountsForIdentity(ctx, identityID)
	if err != nil {
		return err
	}
	for _, a := range accts {
		if err := s.cfg.Sessions.RevokeAll(ctx, a.TenantID, a.ID); err != nil {
			return err
		}
	}
	return nil
}

// mintSecret is the plaintext a kind of row carries and the hash that is stored
// in its place.
//
// It is separate from the row so that the queue can write the row now and make
// the secret later — see [Outbox] for why a queued row cannot carry its own
// secret. The inline path is the two called together, which is what
// mintVerification is, so there is one code path rather than a copy.
//
// Two kinds of secret, and the switch is the whole of the difference between
// them: a link carries a token nobody could guess, and a sign-in code carries
// six digits somebody has to read out loud.
func (s *Service) mintSecret(kind VerificationKind, ident *Identity) (secret string, hash []byte, err error) {
	if kind == KindEmailCode {
		return s.mintCode(ident)
	}
	return s.mintToken()
}

// mintToken is thirty-two random bytes, the hash that is stored, and the
// plaintext that is not.
func (s *Service) mintToken() (token string, hash []byte, err error) {
	raw := make([]byte, tokenBytes)
	if _, err := cryptorand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("account: generate token: %w", err)
	}
	sum := sha256.Sum256(raw)
	return tokenEncoding.EncodeToString(raw), sum[:], nil
}

// newVerification writes the row.
//
// A nil hash is one that has been queued and not yet sent: the secret does not
// exist yet, and nothing can reach the row by token because every lookup of that
// shape is an equality against token_hash and equality against NULL is never
// true.
func (s *Service) newVerification(
	ctx context.Context, ident *Identity, invite *pendingInvite,
	kind VerificationKind, ttl time.Duration, hash []byte,
) (*Verification, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("account: generate verification id: %w", err)
	}

	now := s.now()
	v := &Verification{
		ID:         id,
		IdentityID: ident.ID,
		Kind:       kind,
		TokenHash:  hash,
		CreatedAt:  now,
		ExpiresAt:  now.Add(ttl),
	}
	if invite != nil {
		tenantID, role := invite.TenantID, invite.Role
		v.InvitedToTenantID = &tenantID
		v.InvitedRole = &role
		v.InvitedDisplayName = invite.DisplayName
		v.InvitedByAccountID = invite.ByAccountID
		v.InvitedByAPIKeyID = invite.ByAPIKeyID
	}
	if err := s.cfg.Store.CreateVerification(ctx, v); err != nil {
		return nil, err
	}
	return v, nil
}

// deliver is the one place the queued and inline paths differ, and every caller
// that mints a secret goes through it.
//
// invite is set for an invitation and nil for everything else — it is the one
// kind that is about a tenant rather than about a person, and it is what the
// mail has to name. It carries no account, because on this path there is not
// one yet: accepting is what creates it.
//
// It returns the row so that a caller can answer with what it wrote. An
// invitation is the one that needs to — a listing row has to come back to
// whoever sent it — and on the queued path the row is all there is, the secret
// not existing until the dispatcher makes it.
func (s *Service) deliver(
	ctx context.Context, ident *Identity, invite *pendingInvite, kind VerificationKind, ttl time.Duration,
) (*Verification, error) {
	if s.cfg.Outbox == nil {
		secret, hash, err := s.mintSecret(kind, ident)
		if err != nil {
			return nil, err
		}
		v, err := s.newVerification(ctx, ident, invite, kind, ttl, hash)
		if err != nil {
			return nil, err
		}

		// Read back rather than assembled, so that the mail is handed the same
		// shape on both paths: the tenant's name comes from a join and this is
		// the one place that has it.
		var inv *Invitation
		if kind == KindInvitation {
			if inv, err = s.cfg.Store.InvitationByID(ctx, v.ID); err != nil {
				return nil, err
			}
		}
		if err := s.notify(ctx, kind, ident, inv, secret); err != nil {
			return nil, err
		}
		return v, nil
	}

	// Both writes together, and in the caller's transaction when there is one.
	// A verification without its delivery is an orphan nobody will ever mail —
	// invisible, except as an invitation in a listing that was never sent. InTx
	// is re-entrant, so this joins rather than nests.
	var out *Verification
	err := s.cfg.Store.InTx(ctx, func(ctx context.Context) error {
		v, err := s.newVerification(ctx, ident, invite, kind, ttl, nil)
		if err != nil {
			return err
		}
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("account: generate delivery id: %w", err)
		}
		out = v
		return s.cfg.Outbox.Enqueue(ctx, &Delivery{
			ID:             id,
			VerificationID: v.ID,
			Kind:           kind,
			State:          DeliveryPending,
			DeliverAt:      s.now(),
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// pendingInvite is what an invitation's row carries beyond the secret: the
// tenant, the role, the name, and who asked.
//
// Its own type rather than four parameters, because they travel together
// everywhere and three of the four are pointers — a call site with four
// positional arguments of those shapes is a call site somebody transposes.
type pendingInvite struct {
	TenantID    uuid.UUID
	Role        Role
	DisplayName string
	ByAccountID *uuid.UUID
	ByAPIKeyID  *uuid.UUID
}

// errInvalidLink is the answer to every way a link can be wrong.
//
// A link that expired, one that was already used, and one somebody invented are
// all the same sentence, because knowing which would tell an attacker whether
// they had guessed a real one.
var errInvalidLink = rigerr.BadRequest("this link is not valid or has expired")

// redeem resolves a link token to its row and the person it is for.
//
// It reads and checks; consuming is the caller's, inside the transaction that
// acts on what it found.
func (s *Service) redeem(ctx context.Context, token string, kind VerificationKind) (*Verification, *Identity, error) {
	v, ident, err := s.findByToken(ctx, token, kind)
	if err != nil {
		return nil, nil, err
	}
	if !v.Usable(s.now()) {
		return nil, nil, errInvalidLink
	}
	return v, ident, nil
}

// findByToken is redeem without the usability check.
//
// The split exists for [Service.PreviewInvitation], which answers "what is this
// link for" and must not act on it. Keeping the lookup here rather than copying
// it is what stops the preview quietly acquiring the half that consumes: there
// is one decoder, one hash, and one place that turns a secret into a row.
//
// It still refuses the wrong kind and an identity that is gone, because neither
// is a state a caller can do anything sensible with.
func (s *Service) findByToken(ctx context.Context, token string, kind VerificationKind) (*Verification, *Identity, error) {
	hash, ok := tokenHash(token)
	if !ok {
		return nil, nil, errInvalidLink
	}

	v, err := s.cfg.Store.VerificationByHash(ctx, hash)
	if err != nil {
		return nil, nil, err
	}
	if v == nil || v.Kind != kind {
		return nil, nil, errInvalidLink
	}

	ident, err := s.cfg.Store.FindIdentityByID(ctx, v.IdentityID)
	if err != nil {
		return nil, nil, err
	}
	if ident == nil {
		return nil, nil, errInvalidLink
	}
	return v, ident, nil
}

func (s *Service) consume(ctx context.Context, v *Verification) (bool, error) {
	return s.cfg.Store.ConsumeVerification(ctx, v.ID, s.now())
}

func (s *Service) write(ctx context.Context, e authlog.Entry) {
	if e.At.IsZero() {
		e.At = s.now()
	}
	s.cfg.Log.Write(ctx, e)
}

func (s *Service) minDuration() time.Duration {
	if s.cfg.MinDuration != 0 {
		return s.cfg.MinDuration
	}
	return DefaultMinDuration
}

// tokenHash turns a presented token into the value stored in its place, and
// reports whether it could be one at all.
//
// One decoder, because two places that have to agree about an encoding is one
// place too many: [Service.findByToken] and [Service.PreviewInvitation] both
// start here.
func tokenHash(token string) ([]byte, bool) {
	raw, err := tokenEncoding.DecodeString(strings.ToUpper(strings.TrimSpace(token)))
	if err != nil || len(raw) != tokenBytes {
		return nil, false
	}
	sum := sha256.Sum256(raw)
	return sum[:], true
}

// MaskEmail hides most of an address while leaving it recognisable.
//
// "bo@school.example" becomes "b***@school.example". The domain is untouched,
// deliberately: it is what tells somebody which of their addresses a link is
// for, and it is usually the tenant's own domain, which whatever is showing this
// has already named. A one-character local part becomes "*" rather than itself.
//
// Exported because an application rendering its own landing page should mask the
// way rig does rather than invent a second format for the same field. What it is
// for is [Service.PreviewInvitation]: the holder of an invitation link is not yet
// proven to be its addressee, mail gets forwarded, and the unmasked form would
// turn a leaked link into a confirmed address.
func MaskEmail(address string) string {
	local, domain, found := strings.Cut(strings.TrimSpace(address), "@")
	if !found || local == "" {
		return "***"
	}
	if len(local) == 1 {
		return "*@" + domain
	}
	return local[:1] + "***@" + domain
}

// normalizeEmail is how an address is compared and counted.
//
// Trim first, because a pasted address regularly arrives with a trailing space
// and being unable to sign in over one is maddening.
func normalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// sleepUntil waits, unless the request has already gone away.
func sleepUntil(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-t.C:
	case <-ctx.Done():
	}
}
