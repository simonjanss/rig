package authmodel

import (
	"strings"
)

// The coarse level an account holds in one tenant.
type AccountRoleLevel string

// The values of AccountRoleLevel.
const (
	// May do anything, including the things that end the tenant.
	AccountRoleLevelOwner AccountRoleLevel = "Owner"
	// Administers the tenant without the decisions that are the owner's to make.
	AccountRoleLevelAdmin AccountRoleLevel = "Admin"
	// Gets on with the work.
	AccountRoleLevelBasic AccountRoleLevel = "Basic"
)

// AllAccountRoleLevel is every value, in declaration order.
var AllAccountRoleLevel = []AccountRoleLevel{AccountRoleLevelOwner, AccountRoleLevelAdmin, AccountRoleLevelBasic}

// Valid reports whether the value is one the database will accept.
func (v AccountRoleLevel) Valid() bool {
	switch v {
	case AccountRoleLevelOwner, AccountRoleLevelAdmin, AccountRoleLevelBasic:
		return true
	default:
		return false
	}
}

// ParseAccountRoleLevel reads a value, accepting any casing and surrounding
// space.
//
// Normalization uses it, so "IN_PROGRESS" from one client and "in_progress"
// from another mean the same thing rather than one of them being a validation
// failure nobody can explain. The spelling still has to be the label’s: a
// value is a name the database knows, not a phrase.
func ParseAccountRoleLevel(s string) (AccountRoleLevel, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "owner":
		return AccountRoleLevelOwner, true
	case "admin":
		return AccountRoleLevelAdmin, true
	case "basic":
		return AccountRoleLevelBasic, true
	default:
		return "", false
	}
}

// String implements fmt.Stringer.
func (v AccountRoleLevel) String() string { return string(v) }
