package presence

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// Config is everything a [Service] needs that is not a request.
//
// It is resolved from the project's `presence:` block and written into the
// generated wiring, so a TTL is a line in a file the documentation can quote
// rather than a literal in a main function nobody diffs.
type Config struct {
	// DB is the pool. Not necessarily the application's own transaction, and
	// nothing here ever joins one — which is the one way presence is simpler
	// than the inbox. An announcement has to be written in the transaction that
	// caused it or it can describe something that never happened; a heartbeat is
	// not part of a change, it is a statement about the last twenty seconds, and
	// a heartbeat that committed beside a write that rolled back is still telling
	// the truth.
	DB DB

	// TTL is how long a session stays present after its last heartbeat. Zero
	// means [DefaultTTL]; anything under [MinTTL] panics in [NewService].
	TTL time.Duration

	// Heartbeat is how often a browser should confirm it is still there.
	//
	// This package never acts on it. It is carried so that [Service.Beat] can
	// answer with it, which is what puts the interval on the server side of the
	// wire: changing it is a deploy of this binary rather than a release of the
	// front end, and there is no copy of the number in the browser to disagree.
	Heartbeat time.Duration

	// Targets are the tables a presence may point at, written from the compiled
	// document.
	//
	// It is a typo boundary rather than a security one — target_table reaches no
	// SQL statement, so nothing is injectable through it — but without it the
	// column is untrusted text and every reader has to treat it that way. Empty
	// accepts any table name, which is what a caller with no document to consult
	// has to do.
	Targets []string

	// Now is the clock, so a test can move it. Nil means [time.Now].
	Now func() time.Time
}

// Service is the heartbeat, the leave and the plain read.
//
// The sweep is [Sweeper], separately, because it is the one operation here that
// is not about one caller's own row.
type Service struct {
	cfg Config
}

