package notify

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/runtime/dbx"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// dueBatch bounds how many notifications one pass resolves.
//
// Bounded for the reason every sweep here is bounded: a tenant with a bad week
// should not become a single pass holding a connection while it works through a
// backlog, and the next tick takes the rest.
const dueBatch = 100

// Engine is the dispatcher: it takes notifications whose time has come, asks who
// should hear about them, and writes the inbox lines.
//
// It is the only thing in this module that runs on its own, and it runs in two
// places at once by design — an in-process goroutine nudged when a transaction
// commits, and a `serve.Task` an operator's cron invokes. The task is the
// guarantee; the goroutine is latency and nothing else. Say that in those words,
// because the shape invites the opposite reading: nothing is lost when the nudge
// is skipped, since the row is still Pending and the task is still coming.
type Engine struct {
	cfg   Config
	store store

	// links is every notifiable table's join, so a notification can be traced
	// back to the row it is about. Written from the compiled document.
	links []Subject

	nudge chan struct{}
	stop  chan struct{}
	done  chan struct{}

	mu       sync.Mutex
	claiming bool
	interval time.Duration
}

// EngineConfig is what an engine needs beyond [Config].
type EngineConfig struct {
	Config

	// Links is one entry per notifiable table: its join table and the column in
	// it. A generator writes them from the document, so no name here came from
	// a request.
	Links []Subject

	// Interval is how often a pass runs without a nudge. Zero means a minute,
	// which is the number a scheduled notification's lateness is bounded by.
	Interval time.Duration
}

// DefaultInterval is how often the engine looks for work on its own.
//
// A minute, because that is the bound on how late a *scheduled* notification
// can be — an immediate one does not wait for it, since the commit that caused
// it nudges the engine directly.
const DefaultInterval = time.Minute

// NewEngine builds a dispatcher. It does nothing until [Engine.Start].
func NewEngine(cfg EngineConfig) *Engine {
	interval := cfg.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Engine{
		cfg:      cfg.Config,
		store:    store{db: cfg.DB},
		links:    cfg.Links,
		nudge:    make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		claiming: true,
		interval: interval,
	}
}

// Start runs the dispatcher until [Engine.Close].
//
// This is the one place rig runs periodic work in-process, against a position
// stated in five other places — a task rather than a goroutine, so it is a cron
// job rather than something racing itself in every replica. The departure is
// defensible because a pass holds no state: two replicas resolving the same
// notification write the same inbox lines, and the unique index absorbs it.
// What it buys is that a reply notification is in somebody's inbox in
// milliseconds rather than by the next tick.
func (e *Engine) Start() {
	go e.run()
}

// Nudge asks for a pass now.
//
// It is an optimization and nothing may be built on it. A dropped nudge — and
// they are dropped, because the channel holds one — costs at most one interval,
// since the row is Pending and the next pass takes it.
func (e *Engine) Nudge() {
	select {
	case e.nudge <- struct{}{}:
	default:
	}
}

// StopClaiming stops taking new work while the server is still answering.
//
// Registered with app.Drain, which runs after readiness goes false and before
// the server stops: the requests still in flight are the last ones whose commits
// will nudge this, and the time left is better spent finishing than starting.
func (e *Engine) StopClaiming(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.claiming = false
	return nil
}

