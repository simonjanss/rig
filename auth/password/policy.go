package password

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/simonjanss/rig/runtime/rigerr"
)

// Policy is what a new password must satisfy.
//
// There are no composition rules, and that is the recommendation rather than an
// omission: demanding an uppercase letter, a digit, and a symbol pushes people
// toward Password1! and a sticky note, which is worse on both counts than a
// longer passphrase. Length and a breach check are what actually help.
type Policy struct {
	// MinLength is in characters, not bytes, so a passphrase in a language
	// that needs more than one byte per character is not penalized for it.
	MinLength int
	// MaxLength bounds the work. It is not about password strength: argon2id
	// over a ten-megabyte "password" is a denial of service anyone can send,
	// and the limit is what stops it.
	MaxLength int
	// Breached, when set, rejects passwords known to have leaked. See [HIBP].
	Breached BreachChecker
}

// DefaultPolicy is twelve characters, no composition rules, no breach check.
//
// The breach check is off by default because it talks to a third party, and
// that is a decision an application makes rather than one a library makes for
// it.
func DefaultPolicy() Policy {
	return Policy{MinLength: 12, MaxLength: 1024}
}

// BreachChecker reports how many times a password has appeared in a known
// breach.
type BreachChecker interface {
	// Count returns the number of times the password appears in breach
	// corpora, or zero if it does not appear.
	//
	// An implementation that cannot reach its source returns an error rather
	// than zero. The caller decides what to do about it; a checker that
	// silently answers "not breached" when it is offline is worse than none,
	// because it looks like it is working.
	Count(ctx context.Context, plain string) (int, error)
}

// Check validates a password against the policy.
//
// A breach checker that fails is not a failed check. The service being down is
// not the person's fault, and refusing to let anybody set a password because a
// third party is having an outage trades a small risk for a total one.
func (p Policy) Check(ctx context.Context, plain string) error {
	min := p.MinLength
	if min == 0 {
		min = DefaultPolicy().MinLength
	}
	max := p.MaxLength
	if max == 0 {
		max = DefaultPolicy().MaxLength
	}

	// Leading and trailing spaces are almost always a paste artifact, but the
	// password is stored exactly as given: trimming it silently would mean the
	// value that worked at sign-up is not the value the person typed.
	switch n := utf8.RuneCountInString(plain); {
	case n < min:
		return rigerr.Invalid("a password must be at least %d characters", min)
	case n > max:
		return rigerr.Invalid("a password must be at most %d characters", max)
	}

	if strings.TrimSpace(plain) == "" {
		return rigerr.Invalid("a password cannot be only whitespace")
	}

	if p.Breached == nil {
		return nil
	}
	count, err := p.Breached.Count(ctx, plain)
	if err != nil {
		return nil
	}
	if count > 0 {
		// The count is deliberately not in the message. "seen 3 million times"
		// invites an argument; it does not help anyone choose a better one.
		return rigerr.Invalid(
			"this password has appeared in a public data breach; please choose another")
	}
	return nil
}
