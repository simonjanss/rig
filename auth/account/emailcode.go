package account

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/authlog"
	"github.com/simonjanss/rig/auth/session"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/throttle"
)

// Defaults for the mailed-code sign-in.
const (
	// DefaultEmailCodeLength is six digits, which is what people expect to be
	// asked for and what fits in a glance. It is short enough to be guessable,
	// which is why [EmailCodeOptions.MaxAttempts] exists.
	DefaultEmailCodeLength = 6
	// DefaultEmailCodeTTL is ten minutes: long enough to find the mail on
	// another device, short enough that a live credential is not sitting in a
	// mailbox all afternoon.
	DefaultEmailCodeTTL = 10 * time.Minute
	// DefaultEmailCodeMaxAttempts is three, and it is deliberately well below
	// the five wrong sign-ins that lock an address.
	//
	// If the two matched, somebody who mistyped a code three or four times
	// would kill the code *and* lock themselves out of the address for fifteen
	// minutes — so the ordinary clumsy failure would be unrecoverable rather
	// than "that one is dead, ask for another". Three tries on a code you can
	// re-read from the mail in front of you is enough, and it leaves room
	// underneath the lockout for the second code to work.
	//
	// With six digits it is also a one in a third of a million chance of
	// guessing a particular code.
	DefaultEmailCodeMaxAttempts = 3

	// MinEmailCodeLength is the shortest code this package will mint. Below it
	// no attempt ceiling makes a code safe: four digits with five attempts is
	// one guess in two thousand, which is not a credential.
	MinEmailCodeLength = 6
	// MaxEmailCodeLength bounds the other end. Past this somebody is typing a
	// token, and a token belongs in a link.
	MaxEmailCodeLength = 10
)

// ErrInvalidCode is the answer to every way a code can be wrong.
//
// One error for a wrong code, an expired code, a code that was already used, a
// code that has been guessed at too many times, and an address rig has never
// heard of — because the difference between the last one and the rest is exactly
// what somebody probing for accounts is trying to learn. [Service.VerifyEmailCode]
// pads its own duration for the same reason.
var ErrInvalidCode = rigerr.Unauthorized("that code is not correct or has expired")

// EmailCodeOptions is the mailed-code sign-in: rig sends a short numeric code to
// an address and somebody types it back.
//
// The zero value is off, and off is the default. It is the way in for anybody
// with no account at a configured provider, which in most deployments is most
// people — but whether a deployment wants it is a decision, and so is whether a
// code may go to an address nobody has ever seen.
type EmailCodeOptions struct {
	// Enabled turns the flow on. [New] refuses it with no Notifier: a code
	// nobody receives is a door nobody can open.
	Enabled bool

	// Length is how many digits. Zero means [DefaultEmailCodeLength], and
	// anything between [MinEmailCodeLength] and [MaxEmailCodeLength] is
	// accepted.
	Length int
	// TTL is how long a code lasts. Zero means [DefaultEmailCodeTTL].
	TTL time.Duration
	// MaxAttempts is how many wrong guesses kill a code. Zero means
	// [DefaultEmailCodeMaxAttempts].
	//
	// It is a ceiling on one code rather than a rate limit on an address, and
	// the two are not substitutes: a limit counts failures over a rolling
	// window, so a fresh code arrives with the old code's failures still
	// counted and five fat-fingered attempts lock the address rather than
	// killing one code.
	MaxAttempts int

	// AllowProvisioning sends a code to an address rig has never seen, creating
	// the person when the code is asked for.
	//
	// Off by default, the way
	// [github.com/simonjanss/rig/auth/oauth.Config.AllowProvisioning] is and for
	// the same reason: it is the difference between a deployment anybody can get
	// an identity in and one where somebody has to be invited first. On, it is
	// self-registration — [Config.OnRegistered] runs in the transaction that
	// creates the person, which is the hook that puts a newcomer somewhere.
	//
	// The trade to know: with it on, a stranger typing an address creates a row
	// and runs that hook before proving anything. A starter tenant made this way
	// may belong to somebody who never types the code.
	AllowProvisioning bool
}

