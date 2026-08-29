// Package account implements the sign-in flows.
//
// Everything here exists to get a handful of details right that are easy to get
// wrong and expensive to get wrong:
//
//   - The lockout is checked before the password is verified, so a locked
//     request neither burns an argon2 hash nor extends its own window.
//   - A login for an address with no account still spends the time an argon2
//     hash costs, so response time is not a membership oracle.
//   - A wrong password and a disabled account are told apart only after the
//     password is verified, so "this account is disabled" cannot be used to
//     enumerate accounts.
//   - A password reset answers the same way whether or not the address exists.
//   - Setting a new password ends every session, because a change made because
//     somebody else knows your password is not a change if they stay signed in.
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
	"github.com/simonjanss/rig/auth/password"
	"github.com/simonjanss/rig/auth/session"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/throttle"
)

// ErrInvalidCredentials is the answer to every failed sign-in.
//
// One error for a wrong password and for an address nobody has ever registered,
// because the difference is exactly what an attacker is trying to learn.
var ErrInvalidCredentials = rigerr.Unauthorized("the email address or password is not correct")

// Defaults.
const (
	// DefaultMinDuration pads a login. Verifying a real hash and verifying the
	// dummy take about the same time, but "about" is measurable over enough
	// samples; a floor makes the remaining difference noise.
	DefaultMinDuration = 750 * time.Millisecond
	// DefaultResetTTL is short: a reset link is a live credential.
	DefaultResetTTL = time.Hour
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
	Hasher     *password.Hasher
	Policy     password.Policy
	Log        authlog.Log
	Notifier   Notifier

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

	// OnRegistered runs inside the transaction that creates a self-registered
	// identity — after the person and their credential exist, before the
	// identity session is issued. Returning an error rolls the whole sign-up
	// back, so a retry is a clean retry rather than a conflict with a half-made
	// account.
	//
	// It receives the service because the ordinary body is a call back into it —
	// [Service.Provision] with Invite set, bringing the newcomer into a starter
	// tenant — and the closure is handed to [New] before the service exists.
	// Reach the transaction itself with dbx.Tx(ctx), the same way a generated
	// repository does.
	//
	// Nil, the default, registers the person and nothing else.
	OnRegistered func(ctx context.Context, accounts *Service, in Registered) error

	// Limiter and Limits are what stop somebody guessing. A service built
	// without a limiter refuses to exist: a login endpoint with no lockout is
	// not a login endpoint, it is a password oracle with a queue.
	Limiter *throttle.Limiter
	Limits  throttle.Defaults

	MinDuration     time.Duration
	ResetTTL        time.Duration
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
			"a login endpoint with no lockout is a password oracle with a queue")
	}

	if cfg.Hasher == nil {
		cfg.Hasher = password.New(password.DefaultParams())
	}
	if cfg.Policy.MinLength == 0 {
		cfg.Policy = password.DefaultPolicy()
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
	if cfg.ResetTTL == 0 {
		cfg.ResetTTL = DefaultResetTTL
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

// LoginInput is a sign-in attempt.
type LoginInput struct {
	// TenantID says which tenant the session is for, and may be left empty.
	//
	// Set it when the application already knows — a subdomain, a header, a path
	// segment — and the sign-in is refused unless the person belongs to that
	// tenant. Leave it empty and the person is signed in to one of their own
	// tenants, which is what a single sign-in page wants: nobody knows which
	// tenants an address belongs to until the password is checked, so asking
	// first is asking a question the visitor cannot answer.
	//
	// Either way the address is not looked up in a tenant: an address is one
	// person across every tenant, and the tenant only decides which of that
	// person's accounts the session belongs to.
	TenantID     uuid.UUID
	EmailAddress string
	Password     string

	Remember  bool
	Client    session.Client
	IPAddress string
	UserAgent string
}

// Login verifies a password and starts a session.
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
	// in. When a tenant was named it is that one; otherwise the oldest.
	Session *session.Pair
	// Identity is always issued, including alongside a session: signing in and
	// then switching tenants is one flow, and the picker needs a credential
	// that outlives whichever tenant was landed on.
	Identity session.Issued
	// Tenants are every one this person belongs to, so a client can draw the
	// picker without a second call.
	Tenants []Membership
}

// Login signs somebody in.
func (s *Service) Login(ctx context.Context, in LoginInput) (SignInResult, error) {
	started := s.now()
	email := normalizeEmail(in.EmailAddress)

	pair, err := s.login(ctx, in, email)

	// The pad runs on both paths. Padding only failures would make success the
	// fast answer, which is the same oracle in reverse.
	if d := s.minDuration(); d > 0 {
		s.sleep(ctx, d-s.now().Sub(started))
	}
	return pair, err
}

func (s *Service) login(ctx context.Context, in LoginInput, email string) (SignInResult, error) {
	// Before the password, before the account lookup, before anything
	// expensive. A locked request that still ran argon2 would let an attacker
	// keep the server busy for free, and a locked request that still recorded a
	// failure would keep extending its own lockout.
	decision, err := s.cfg.Limiter.Allow(ctx,
		throttle.Check{Limit: s.cfg.Limits.LoginByEmail, Key: throttle.Email(email)},
		throttle.Check{Limit: s.cfg.Limits.LoginByIP, Key: throttle.IP(in.IPAddress)},
	)
	if err != nil {
		return SignInResult{}, err
	}
	if !decision.Allowed {
		locked := authlog.Entry{
			Event: authlog.EventAccountLocked, Outcome: authlog.Failed,
			EmailAddress: email,
			IPAddress:    in.IPAddress, UserAgent: in.UserAgent,
			Detail: map[string]any{"limit": decision.Limit.Name},
		}
		if in.TenantID != uuid.Nil {
			locked.TenantID = &in.TenantID
		}
		s.write(ctx, locked)
		return SignInResult{}, decision.Err()
	}

	// The person, found without reference to the tenant. The password is theirs
	// and so is the address; which tenant this session is for comes next.
	ident, err := s.cfg.Store.FindIdentityByEmail(ctx, email)
	if err != nil {
		return SignInResult{}, err
	}

	// The hash to check against, real or not. Skipping the work for an unknown
	// address is what turns response time into a list of your customers.
	stored := s.cfg.Hasher.Dummy()
	if ident != nil {
		cred, err := s.cfg.Store.Credential(ctx, ident.ID)
		if err != nil {
			return SignInResult{}, err
		}
		if cred != nil {
			stored = cred.PasswordHash
		}
	}

	ok, needsRehash, err := s.cfg.Hasher.Verify(stored, in.Password)
	if err != nil && !errors.Is(err, password.ErrMalformed) {
		return SignInResult{}, err
	}
	if !ok || ident == nil {
		s.failLogin(ctx, in, email, nil, "wrong credentials")
		return SignInResult{}, ErrInvalidCredentials
	}

	// Only now, with the password confirmed, is it safe to say anything about
	// the person. Refusing a disabled one before this point would answer
	// "disabled" to anybody who guessed the address.
	if !ident.IsActive {
		s.failLogin(ctx, in, email, nil, "identity disabled")
		return SignInResult{}, rigerr.Forbidden("this account has been disabled")
	}
	if s.cfg.RequireVerifiedEmail && !ident.Verified() {
		s.failLogin(ctx, in, email, nil, "email not verified")
		return SignInResult{}, rigerr.Forbidden("confirm your email address before signing in")
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
		s.failLogin(ctx, in, email, nil, "no account in this tenant")
		return SignInResult{}, rigerr.Forbidden("you do not have access to this tenant")
	}
	if acct != nil && !acct.IsActive {
		s.failLogin(ctx, in, email, acct, "account disabled")
		return SignInResult{}, rigerr.Forbidden("this account has been disabled")
	}

	// A backstop rather than the rule. A service account has no identity, so the
	// lookup above cannot reach one in the schema rig ships — the CHECK on
	// account makes sure of it, and an integration's address resolves to nobody
	// at all, which is a better answer than this one. This stays for a schema
	// where that constraint was dropped, because the consequence of being wrong
	// is a way in that nobody is watching.
	if acct != nil && acct.Kind == KindService {
		s.failLogin(ctx, in, email, acct, "service account")
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
		// the password was right, and where they go next is the picker's problem.
		s.write(ctx, authlog.Entry{
			Event: authlog.EventLoginSucceeded, Outcome: authlog.Succeeded,
			EmailAddress: email, IPAddress: in.IPAddress, UserAgent: in.UserAgent,
			Detail: map[string]any{"tenants": 0},
		})
		return out, nil
	}

	if needsRehash {
		// Best effort. This is the only moment the plaintext exists, but
		// failing the login over a bookkeeping write would be worse than
		// leaving the hash where it is.
		_ = s.storePassword(ctx, ident, in.Password)
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

	s.write(ctx, authlog.Entry{
		Event: authlog.EventLoginSucceeded, Outcome: authlog.Succeeded,
		TenantID: &acct.TenantID, AccountID: &acct.ID, EmailAddress: email,
		IPAddress: in.IPAddress, UserAgent: in.UserAgent,
		TokenRootID: &pair.RootTokenID,
	})
	return out, nil
}

// accountFor is the account a sign-in is for.
//
// Named tenant: that one, or nothing. No tenant: whichever tenant they joined
// first, skipping any they have been removed from. First rather than last because
// it is stable — somebody's oldest tenant does not change under them — and
// because the interface puts the rest one click away, so the choice only has to be
// predictable rather than clever.
func (s *Service) accountFor(ctx context.Context, tenantID, identityID uuid.UUID) (*Account, error) {
	if tenantID != uuid.Nil {
		acct, err := s.cfg.Store.AccountForIdentity(ctx, tenantID, identityID)
		if err != nil || acct == nil {
			return nil, err
		}
		return acct, nil
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

// failLogin records a refused sign-in.
//
// The reason goes in the detail, never in the response. An operator reading the
// log needs to know the difference between a wrong password and a disabled
// account; the person at the keyboard is told the same thing either way.
func (s *Service) failLogin(ctx context.Context, in LoginInput, email string, acct *Account, reason string) {
	e := authlog.Entry{
		Event: authlog.EventLoginFailed, Outcome: authlog.Failed,
		EmailAddress: email,
		IPAddress:    in.IPAddress, UserAgent: in.UserAgent,
		Detail: map[string]any{"reason": reason},
	}
	// Only when there is one. A sign-in that named no tenant has no tenant to
	// record, and the entry still has to be written: it is what the lockout counts.
	if in.TenantID != uuid.Nil {
		e.TenantID = &in.TenantID
	}
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

// RequestPasswordReset mints a reset link and hands it to the notifier.
//
// It answers the same way whether or not the address exists, and it records the
// attempt either way — which is what makes the rate limit work: an attacker
// hammering addresses to see which ones respond differently gets the same
// answer every time and runs out of budget doing it.
//
// The tenant is only for the log entry. A password belongs to the person, so
// somebody who has forgotten theirs is not asking about one tenant, and
// answering "no such address here" from the wrong subdomain would be a puzzle
// with no solution.
func (s *Service) RequestPasswordReset(ctx context.Context, tenantID uuid.UUID, emailAddress, ip string) error {
	email := normalizeEmail(emailAddress)

	decision, err := s.cfg.Limiter.Allow(ctx,
		throttle.Check{Limit: s.cfg.Limits.PasswordReset, Key: throttle.Email(email)})
	if err != nil {
		return err
	}
	if !decision.Allowed {
		return decision.Err()
	}

	ident, err := s.cfg.Store.FindIdentityByEmail(ctx, email)
	if err != nil {
		return err
	}

	entry := authlog.Entry{
		Event: authlog.EventPasswordResetRequested, Outcome: authlog.Failed,
		TenantID: &tenantID, EmailAddress: email, IPAddress: ip,
	}
	if ident == nil {
		// Recorded as a failure and answered as a success. The record is for
		// the rate limit; the answer is so the caller learns nothing.
		s.write(ctx, entry)
		return nil
	}

	entry.Outcome = authlog.Succeeded

	s.write(ctx, entry)

	return s.deliver(ctx, ident, nil, KindPasswordReset, s.cfg.ResetTTL)
}

// ConfirmPasswordReset sets a new password from a reset link.
func (s *Service) ConfirmPasswordReset(ctx context.Context, token, newPassword, ip string) error {
	v, ident, err := s.redeem(ctx, token, KindPasswordReset)
	if err != nil {
		return err
	}
	if err := s.cfg.Policy.Check(ctx, newPassword); err != nil {
		return err
	}

	if err := s.cfg.Store.InTx(ctx, func(ctx context.Context) error {
		if err := s.storePassword(ctx, ident, newPassword); err != nil {
			return err
		}
		consumed, err := s.consume(ctx, v)
		if err != nil {
			return err
		}
		if !consumed {
			// Two requests raced for the same link and the other one won.
			return rigerr.BadRequest("this link has already been used")
		}
		return nil
	}); err != nil {
		return err
	}

	// Every session, in every tenant. A reset happens because somebody has lost
	// control of their account; ending only the sessions of the tenant the
	// link was clicked from would leave whoever took it signed in to the others.
	if err := s.revokeEverySession(ctx, ident.ID); err != nil {
		return err
	}

	s.write(ctx, authlog.Entry{
		Event: authlog.EventPasswordResetCompleted, Outcome: authlog.Succeeded,
		EmailAddress: normalizeEmail(ident.EmailAddress), IPAddress: ip,
	})
	return nil
}

// ChangePasswordInput is a deliberate password change.
type ChangePasswordInput struct {
	TenantID  uuid.UUID
	AccountID uuid.UUID

	CurrentPassword string
	NewPassword     string

	Client    session.Client
	IPAddress string
	UserAgent string
}

// ChangePassword replaces a password for somebody who knows the old one, and
// returns a fresh session.
//
// The old sessions are all revoked, including the one making the request, and a
// new pair is issued in their place. Anything else forces a choice between
// signing the person out of the tab they are looking at and leaving a thief
// signed in on another continent.
func (s *Service) ChangePassword(ctx context.Context, in ChangePasswordInput) (session.Pair, error) {
	acct, ident, err := s.person(ctx, in.TenantID, in.AccountID)
	if err != nil {
		return session.Pair{}, err
	}

	cred, err := s.cfg.Store.Credential(ctx, ident.ID)
	if err != nil {
		return session.Pair{}, err
	}
	if cred == nil {
		return session.Pair{}, rigerr.BadRequest(
			"this account has no password yet; use the reset link to set one")
	}

	ok, _, err := s.cfg.Hasher.Verify(cred.PasswordHash, in.CurrentPassword)
	if err != nil && !errors.Is(err, password.ErrMalformed) {
		return session.Pair{}, err
	}
	if !ok {
		return session.Pair{}, rigerr.Unauthorized("the current password is not correct")
	}
	if err := s.cfg.Policy.Check(ctx, in.NewPassword); err != nil {
		return session.Pair{}, err
	}
	if err := s.storePassword(ctx, ident, in.NewPassword); err != nil {
		return session.Pair{}, err
	}
	if err := s.revokeEverySession(ctx, ident.ID); err != nil {
		return session.Pair{}, err
	}

	pair, err := s.cfg.Sessions.Issue(ctx, session.IssueInput{
		TenantID:  acct.TenantID,
		AccountID: acct.ID,
		Client:    in.Client,
		IPAddress: in.IPAddress,
		UserAgent: in.UserAgent,
	})
	if err != nil {
		return session.Pair{}, err
	}

	s.write(ctx, authlog.Entry{
		Event: authlog.EventPasswordChanged, Outcome: authlog.Succeeded,
		TenantID: &acct.TenantID, AccountID: &acct.ID,
		EmailAddress: normalizeEmail(acct.EmailAddress),
		IPAddress:    in.IPAddress, UserAgent: in.UserAgent,
		TokenRootID: &pair.RootTokenID,
	})
	return pair, nil
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
	return s.deliver(ctx, ident, nil, KindEmailVerification, s.cfg.VerificationTTL)
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

// HasPassword reports whether a person has a password at all.
//
// Somebody who only ever signed in through a provider has none, and neither does
// somebody who has been invited and not yet arrived. It is here so that a caller
// setting a first password can tell the difference without reading the hash: an
// application that fetched the credential to check would be an application
// holding a hash it has no use for.
func (s *Service) HasPassword(ctx context.Context, identityID uuid.UUID) (bool, error) {
	cred, err := s.cfg.Store.Credential(ctx, identityID)
	if err != nil {
		return false, err
	}
	return cred != nil, nil
}

// SetPassword replaces a person's password without asking for the old one.
//
// It is for provisioning and for an administrator resetting somebody out of a
// hole — not for a self-service change, which is [Service.ChangePassword].
// Sessions are revoked either way, in every tenant.
func (s *Service) SetPassword(ctx context.Context, identityID uuid.UUID, newPassword string) error {
	ident, err := s.cfg.Store.FindIdentityByID(ctx, identityID)
	if err != nil {
		return err
	}
	if ident == nil {
		return rigerr.NotFound("nobody with that identifier")
	}
	if err := s.cfg.Policy.Check(ctx, newPassword); err != nil {
		return err
	}
	if err := s.storePassword(ctx, ident, newPassword); err != nil {
		return err
	}
	return s.revokeEverySession(ctx, identityID)
}

// person resolves an account and the identity behind it.
//
// Every flow that touches a credential needs both: the account is what a caller
// names and what a log entry records, and the identity is what the password
// belongs to. A service account has no identity, and asking for its password is
// a mistake worth naming rather than a nil to trip over later.
func (s *Service) person(ctx context.Context, tenantID, accountID uuid.UUID) (*Account, *Identity, error) {
	acct, err := s.cfg.Store.FindByID(ctx, tenantID, accountID)
	if err != nil {
		return nil, nil, err
	}
	if acct == nil {
		return nil, nil, rigerr.NotFound("no account with that identifier")
	}
	if acct.IdentityID == nil {
		return nil, nil, rigerr.BadRequest("a service account has no password; revoke its key instead")
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

// revokeEverySession ends every session a person has, in every tenant they
// belong to.
//
// One password covers every tenant, so anything less would leave a thief signed
// in to the tenants the person was not looking at when they changed it.
func (s *Service) revokeEverySession(ctx context.Context, identityID uuid.UUID) error {
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

// storePassword hashes and saves.
func (s *Service) storePassword(ctx context.Context, ident *Identity, plain string) error {
	c, err := s.cfg.Hasher.Hash(plain)
	if err != nil {
		return err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("account: generate credential id: %w", err)
	}

	return s.cfg.Store.SaveCredential(ctx, &Credential{
		ID:           id,
		IdentityID:   ident.ID,
		PasswordHash: c.Encoded,
		Algorithm:    c.Algorithm,
		Params:       c.Params,
		CreatedAt:    s.now(),
	})
}

// mintVerification creates a link and returns its token.
func (s *Service) mintVerification(ctx context.Context, ident *Identity, tenantID *uuid.UUID, kind VerificationKind, ttl time.Duration) (string, error) {
	token, hash, err := s.mintToken()
	if err != nil {
		return "", err
	}
	if _, err := s.newVerification(ctx, ident, tenantID, kind, ttl, hash); err != nil {
		return "", err
	}
	return token, nil
}

// mintToken is the secret half: thirty-two random bytes, the hash that is stored,
// and the plaintext that is not.
//
// It is separate from the row so that the queue can write the row now and make
// the secret later — see [Outbox] for why a queued link cannot carry its own
// token. The inline path is the two of them called together, which is what
// mintVerification is, so there is one code path rather than a copy.
func (s *Service) mintToken() (token string, hash []byte, err error) {
	raw := make([]byte, tokenBytes)
	if _, err := cryptorand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("account: generate token: %w", err)
	}
	sum := sha256.Sum256(raw)
	return tokenEncoding.EncodeToString(raw), sum[:], nil
}

// newVerification writes the link row.
//
// A nil hash is a link that has been queued and not yet sent: the secret does not
// exist yet, and nothing can reach the row by token because every lookup is an
// equality against token_hash and equality against NULL is never true.
func (s *Service) newVerification(ctx context.Context, ident *Identity, tenantID *uuid.UUID, kind VerificationKind, ttl time.Duration, hash []byte) (*Verification, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("account: generate verification id: %w", err)
	}

	now := s.now()
	v := &Verification{
		ID:                id,
		IdentityID:        ident.ID,
		InvitedToTenantID: tenantID,
		Kind:              kind,
		TokenHash:         hash,
		CreatedAt:         now,
		ExpiresAt:         now.Add(ttl),
	}
	if err := s.cfg.Store.CreateVerification(ctx, v); err != nil {
		return nil, err
	}
	return v, nil
}

// deliver is the one place the queued and inline paths differ, and every caller
// that mints a link goes through it.
//
// acct is only read for an invitation, which is the one link that is about a
// tenant rather than about a person.
func (s *Service) deliver(ctx context.Context, ident *Identity, acct *Account, kind VerificationKind, ttl time.Duration) error {
	var tenantID *uuid.UUID
	if acct != nil {
		id := acct.TenantID
		tenantID = &id
	}

	if s.cfg.Outbox == nil {
		token, err := s.mintVerification(ctx, ident, tenantID, kind, ttl)
		if err != nil {
			return err
		}
		return s.notify(ctx, kind, ident, acct, token)
	}

	// Both writes together, and in the caller's transaction when there is one.
	// A verification without its delivery is an orphan link nobody will ever
	// mail — invisible, except as an invitation in a listing that was never
	// sent. InTx is re-entrant, so this joins rather than nests.
	return s.cfg.Store.InTx(ctx, func(ctx context.Context) error {
		v, err := s.newVerification(ctx, ident, tenantID, kind, ttl, nil)
		if err != nil {
			return err
		}
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("account: generate delivery id: %w", err)
		}
		return s.cfg.Outbox.Enqueue(ctx, &Delivery{
			ID:             id,
			VerificationID: v.ID,
			Kind:           kind,
			State:          DeliveryPending,
			DeliverAt:      s.now(),
		})
	})
}

// redeem resolves a link token to its row and the person it is for.
//
// Every failure looks the same to the caller. A link that expired, a link that
// was already used, and a link somebody invented are all "this link is not
// valid", because knowing which would tell an attacker whether they had guessed
// a real one.
func (s *Service) redeem(ctx context.Context, token string, kind VerificationKind) (*Verification, *Identity, error) {
	invalid := rigerr.BadRequest("this link is not valid or has expired")

	raw, err := tokenEncoding.DecodeString(strings.ToUpper(strings.TrimSpace(token)))
	if err != nil || len(raw) != tokenBytes {
		return nil, nil, invalid
	}

	sum := sha256.Sum256(raw)
	v, err := s.cfg.Store.VerificationByHash(ctx, sum[:])
	if err != nil {
		return nil, nil, err
	}
	if v == nil || v.Kind != kind || !v.Usable(s.now()) {
		return nil, nil, invalid
	}

	ident, err := s.cfg.Store.FindIdentityByID(ctx, v.IdentityID)
	if err != nil {
		return nil, nil, err
	}
	if ident == nil {
		return nil, nil, invalid
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