// NewService builds the service.
//
// It panics on a TTL under [MinTTL], for the reason notify's engine panics on a
// short claim lease: what a too-short window produces is not an error anybody
// sees, it is presence that looks unreliable, and the cheapest place to notice
// is the line that configured it.
func NewService(cfg Config) *Service {
	if cfg.DB == nil {
		panic("presence: Config.DB is required")
	}
	if cfg.TTL == 0 {
		cfg.TTL = DefaultTTL
	}
	if cfg.TTL < MinTTL {
		panic(fmt.Sprintf("presence: a TTL of %s is under the %s minimum; "+
			"below it a session flickers on an ordinary mobile connection", cfg.TTL, MinTTL))
	}
	if cfg.Heartbeat == 0 {
		cfg.Heartbeat = DefaultHeartbeat
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{cfg: cfg}
}

// TTL is how long a session stays present after its last heartbeat, as resolved.
//
// Exported because it is half of what a subscriber needs and the routes have to
// answer with it.
func (s *Service) TTL() time.Duration { return s.cfg.TTL }

// Heartbeat is how often a browser should say it is still there, as resolved.
func (s *Service) Heartbeat() time.Duration { return s.cfg.Heartbeat }

// Beat is one heartbeat: where a session is, and that it is still there.
//
// The two are one call because they are one statement about one row, and
// splitting them would mean two writes where the interesting case is one — a
// client that moved to another field has also just proved it is alive.
type Beat struct {
	// SessionKey identifies the tab. Required: without it two tabs of one
	// account overwrite each other on every beat, and the person appears to
	// teleport between the two things they are doing.
	SessionKey string
	// Scope is which part of the application this is, named by the application.
	Scope string
	// Target is what they are on, narrowing from a table to a row to a field.
	Target Target
	// Activity is whether they are looking or typing. The zero value reads as
	// [Viewing].
	Activity Activity
}

// Beat records that a session is here, and what it is looking at.
//
// One statement: an upsert on (tenant_id, account_id, session_key), so a tab
// left open all day is one row rather than four thousand. The account and the
// tenant come from the claims and there is no field on [Beat] to override them —
// a caller cannot say that somebody else is editing something, and that is
// structural rather than a check somebody could forget to make.
//
// It answers with the row as written, whose SeenAt is this server's clock at the
// moment of the write. That reading is the only one a browser gets, and it is
// what makes a client-side freshness test possible at all.
func (s *Service) Beat(ctx context.Context, claims tenancy.Claims, b Beat) (*Presence, error) {
	if !claims.Valid() {
		return nil, rigerr.Unauthorized("this request is not authenticated")
	}
	// An API key and a system credential both have no account behind them, and
	// there is nobody for their presence to be about. Refused rather than
	// written against the nil account, which would put every machine caller in
	// the tenant into one row they all fight over.
	if claims.AccountID == uuid.Nil {
		return nil, rigerr.Forbidden("presence is about a person, and this credential is not one")
	}
	if b.SessionKey == "" {
		return nil, rigerr.Invalid("sessionKey is required: it is what tells one of your tabs from another")
	}
	if b.Scope == "" {
		return nil, rigerr.Invalid("scope is required: it is what a subscriber narrows the stream by")
	}
	if err := s.checkTarget(b.Target); err != nil {
		return nil, err
	}

	activity := b.Activity
	if activity == "" {
		activity = Viewing
	}
	return s.upsert(ctx, claims, b, activity, s.cfg.Now().UTC())
}

// checkTarget refuses a target no reader could use.
//
// The two shapes it rejects are the ones the table's own check constraints
// reject, restated here so the answer is a 422 that names the field rather than a
// 500 carrying a constraint name. The constraints stay, because they are what
// makes the rule true for a writer that is not this one.
func (s *Service) checkTarget(t Target) error {
	if t.ID != uuid.Nil && t.Table == "" {
		return rigerr.Invalid("targetId names a row without saying which table it is in")
	}
	if t.Field != "" && t.ID == uuid.Nil {
		return rigerr.Invalid("targetField names a field without a row to name it on")
	}
	if t.Table != "" && len(s.cfg.Targets) > 0 && !slices.Contains(s.cfg.Targets, t.Table) {
		return rigerr.Invalid("%q is not a table in this API", t.Table)
	}
	return nil
}

// Leave removes a session now rather than waiting out the TTL.
//
// It is what makes a generous TTL affordable: an ordinary close, a navigation or
// a tab going to the background sends this, so the TTL only has to cover a crash
// and a dead network.
//
// Idempotent, and deliberately not an error for a session that is already gone.
// The caller is usually a page being torn down, which has nowhere to put an
// answer — and a retry of a request whose response was lost should not look like
// a failure.
func (s *Service) Leave(ctx context.Context, claims tenancy.Claims, sessionKey string) error {
	if !claims.Valid() {
		return rigerr.Unauthorized("this request is not authenticated")
	}
	if sessionKey == "" {
		return rigerr.Invalid("sessionKey is required")
	}
	return s.remove(ctx, claims, sessionKey)
}

// Query narrows a read of who is here.
type Query struct {
	// Scope is which part of the application to look in. Empty is the whole
	// tenant, which is a wider answer than any screen wants and is what a
	// diagnostic page asks for.
	Scope string
	// Target narrows to a table, a row, or a field of one. Its zero value adds
	// nothing.
	Target Target
}

// Here is who is present, for a client that is not streaming.
//
// The freshness filter is applied here, unlike on the stream, and that
// difference is the whole of this package's design: a plain read is a moment and
// can afford a predicate that moves, while a subscription is re-evaluated only
// when a row changes and cannot. See the package documentation.
func (s *Service) Here(ctx context.Context, claims tenancy.Claims, q Query) ([]*Presence, error) {
	if !claims.Valid() {
		return nil, rigerr.Unauthorized("this request is not authenticated")
	}
	return s.list(ctx, claims, q, s.cfg.Now().UTC().Add(-s.cfg.TTL))
}