// resolveEmailCode fills in the numbers and refuses what cannot work.
//
// Refused at construction rather than found later, which is the argument
// resolveMail makes for the mail queue: the failure a check here prevents is a
// sign-in nobody can complete, and there is nothing in a running system that
// would point at the configuration.
func resolveEmailCode(cfg Config) (EmailCodeOptions, error) {
	c := cfg.EmailCode
	if !c.Enabled {
		// Nothing reads these when the flow is off, and resolving them anyway
		// would mean refusing a configuration nobody is using.
		return c, nil
	}

	if _, ok := cfg.Notifier.(NoNotifier); ok || cfg.Notifier == nil {
		return c, errors.New("account: EmailCode is enabled but no Notifier is, so every " +
			"code would be minted and then dropped and nobody could ever sign in; " +
			"set a Notifier, or leave EmailCode off")
	}

	if c.Length == 0 {
		c.Length = DefaultEmailCodeLength
	}
	if c.Length < MinEmailCodeLength || c.Length > MaxEmailCodeLength {
		return c, fmt.Errorf("account: EmailCode.Length is %d, and only %d to %d digits "+
			"are accepted: shorter is guessable whatever the attempt ceiling, and longer "+
			"is a token, which belongs in a link",
			c.Length, MinEmailCodeLength, MaxEmailCodeLength)
	}

	if c.TTL == 0 {
		c.TTL = DefaultEmailCodeTTL
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = DefaultEmailCodeMaxAttempts
	}
	return c, nil
}

// RequestEmailCode mails a short single-use code and answers the same way
// whether or not the address exists.
//
// The enumeration rule is the whole shape of it. An address rig has never seen
// either gets an identity and a code, when [EmailCodeOptions.AllowProvisioning]
// says so, or gets nothing at all — and in both cases the caller is told the
// same nothing, because whether somebody has an account here is not a stranger's
// business. The refusal that *is* reported is the rate limit, which is about the
// caller's behaviour rather than about the address.
//
// Any code already live for that person is revoked first. Three reasons: the
// newest code is then the one that works rather than probably the one that
// works, the table cannot be filled by asking repeatedly, and on the queued path
// the dispatcher finds the older row settled and marks its delivery skipped,
// which is exactly what [Outbox] documents.
//
// The tenant is only for the log entry. A code proves an address and an address
// belongs to the person, so which tenant they end up in is decided when they
// sign in rather than here.
func (s *Service) RequestEmailCode(ctx context.Context, in RequestEmailCodeInput) error {
	if !s.cfg.EmailCode.Enabled {
		return rigerr.NotFound("this deployment does not sign anybody in with a code")
	}

	email := normalizeEmail(in.EmailAddress)
	if email == "" || !strings.Contains(email, "@") {
		return rigerr.Invalid("%q is not an email address", in.EmailAddress)
	}

	// Unauthenticated, it sends mail, and with provisioning on it writes a row —
	// so it is limited before anything else, per address and per source. The
	// address limit stops somebody being mailed a code every second; the source
	// limit is the one that stops a script making ten thousand identities.
	decision, err := s.cfg.Limiter.Allow(ctx,
		throttle.Check{Limit: s.cfg.Limits.EmailCodeRequest, Key: throttle.Email(email)},
		throttle.Check{Limit: s.cfg.Limits.EmailCodeByIP, Key: throttle.IP(in.IPAddress)},
	)
	if err != nil {
		return err
	}
	if !decision.Allowed {
		return decision.Err()
	}

	entry := authlog.Entry{
		Event: authlog.EventEmailCodeRequested, Outcome: authlog.Succeeded,
		EmailAddress: email, IPAddress: in.IPAddress, UserAgent: in.UserAgent,
	}
	if in.TenantID != uuid.Nil {
		tenantID := in.TenantID
		entry.TenantID = &tenantID
	}

	ident, err := s.cfg.Store.FindIdentityByEmail(ctx, email)
	if err != nil {
		return err
	}

	if ident == nil && !s.cfg.EmailCode.AllowProvisioning {
		// Recorded as a failure and answered as a success, which is the same
		// pair of decisions this package makes everywhere: the row is what the
		// rate limit counts and what an operator reads, and the response is what
		// a stranger is told.
		entry.Outcome = authlog.Failed
		entry.Detail = map[string]any{"reason": "no such address"}
		s.write(ctx, entry)
		return nil
	}

	if ident != nil {
		if err := s.cfg.Store.InTx(ctx, func(ctx context.Context) error {
			return s.freshCode(ctx, ident)
		}); err != nil {
			return err
		}
		s.write(ctx, entry)
		return nil
	}

	// Nobody has this address, and this deployment lets one in. The person, the
	// application's hook and the code are one transaction: a hook that fails
	// rolls the whole thing back, so a retry is a clean retry rather than a
	// conflict with a half-made person.
	ident = &Identity{
		ID:           uuid.New(),
		EmailAddress: strings.TrimSpace(in.EmailAddress),
		DisplayName:  displayNameFor(in.EmailAddress),
		IsActive:     true,
	}
	if err := s.cfg.Store.InTx(ctx, func(ctx context.Context) error {
		if err := s.cfg.Store.InsertIdentity(ctx, ident); err != nil {
			return err
		}
		if s.cfg.OnRegistered != nil {
			if err := s.cfg.OnRegistered(ctx, s, Registered{
				IdentityID:   ident.ID,
				EmailAddress: ident.EmailAddress,
				DisplayName:  ident.DisplayName,
				IPAddress:    in.IPAddress,
				UserAgent:    in.UserAgent,
			}); err != nil {
				return err
			}
		}
		return s.freshCode(ctx, ident)
	}); err != nil {
		return err
	}

	s.write(ctx, authlog.Entry{
		Event: authlog.EventAccountProvisioned, Outcome: authlog.Succeeded,
		EmailAddress: email, IPAddress: in.IPAddress, UserAgent: in.UserAgent,
		Detail: map[string]any{"self_registered": true},
	})
	s.write(ctx, entry)
	return nil
}

