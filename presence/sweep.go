package presence

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// sweepBatch bounds one pass, so a table that had a bad week does not become a
// single statement holding a connection while it rewrites every page.
const sweepBatch = 500

// SweepReport is what one pass removed.
//
// The count including the zero, for the reason the file sweeper's report gives:
// a pass that removed nothing is the ordinary case and still worth a line,
// because the absence of one cannot be told from the job not running.
type SweepReport struct {
	// Expired is how many rows were past their TTL and grace.
	Expired int
}

// String is the one line a sweep is worth in a log.
func (r SweepReport) String() string {
	return fmt.Sprintf("presence: swept %d expired", r.Expired)
}

// Sweeper deletes presence rows past their TTL and grace.
//
// **It is not the guarantee behind presence, and that is the whole contrast with
// the notification dispatcher.** There, the task is the guarantee: a
// notification that is never dispatched is lost. Here, who is present is decided
// by whoever is reading, against the clock, and correctly within a second — so
// this only keeps the table, and every new subscriber's first fetch, from
// carrying yesterday. Skipping it costs space and a slower first fetch.
//
// It does converge every subscriber's copy as a side effect, because a DELETE is
// a change and a change is what a shape's filter is re-evaluated on. That is
// what keeps a tab open for eight hours from holding a thousand dead rows.
//
// # There is deliberately no claim lease
//
// notify's dispatcher leases the rows it claims, because resolving an audience
// twice costs a read and sending twice costs somebody a duplicate mail. Neither
// has an analogue here: `DELETE … WHERE seen_at < $1` is idempotent and
// commutative, so two replicas sweeping at once agree and the loser deletes
// nothing.
//
// Which is why this may be a goroutine at all. The rule it is departing from —
// "a subcommand rather than a goroutine, so it is a cron job rather than
// something racing itself in every replica" — is about work that racing can get
// wrong, and there is none here. The absence of a lease is a decision, not an
// omission.
type Sweeper struct {
	svc      *Service
	interval time.Duration
	grace    time.Duration

	mu      sync.Mutex
	stop    chan struct{}
	done    chan struct{}
	running bool
	closed  bool
}

// SweeperConfig is what a sweeper needs beyond a [Service].
type SweeperConfig struct {
	// Service is what it sweeps through, so the TTL and the pool are resolved
	// once rather than twice.
	Service *Service

	// Interval is how often a pass runs. Zero means [DefaultSweep]; negative
	// means never, which leaves the generated task as the only sweeper and is
	// the right setting for an operator who would rather this were a cron job.
	Interval time.Duration

	// Grace is how long past the TTL a row survives before it is deleted.
	//
	// It exists so the two expiry mechanisms cannot disagree. A subscriber stops
	// drawing a row at the TTL and this deletes it at TTL plus grace, so a row is
	// always invisible before it is gone — never the other way round, which is a
	// row that reappears when a slow client catches up. Zero means
	// [DefaultGrace].
	Grace time.Duration
}

// NewSweeper builds one. It does nothing until [Sweeper.Start].
func NewSweeper(cfg SweeperConfig) *Sweeper {
	if cfg.Service == nil {
		panic("presence: SweeperConfig.Service is required")
	}
	interval := cfg.Interval
	if interval == 0 {
		interval = DefaultSweep
	}
	grace := cfg.Grace
	if grace == 0 {
		grace = DefaultGrace
	}
	return &Sweeper{
		svc:      cfg.Service,
		interval: interval,
		grace:    grace,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start begins the ticker. A negative interval starts nothing and is not an
// error: it is how an operator says the cron job owns this.
//
// Idempotent, and safe on a sweeper that has already been closed. What it
// records is whether there is a goroutine for [Sweeper.Close] to wait for —
// starting nothing has to be told apart from starting something, because the
// difference is whether `done` will ever be closed.
func (s *Sweeper) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running || s.closed || s.interval < 0 {
		return
	}
	s.running = true
	go s.loop()
}

// loop sweeps on a ticker until Close.
//
// No nudge channel, and its absence mirrors notify: a nudge exists there so a
// notification that has just been committed is dispatched in milliseconds rather
// than at the next tick. Nothing about presence is waiting on this.
func (s *Sweeper) loop() {
	defer close(s.done)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			// The pass gets its own bounded context rather than one that lives as
			// long as the process: a sweep that cannot reach the database should
			// fail and be retried on the next tick, not hold a slot forever.
			ctx, cancel := context.WithTimeout(context.Background(), s.interval)
			_, _ = s.Sweep(ctx)
			cancel()
		}
	}
}

// Close stops the ticker and waits for a pass in flight.
//
// Register it before the pool closes, because what is in flight is a write:
//
//	app.CloseWithin("presence", 5*time.Second, sweeper.Close)
//
// There is no Drain step, unlike the notification engine. Nothing here is worth
// finishing — a pass interrupted mid-DELETE leaves rows that the next pass takes,
// in whichever replica runs it.
//
// Two ways it returns without waiting, and both are arrangements somebody
// actually has. **A sweeper that was never started has nothing to wait for** —
// which is what an operator who left the sweep to the cron job and kept this
// registration has, and a wait there would hold shutdown open for the whole
// timeout over a goroutine that does not exist. And the wait honours the context
// it is given, the way notify's engine does: a pass that cannot reach the
// database should not outlive the deadline the caller declared for it.
func (s *Sweeper) Close(ctx context.Context) error {
	s.mu.Lock()
	running := s.running
	if !s.closed {
		s.closed = true
		close(s.stop)
	}
	s.mu.Unlock()

	if !running {
		return nil
	}
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Sweep runs one pass.
//
// This is what the generated task calls, and it is safe to call while the ticker
// is running — for the same reason two replicas may: deleting rows that are
// already expired is idempotent, so the two passes agree and one of them finds
// nothing.
func (s *Sweeper) Sweep(ctx context.Context) (SweepReport, error) {
	var report SweepReport
	before := s.svc.cfg.Now().UTC().Add(-(s.svc.cfg.TTL + s.grace))

	// Until a pass comes back short. A single bounded statement would leave a
	// backlog that never shrinks on a table that had a spike, and the loop ends
	// on its own because every iteration deletes what it counted.
	for {
		n, err := s.svc.sweep(ctx, before, sweepBatch)
		report.Expired += n
		if err != nil {
			return report, err
		}
		if n < sweepBatch {
			return report, nil
		}
		if err := ctx.Err(); err != nil {
			return report, err
		}
	}
}
