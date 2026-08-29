// Package readopt controls the filters a generated read applies by default.
//
// Every query-based read excludes soft-deleted rows, excludes snapshots, and
// scopes to the caller's tenant. Those are the right defaults, and they are
// applied without being asked for — which is precisely why there has to be an
// explicit way to say otherwise. An escape hatch nobody can find is a reason to
// bypass the repository entirely.
package readopt

import "github.com/simonjanss/rig/runtime/rigerr"

// Config is the resolved set of options.
type Config struct {
	// IncludeDeleted drops the "not deleted" filter.
	IncludeDeleted bool
	// OnlyDeleted returns just the deleted rows, within the restore window.
	OnlyDeleted bool
	// IncludeSnapshots drops the "live version only" filter.
	IncludeSnapshots bool
	// SkipTenantScope drops the tenant filter. It exists for administrative
	// tooling and cross-tenant reporting, and for nothing a request handler
	// should ever do.
	SkipTenantScope bool
	// SkipOwnerScope drops the "rows the caller created" filter, on a table that
	// has one.
	//
	// The only option here that a request handler does set, and the direction is
	// the reason it is safe to: the narrow read is what happens when nobody says
	// anything, so a handler that forgets this returns too little rather than too
	// much. Widening is the thing somebody had to ask for and hold a permission
	// for.
	SkipOwnerScope bool
}

// Option adjusts a read.
type Option func(*Config)

// WithDeleted includes soft-deleted rows alongside live ones.
func WithDeleted() Option { return func(c *Config) { c.IncludeDeleted = true } }

// WithOnlyDeleted returns only soft-deleted rows still inside the restore
// window — the "trash" view.
func WithOnlyDeleted() Option { return func(c *Config) { c.OnlyDeleted = true } }

// WithSnapshots includes historical versions alongside live rows.
func WithSnapshots() Option { return func(c *Config) { c.IncludeSnapshots = true } }

// WithoutTenantScope reads across every tenant.
func WithoutTenantScope() Option { return func(c *Config) { c.SkipTenantScope = true } }

// WithoutOwnerScope reads every row in the tenant, not only the caller's own.
func WithoutOwnerScope() Option { return func(c *Config) { c.SkipOwnerScope = true } }

// Apply resolves a set of options.
//
// Contradictory options are an error rather than a precedence rule. Asking for
// deleted rows and only-deleted rows at once means the caller has confused
// themselves, and quietly picking one would hide that.
func Apply(opts []Option) (Config, error) {
	var c Config
	for _, opt := range opts {
		if opt != nil {
			opt(&c)
		}
	}
	if c.IncludeDeleted && c.OnlyDeleted {
		return Config{}, rigerr.BadRequest("WithDeleted and WithOnlyDeleted contradict each other")
	}
	return c, nil
}
