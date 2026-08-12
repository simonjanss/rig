package throttle

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/simonjanss/rig/runtime/dbx"
)

// Postgres counts over a log table.
//
// One indexed query answers the whole question — how many, when was the first,
// and has anything cleared the window since — which is why the limiter needs no
// state of its own and why two replicas cannot disagree.
type Postgres struct {
	db  dbx.Conn
	cfg PostgresConfig
}

// PostgresConfig says where the events are.
//
// The defaults match the auth log `rig setup-project` writes. It is
// configurable anyway because the interesting part is the mapping from a key
// kind to a column, and an application that logs something rig did not think of
// should be able to rate-limit on it.
type PostgresConfig struct {
	// Table is the log table, for example "auth_log".
	Table string
	// EventColumn holds the event name.
	EventColumn string
	// AtColumn holds when the event happened.
	AtColumn string

	// Match maps a [Key.Kind] onto the SQL that selects rows about it, with $1
	// bound to the key's value.
	//
	// It is an expression rather than a column name so that a comparison can
	// say what it means: an email address is matched case-insensitively and an
	// address needs a cast, and neither is expressible as a bare column.
	Match map[string]string
}

// DefaultPostgresConfig is the shape `rig setup-project` scaffolds.
func DefaultPostgresConfig() PostgresConfig {
	return PostgresConfig{
		Table:       "auth_log",
		EventColumn: "event",
		AtColumn:    "created_at",
		Match: map[string]string{
			// The index is on lower(email_address), so the call belongs on the
			// column: wrapping the parameter instead would make the query scan.
			KeyEmailAddress: "lower(email_address) = $1",
			KeyIPAddress:    "ip_address = $1::inet",
			KeyAccount:      "account_id = $1::uuid",
			KeyAPIKey:       "api_key_ref = $1",
			KeyTokenFamily:  "token_root_id = $1::uuid",
		},
	}
}

// NewPostgres builds a counter over a connection.
func NewPostgres(db dbx.Conn, cfg PostgresConfig) *Postgres {
	if cfg.Table == "" {
		cfg = DefaultPostgresConfig()
	}
	return &Postgres{db: db, cfg: cfg}
}

// Count implements [Counter].
func (p *Postgres) Count(ctx context.Context, limit Limit, key Key, since time.Time) (int, time.Time, error) {
	match, ok := p.cfg.Match[key.Kind]
	if !ok {
		// Silently counting nothing would make the limit look configured and
		// behave as though it were not.
		return 0, time.Time{}, fmt.Errorf("throttle: no column is mapped for key kind %q", key.Kind)
	}

	// Only the parameters the statement mentions: a limit that nothing clears
	// has no fourth placeholder, and Postgres counts.
	args := []any{key.Value, limit.Event, since}
	if limit.ClearedBy != "" {
		args = append(args, limit.ClearedBy)
	}

	var (
		n        int
		earliest *time.Time
	)
	if err := p.db.QueryRow(ctx, p.sql(match, limit), args...).Scan(&n, &earliest); err != nil {
		return 0, time.Time{}, fmt.Errorf("throttle: count %s: %w", limit.Name, err)
	}

	if earliest == nil {
		return n, time.Time{}, nil
	}
	return n, *earliest, nil
}

// sql builds the counting query.
//
// The floor is the later of the window's start and the most recent clearing
// event, computed inside the statement rather than by a second round trip: two
// queries could straddle a clearing event and count failures it had already
// wiped.
func (p *Postgres) sql(match string, limit Limit) string {
	var (
		table = p.cfg.Table
		event = p.cfg.EventColumn
		at    = p.cfg.AtColumn
	)

	floor := "$3::timestamptz"
	if limit.ClearedBy != "" {
		floor = fmt.Sprintf(
			"greatest($3::timestamptz, coalesce((SELECT max(c.%[1]s) FROM %[2]s c WHERE %[3]s AND c.%[4]s = $4 AND c.%[1]s > $3::timestamptz), $3::timestamptz))",
			at, table, qualify(match, "c"), event)
	}

	return fmt.Sprintf(
		"SELECT count(*), min(t.%[1]s) FROM %[2]s t WHERE %[3]s AND t.%[4]s = $2 AND t.%[1]s > %[5]s",
		at, table, qualify(match, "t"), event, floor)
}

// qualify points a match expression at one alias.
//
// The same expression is used twice in the statement — once for the events and
// once for whatever cleared them — and an unqualified column would be ambiguous
// in the subquery.
func qualify(match, alias string) string {
	// The expressions are rig's own, from a small closed map, so this is a
	// rename rather than a parser: it prefixes the identifiers the default
	// configuration uses and leaves everything else alone.
	for _, column := range []string{
		"email_address", "ip_address", "account_id", "api_key_ref", "api_key_id", "token_root_id",
	} {
		match = strings.ReplaceAll(match, column, alias+"."+column)
	}
	return match
}