// Close finishes the pass in flight and stops.
//
// Registered with app.CloseWithin, which runs before the pool closes — not
// incidental, because what is in flight is a write.
func (e *Engine) Close(ctx context.Context) error {
	select {
	case <-e.stop:
	default:
		close(e.stop)
	}
	select {
	case <-e.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Engine) run() {
	defer close(e.done)

	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()

	for {
		select {
		case <-e.stop:
			return
		case <-ticker.C:
		case <-e.nudge:
		}

		e.mu.Lock()
		claiming := e.claiming
		e.mu.Unlock()
		if !claiming {
			continue
		}

		// A pass that fails is a pass: the rows are still Pending and the next
		// one takes them. Nothing here may take the process down.
		_, _ = e.Resolve(context.Background())
	}
}

// Report is what one pass did, for the log line the task writes.
//
// Every count, including the zeros, for the reason the file sweeper's report
// gives: a pass that resolved nothing is the ordinary case and still worth
// seeing, because the absence of a line cannot be told from the job not running.
type Report struct {
	// Resolved is how many notifications had their audience computed.
	Resolved int
	// Recipients is how many inbox lines were written, and Collapsed how many
	// joined an existing unread line instead.
	Recipients int
	Collapsed  int

	// Empty is how many resolutions produced no recipients at all.
	//
	// It is here because it is the one failure mode of this design that is
	// otherwise silent: a notification with no audience looks exactly like a
	// notification nobody was owed. The commonest cause is an owner-scoped read
	// inside an Audience method, which returns nothing under System claims until
	// it is told not to narrow.
	Empty int

	// Unregistered is how many notifications named a table nothing answers for.
	// A build whose registry lost a service, most often.
	Unregistered int
}

// String is the one line a pass is worth in a log: every count, zeros included.
func (r Report) String() string {
	return fmt.Sprintf(
		"notify: resolved %d, %d recipients, %d collapsed, %d with nobody to tell, %d unregistered",
		r.Resolved, r.Recipients, r.Collapsed, r.Empty, r.Unregistered)
}

// Resolve is one pass: take what is due, ask who, write the lines.
//
// Each notification is its own transaction. One tenant's Audience method failing
// must not hold up everybody else's, and a pass that resolved five and failed on
// the sixth should keep the five.
func (e *Engine) Resolve(ctx context.Context) (Report, error) {
	var report Report

	due, err := e.store.due(ctx, e.cfg.now(), dueBatch)
	if err != nil {
		return report, err
	}

	for _, n := range due {
		r, err := e.resolveOne(ctx, n)
		report.Resolved += r.Resolved
		report.Recipients += r.Recipients
		report.Collapsed += r.Collapsed
		report.Empty += r.Empty
		report.Unregistered += r.Unregistered
		if err != nil {
			return report, err
		}
	}
	return report, nil
}

func (e *Engine) resolveOne(ctx context.Context, n *Notification) (Report, error) {
	var report Report

	// System claims for the row's own tenant, so the reads inside Audience are
	// tenant-scoped without anybody threading a tenant through. One thing about
	// them is surprising and is worth knowing before writing one of these
	// methods: AccountID is uuid.Nil, so an owner-scoped read returns nothing
	// until it is given readopt.WithoutOwnerScope(). It fails as an empty
	// audience rather than as an error, and an empty audience is the hardest bug
	// in this system to notice — which is what Report.Empty exists for.
	inner := tenancy.NewContext(ctx, tenancy.System(n.TenantID))

	err := dbx.InTx(inner, e.cfg.DB, func(ctx context.Context, _ dbx.Conn) error {
		subjects, err := e.store.subjectsOf(ctx, n, e.links)
		if err != nil {
			return err
		}

		accounts, unregistered, err := e.audience(ctx, n, subjects)
		if err != nil {
			return err
		}
		report.Unregistered += unregistered

		written, collapsed, err := e.store.fanOut(ctx, n, accounts)
		if err != nil {
			return err
		}
		report.Recipients += written
		report.Collapsed += collapsed

		if len(accounts) == 0 {
			report.Empty++
		}
		report.Resolved++
		return e.store.resolve(ctx, n.ID, e.cfg.now())
	})
	return report, err
}

// audience asks every subject of a notification who should hear about it, and
// merges the answers.
//
// Merged rather than concatenated because two subjects can name the same person,
// and while the inbox index would absorb the duplicate it would cost a wasted
// statement per account per subject to find that out.
//
// An announcement that carried its own list skips this entirely. That is the one
// exception to late resolution and it is named rather than hidden: some
// audiences genuinely cannot be re-derived, and the alternative is an Audience
// method reading a version row to reconstruct a list somebody already had.
func (e *Engine) audience(
	ctx context.Context, n *Notification, subjects []Subject,
) ([]uuid.UUID, int, error) {
	if len(n.AccountIDs) > 0 {
		return n.AccountIDs, 0, nil
	}

	var (
		unregistered int
		seen         = make(map[uuid.UUID]bool)
		out          []uuid.UUID
	)
	for _, subject := range subjects {
		answers := e.cfg.Registry.For(subject.Table)
		if answers == nil {
			// Not an error, and not fatal to the pass. A build whose registry
			// lost a service should show up as a count somebody can see rather
			// than as a dispatcher that stops dispatching everything behind it.
			unregistered++
			continue
		}

		accounts, err := answers.Audience(ctx, n, subject.ID)
		if err != nil {
			return nil, unregistered, fmt.Errorf("notify: audience for %s: %w", subject.Table, err)
		}
		for _, id := range accounts {
			if id == uuid.Nil || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, unregistered, nil
}