// RequestEmailCodeInput asks for a code.
type RequestEmailCodeInput struct {
	// TenantID is only for the audit entry, and may be left empty: a code is
	// about an address rather than about a membership.
	TenantID     uuid.UUID
	EmailAddress string

	IPAddress string
	UserAgent string
}

// freshCode revokes whatever code this person has live and mints another.
func (s *Service) freshCode(ctx context.Context, ident *Identity) error {
	live, err := s.cfg.Store.LiveVerification(ctx, ident.ID, KindEmailCode)
	if err != nil {
		return err
	}
	if live != nil {
		if _, err := s.cfg.Store.RevokeVerification(ctx, live.ID, s.now()); err != nil {
			return err
		}
	}
	_, err = s.deliver(ctx, ident, nil, KindEmailCode, s.cfg.EmailCode.TTL)
	return err
}

// VerifyEmailCodeInput is a sign-in with a mailed code.
type VerifyEmailCodeInput struct {
	// TenantID says which tenant the session is for, and may be left empty —
	// with the same meaning it has for a provider sign-in: nobody knows which
	// tenants an address belongs to before it has been proved, so a single
	// sign-in page asks for none and the picker sorts it out.
	TenantID     uuid.UUID
	EmailAddress string
	Code         string

	Remember  bool
	Client    session.Client
	IPAddress string
	UserAgent string
}

// VerifyEmailCode signs somebody in with the code that was mailed to them.
//
// It answers the body every other sign-in answers — the tenant list, the one
// landed in marked, an identity token, and a session when there is a tenant to
// have one for — which is what makes it fit: the picker, "where you were last"
// and every client already work, because there is nothing new in the response.
//
// It also confirms the address, because the code is the proof: it went to that
// address and came back. That is the same argument accepting an invitation
// makes, and it is why this calls the unexported tail — RequireVerifiedEmail
// would otherwise refuse somebody on a column their own request had just
// filled in.
func (s *Service) VerifyEmailCode(ctx context.Context, in VerifyEmailCodeInput) (SignInResult, error) {
	started := s.now()

	res, err := s.verifyEmailCode(ctx, in)

	// The pad runs on both paths. Padding only failures would make success the
	// fast answer, which is the same oracle in reverse.
	if d := s.minDuration(); d > 0 {
		s.sleep(ctx, d-s.now().Sub(started))
	}
	return res, err
}

