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

	// The delivery half, and what it runs on. Resolved once at construction so
	// that a pass reads no configuration.
	senders       map[Channel]Sender
	claimedBy     uuid.UUID
	claimTTL      time.Duration
	sendTimeout   time.Duration
	maxAttempts   int
	backoffBase   time.Duration
	defaultDigest Digest

	nudge chan struct{}
	stop  chan struct{}
	done  chan struct{}

	mu       sync.Mutex
	claiming bool
	interval time.Duration
	// held are the leases this process currently owns, so a clean shutdown can
	// give them back rather than leaving them to expire.
	held map[uuid.UUID]bool
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

	// Senders are the channels this project can actually send on. A channel
	// with nothing here has no delivery rows written for it at all, which is
	// the right answer: the alternative is a table of copies nobody will take.
	//
	// Empty is a working configuration: the inbox is not a channel, so an
	// application with no transport still has one.
	Senders map[Channel]Sender

	// ClaimTTL is how long this process's claim is honoured. Zero means
	// [DefaultClaimTTL]; under [MinClaimTTL] panics.
	ClaimTTL time.Duration

	// SendTimeout bounds one call into a [Sender]. Zero means
	// [DefaultSendTimeout]; ClaimTTL or longer panics.
	//
	// It is the deadline on the context a channel is handed, and it is the only
	// thing standing between a provider that never answers and a dispatcher that
	// never runs again.
	SendTimeout time.Duration

	// MaxAttempts and BackoffBase are the retry arithmetic. Zero means the
	// defaults.
	MaxAttempts int
	BackoffBase time.Duration

	// DefaultDigest is what an account with no setting for a channel gets.
	// Empty means Immediate.
	DefaultDigest Digest
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
	ttl := cfg.ClaimTTL
	if ttl == 0 {
		ttl = DefaultClaimTTL
	}
	if ttl < MinClaimTTL {
		// At boot rather than at dispatch time, the way the shutdown budget is
		// checked: under a minute, every message a slow channel is still
		// sending is claimed a second time under ordinary load, and
		// at-least-once stops being a crash-recovery property.
		panic(fmt.Sprintf(
			"notify.NewEngine: claim_ttl is %s, and under %s every message a slow channel is "+
				"still sending is claimed twice; set it longer than that channel's own timeout",
			ttl, MinClaimTTL))
	}

	timeout := cfg.SendTimeout
	if timeout <= 0 {
		timeout = DefaultSendTimeout
	}
	if timeout >= ttl {
		// The same check as the one above and the other way round: a send allowed
		// to run as long as the lease that protects it means every message a
		// slow channel is still sending has already been claimed by somebody
		// else. Equal is refused with longer, because a send that ends exactly
		// when its lease does ends after it in practice: the lease was stamped
		// before the send started. Refused here rather than found as duplicate
		// mail, and refused with both numbers because the fix is to change one of
		// them and the message should not make the reader guess which.
		panic(fmt.Sprintf(
			"notify.NewEngine: send_timeout is %s and claim_ttl is %s, so a send may still be "+
				"running when its own lease expires and another dispatcher takes the row; "+
				"set send_timeout below claim_ttl",
			timeout, ttl))
	}

	attempts := cfg.MaxAttempts
	if attempts <= 0 {
		attempts = DefaultMaxAttempts
	}
	backoff := cfg.BackoffBase
	if backoff <= 0 {
		backoff = DefaultBackoffBase
	}
	digest := cfg.DefaultDigest
	if digest == "" {
		digest = DigestImmediate
	}

	return &Engine{
		cfg:   cfg.Config,
		store: store{db: cfg.DB},
		links: cfg.Links,

		senders: cfg.Senders,
		// One identifier per process, with the hostname beside it in the log
		// line, so a lease that is stuck traces to a pod rather than a mystery.
		claimedBy:     uuid.New(),
		claimTTL:      ttl,
		sendTimeout:   timeout,
		maxAttempts:   attempts,
		backoffBase:   backoff,
		defaultDigest: digest,

		nudge:    make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		claiming: true,
		interval: interval,
		held:     make(map[uuid.UUID]bool),
	}
}

// ClaimedBy is this process's lease identifier, for the log line that makes a
// stuck lease traceable to a pod.
func (e *Engine) ClaimedBy() uuid.UUID { return e.claimedBy }

// The lease bookkeeping a clean shutdown reads.
func (e *Engine) hold(ds []Delivery) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, d := range ds {
		e.held[d.ID] = true
	}
}

func (e *Engine) forget(id uuid.UUID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.held, id)
}

func (e *Engine) forgetAll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	clear(e.held)
}

func (e *Engine) heldIDs() []uuid.UUID {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]uuid.UUID, 0, len(e.held))
	for id := range e.held {
		out = append(out, id)
	}
	return out
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
	case <-ctx.Done():
		return ctx.Err()
	}

	// And then the work goes back rather than being left to expire. The TTL is
	// for crashes; a process that knows it is going has no excuse for being
	// slow about saying so, and leaving them turns every ordinary rollout into
	// a delivery delay — repeatedly, for a rollout that replaces every pod.
	_, err := e.ReleaseClaims(ctx)
	return err
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
		_, _ = e.Dispatch(context.Background())
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

	// Deliveries is how many copies were written for channels somebody's
	// settings allow, and Held how many were moved into a window rather than
	// being due at once.
	Deliveries int
	Held       int
}

// String is the one line a pass is worth in a log: every count, zeros included.
func (r Report) String() string {
	return fmt.Sprintf(
		"notify: resolved %d, %d recipients, %d collapsed, %d with nobody to tell, "+
			"%d unregistered, %d deliveries, %d held",
		r.Resolved, r.Recipients, r.Collapsed, r.Empty, r.Unregistered,
		r.Deliveries, r.Held)
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

		// The copies each of those lines is owed, in the same transaction: a
		// line somebody sees in the application and never hears about anywhere
		// else is exactly the kind of "sometimes" a notification system is
		// judged on.
		for _, account := range accounts {
			id, ok, err := e.store.recipientID(ctx, n.ID, account)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if err := e.writeDeliveries(ctx, n, id, account, &report); err != nil {
				return err
			}
		}

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
