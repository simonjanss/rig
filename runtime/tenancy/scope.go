package tenancy

import "github.com/simonjanss/rig/runtime/rigerr"

// Scope is how wide a read reaches inside the caller's tenant.
//
// It exists because the alternative is worse. A resource where people should
// normally see their own rows has two obvious shapes: one endpoint that quietly
// returns different sets to different callers, or two endpoints — /notes and
// /notes/all — where an integration has to be told which one to call and a
// client written against the narrow one silently keeps working when its
// credential is widened. Both hide the width of the answer. A parameter makes it
// part of the request, so the caller says what it wants, the response says what
// it got, and asking for more than you hold is a refusal rather than a shrug.
type Scope string

const (
	// ScopeOwn is rows the caller created. The default, so a request that says
	// nothing gets the narrow answer.
	ScopeOwn Scope = "own"
	// ScopeAll is every row in the tenant. It has to be asked for, and it has to
	// be held.
	ScopeAll Scope = "all"
)

// ScopeParam is the query-string key. Reserved: a column projecting to this name
// is a compile error, because two things answering to one parameter is a bug
// nobody would find from the outside.
const ScopeParam = "scope"

// ParseScope reads the parameter.
//
// An unrecognised value is a 400 rather than a silent fallback to the narrow
// default: a typo that returns fewer rows than the caller expected is a bug that
// looks like missing data, and hunting one of those from the outside is
// miserable.
func ParseScope(raw string) (Scope, error) {
	switch Scope(raw) {
	case "":
		return ScopeOwn, nil
	case ScopeOwn:
		return ScopeOwn, nil
	case ScopeAll:
		return ScopeAll, nil
	default:
		return "", rigerr.BadRequest("%s must be %q or %q, not %q",
			ScopeParam, ScopeOwn, ScopeAll, raw)
	}
}

// RequireScope refuses a caller who asked to see more than they hold.
//
// The wide permission is checked in addition to the endpoint's own, not instead
// of it: a generated handler calls [Require] with the read permission first and
// this second, so widening presupposes being allowed to read at all. A credential
// minted with only the wide key can therefore read nothing — which is the right
// shape (one grant, one thing) and worth knowing before minting one.
//
// Gated on a permission rather than on the caller being a machine. The two are
// not the same thing and conflating them breaks both ways: a person's own API key
// acts as that person and must not see the whole tenant just because it is a key,
// and a human administrator reviewing everybody's rows must be able to. What
// makes a service account wide is that its key was minted with the permission —
// which is a decision somebody made and can revoke, rather than a property of how
// the request arrived.
//
// The refusal is loud. Narrowing the answer instead would be the tempting move
// and it is the wrong one: a caller that asked for everything and got a page of
// its own rows has no way to tell that from there being nothing else, so it draws
// a wrong conclusion from a correct-looking response. Insufficient authority must
// never produce a smaller result set.
func RequireScope(c Claims, s Scope, wide string) error {
	if s != ScopeAll {
		return nil
	}
	if err := Require(c, wide); err != nil {
		return err
	}
	return nil
}