func (s *Service) verifyEmailCode(ctx context.Context, in VerifyEmailCodeInput) (SignInResult, error) {
	if !s.cfg.EmailCode.Enabled {
		return SignInResult{}, rigerr.NotFound("this deployment does not sign anybody in with a code")
	}

	email := normalizeEmail(in.EmailAddress)
	at := signInAttempt{
		tenantID:  in.TenantID,
		email:     email,
		ipAddress: in.IPAddress,
		userAgent: in.UserAgent,
		method:    string(KindEmailCode),
	}

	// Before the lookup and before the compare. A locked request that still did
	// the work would let an attacker keep the server busy for free, and a locked
	// request that still recorded a failure would keep extending its own window.
	//
	// The same two limits a provider sign-in and an invitation are counted
	// against, deliberately: a wrong code writes EventLoginFailed and a right
	// one writes EventLoginSucceeded, so the lockout that already existed bounds
	// this door with nothing new to configure, and getting it right clears it.
	decision, err := s.cfg.Limiter.Allow(ctx,
		throttle.Check{Limit: s.cfg.Limits.LoginByEmail, Key: throttle.Email(email)},
		throttle.Check{Limit: s.cfg.Limits.LoginByIP, Key: throttle.IP(in.IPAddress)},
	)
	if err != nil {
		return SignInResult{}, err
	}
	if !decision.Allowed {
		locked := at.entry(authlog.Entry{
			Event: authlog.EventAccountLocked, Outcome: authlog.Failed,
			Detail: map[string]any{"limit": decision.Limit.Name},
		})
		s.write(ctx, locked)
		return SignInResult{}, decision.Err()
	}

	ident, err := s.cfg.Store.FindIdentityByEmail(ctx, email)
	if err != nil {
		return SignInResult{}, err
	}
	if ident == nil {
		// Told apart from a wrong code by nothing the caller can see. The pad in
		// the exported half is what makes that true in the timing as well as in
		// the body.
		s.failSignIn(ctx, at, nil, "no such address")
		return SignInResult{}, ErrInvalidCode
	}

	v, err := s.redeemCode(ctx, at, ident, in.Code)
	if err != nil {
		return SignInResult{}, err
	}

	// The identity's own state, after the code rather than before it: refusing a
	// disabled account to somebody who has not proved the address would say that
	// the address is registered.
	if !ident.IsActive {
		s.failSignIn(ctx, at, nil, "identity disabled")
		return SignInResult{}, rigerr.Forbidden("this account has been disabled")
	}

	now := s.now()
	if err := s.cfg.Store.InTx(ctx, func(ctx context.Context) error {
		consumed, err := s.consume(ctx, v)
		if err != nil {
			return err
		}
		if !consumed {
			// Two windows racing with the same code. One of them wins and the
			// other is told what it would have been told for any dead code.
			return ErrInvalidCode
		}
		if ident.Verified() {
			return nil
		}
		return s.cfg.Store.MarkIdentityVerified(ctx, ident.ID, now)
	}); err != nil {
		return SignInResult{}, err
	}
	if !ident.Verified() {
		// So that the tail sees what the transaction just wrote rather than the
		// row as it was read.
		ident.EmailVerifiedAt = &now
	}

	return s.signInIdentity(ctx, ident, SignInIdentityInput{
		IdentityID: ident.ID,
		TenantID:   in.TenantID,
		Remember:   in.Remember,
		Client:     in.Client,
		IPAddress:  in.IPAddress,
		UserAgent:  in.UserAgent,
		Method:     string(KindEmailCode),
	})
}

