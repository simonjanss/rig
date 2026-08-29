// Package tick is one safe ticker lifecycle, and nothing else.
//
// A package of its own rather than a file in `runtime/serve`, which is the other
// obvious home and where this started. `serve` is the process's lifecycle — it
// builds the pool, so it imports pgxpool — and the two modules that reach for a
// ticker, `notify` and `presence`, have no other reason to depend on the server
// framework. Putting sixty lines of `time.Ticker` there put `pgxpool` and
// `puddle` in both of their module graphs, and made `dbx.Pool`'s promise that a
// module taking one "never imports pgxpool" untrue for exactly the two modules it
// names.
//
// So: a leaf over `context`, `sync` and `time`, which anything may depend on. It
// is the same argument [github.com/simonjanss/rig/runtime/outbox] makes for
// itself, and the reason both of these can exist at all.
package tick

import (
	"context"
	"sync"
	"time"
)

// Ticker runs one pass on an interval and stops when it is told to.
//
// The interesting part is not the ticker, which is six lines. It is the four
// properties around it, and every one of them is a bug somebody has already had
// in a hand-rolled version:
//
//   - [Ticker.Start] is idempotent, because two goroutines both closing the same
//     `done` channel on the way out is a panic, in a shutdown path, from a
//     goroutine that nothing is recovering.
//   - A Start after a [Ticker.Close] starts nothing, for the same reason.
//   - A Close with no prior Start returns immediately. Waiting on a goroutine
//     that does not exist holds shutdown open for the whole registered timeout
//     and then reports a failure — which is what an operator who left the work
//     to a cron job and kept the shutdown registration has.
//   - A second Close, or two at once, is not a second close of a channel. The
//     check and the close are under one lock, because as a check-then-act they
//     are a race between two callers that both take the branch.
//
// Close takes a context and returns an error so that it registers directly:
//
//	app.CloseWithin("notifications", 15*time.Second, engine.Close)
//
// What it does not do is decide anything about the work. There is no error
// return from a pass, no backoff between passes, and no lease: a pass that fails
// is a pass, and the next one is coming. Anything that needs to be told apart
// from that belongs in [Config.Pass], where the thing being done knows what it
// means.
//
// Safe for concurrent use.
type Ticker struct {
	cfg Config

	nudge chan struct{}

	mu      sync.Mutex
	stop    chan struct{}
	done    chan struct{}
	running bool
	closed  bool
}

// Config is what a [Ticker] needs.
type Config struct {
	// Interval is how often Pass runs.
	//
	// Zero or negative starts nothing, and is not an error: it is how an operator
	// says a cron job owns this. A caller with a default of its own resolves it
	// before it gets here, because there is no interval a library can pick for
	// somebody else's work.
	Interval time.Duration

	// Pass is one run.
	//
	// Called from one goroutine and never concurrently with itself, so it needs
	// no lock of its own against another pass — only against whatever else in the
	// process touches the same rows. It must not panic: nothing here may take a
	// process down over a pass that failed.
	//
	// A nil Pass starts nothing, the same as a zero Interval.
	Pass func(ctx context.Context)

	// PassTimeout bounds one pass. Zero is unbounded.
	//
	// Unbounded is the default because it is the answer that cannot be wrong.
	// Work that already declares its own deadline — a claim lease longer than
	// this interval, most often — would be cut off mid-write by a timeout set
	// here, and cut off *before* the statement that gives the claims back. Set
	// this only where the pass has no deadline of its own, and then the interval
	// is usually the number: a pass that cannot reach the database should fail
	// and be retried on the next tick rather than hold the slot forever.
	PassTimeout time.Duration
}

// New builds one. It does nothing until [Ticker.Start].
func New(cfg Config) *Ticker {
	return &Ticker{
		cfg:   cfg,
		nudge: make(chan struct{}, 1),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
}

// Start begins the ticker. Idempotent, and safe on one already closed.
//
// What it records is whether there is a goroutine for [Ticker.Close] to wait
// for. Starting nothing has to be told apart from starting something, because
// the difference is whether `done` will ever be closed.
func (t *Ticker) Start() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.running || t.closed || t.cfg.Interval <= 0 || t.cfg.Pass == nil {
		return
	}
	t.running = true
	go t.loop()
}

// Nudge asks for a pass now.
//
// An optimization, and nothing may be built on it. A dropped nudge — and they
// are dropped, because the channel holds one — costs at most one interval. A
// caller that never nudges pays nothing for this existing, and a nudge before
// [Ticker.Start] or after [Ticker.Close] is not an error.
func (t *Ticker) Nudge() {
	select {
	case t.nudge <- struct{}{}:
	default:
	}
}

func (t *Ticker) loop() {
	defer close(t.done)

	ticker := time.NewTicker(t.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-t.stop:
			return
		case <-ticker.C:
		case <-t.nudge:
		}
		t.pass()
	}
}

// pass runs one, under its own timeout if there is one. Separate from loop so
// that the cancel is deferred per pass rather than per goroutine.
func (t *Ticker) pass() {
	ctx := context.Background()
	if t.cfg.PassTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t.cfg.PassTimeout)
		defer cancel()
	}
	t.cfg.Pass(ctx)
}

// Close stops the ticker and waits for a pass in flight, honouring ctx.
//
// A ticker that was never started has nothing to wait for and says so at once.
// A ticker that was returns when the pass in flight does, or when ctx is done —
// whichever happens first, because a pass that cannot reach the database should
// not outlive the deadline its caller declared.
//
// Idempotent. Calling it twice, or from two goroutines, is safe.
func (t *Ticker) Close(ctx context.Context) error {
	t.mu.Lock()
	running := t.running
	if !t.closed {
		t.closed = true
		close(t.stop)
	}
	t.mu.Unlock()

	if !running {
		return nil
	}
	select {
	case <-t.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