// redeemCode finds the person's live code and compares it in constant time.
//
// A wrong code costs an attempt, and the attempt is charged whether or not the
// caller ever comes back — which is the point. Six digits with no ceiling is a
// secret worth a million requests, and a rate limit alone does not give one: it
// counts failures over a rolling window, so a fresh code arrives with the old
// code's failures still against it.
//
// It is not found by its hash, and could not be: with only a million values a
// hash is not a key. The address in the request is what finds the row, which is
// also why the stored hash covers the identity as well as the digits — see
// mintCode.
func (s *Service) redeemCode(
	ctx context.Context, at signInAttempt, ident *Identity, presented string,
) (*Verification, error) {
	code, ok := normalizeCode(presented, s.cfg.EmailCode.Length)
	if !ok {
		// Not charged against the row: this is not a guess, it is a malformed
		// request, and charging it would let somebody burn a code by sending
		// rubbish.
		s.failSignIn(ctx, at, nil, "malformed code")
		return nil, ErrInvalidCode
	}

	v, err := s.cfg.Store.LiveVerification(ctx, ident.ID, KindEmailCode)
	if err != nil {
		return nil, err
	}
	// A row with no hash is one the mail queue has written and not sent yet, so
	// its secret does not exist and nothing can match it.
	if v == nil || !v.Usable(s.now()) || len(v.TokenHash) == 0 {
		s.failSignIn(ctx, at, nil, "no live code")
		return nil, ErrInvalidCode
	}

	if subtle.ConstantTimeCompare(v.TokenHash, codeHash(ident.ID, code)) != 1 {
		attempts, dead, err := s.cfg.Store.ChargeVerificationAttempt(
			ctx, v.ID, s.cfg.EmailCode.MaxAttempts, s.now())
		if err != nil {
			return nil, err
		}
		s.failSignIn(ctx, at, nil, "wrong code")
		_ = attempts
		_ = dead
		return nil, ErrInvalidCode
	}
	return v, nil
}

// mintCode is six digits, the hash that is stored, and the plaintext that is
// not.
//
// The hash covers the identity as well as the code, and that is what makes a
// short secret storable at all. sha256 over a million values is a table
// somebody builds once, and two people signing in the same minute can draw the
// same digits — so the preimage carries the person, which makes the stored
// value unguessable from the code alone and unique between people. It costs
// nothing, because the request that verifies a code carries the address: the row
// is found by whose it is and never by its hash.
//
// It is not a substitute for a long secret. What keeps a code safe is
// [EmailCodeOptions.MaxAttempts]; what this keeps safe is the column.
func (s *Service) mintCode(ident *Identity) (code string, hash []byte, err error) {
	length := s.cfg.EmailCode.Length
	if length == 0 {
		length = DefaultEmailCodeLength
	}

	// crypto/rand.Int rather than a byte and a modulo. `b[0] % 10` is biased
	// towards the low digits, which is a real weakening of a six-digit secret
	// and an easy thing to write by accident.
	max := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(length)), nil)
	n, err := cryptorand.Int(cryptorand.Reader, max)
	if err != nil {
		return "", nil, fmt.Errorf("account: generate code: %w", err)
	}

	// Zero-padded, which is load-bearing: "012345" is a six-digit code and
	// "12345" is not the same secret.
	code = fmt.Sprintf("%0*d", length, n)
	return code, codeHash(ident.ID, code), nil
}

// codeHash is what is stored in place of a code. See [Service.mintCode] for why
// the identity is in the preimage.
func codeHash(identityID uuid.UUID, code string) []byte {
	h := sha256.New()
	id := identityID
	h.Write(id[:])
	h.Write([]byte(code))
	return h.Sum(nil)
}

// normalizeCode is what somebody actually typed, turned into what was minted.
//
// Spaces and hyphens come out, because a code read off a screen gets pasted as
// "012 345" often enough to matter. Leading zeros stay: they are part of the
// secret, and stripping them is the bug that ships in half the implementations
// of this flow.
func normalizeCode(presented string, length int) (string, bool) {
	if length == 0 {
		length = DefaultEmailCodeLength
	}

	var b strings.Builder
	for _, r := range strings.TrimSpace(presented) {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == ' ':
		default:
			return "", false
		}
	}

	code := b.String()
	if len(code) != length {
		return "", false
	}
	return code, true
}

// displayNameFor is what to call somebody who has not said.
//
// The local part, which is what people call each other in a hurry anyway and is
// better than an empty cell in every list.
func displayNameFor(emailAddress string) string {
	name, _, _ := strings.Cut(strings.TrimSpace(emailAddress), "@")
	return name
}
