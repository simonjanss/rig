package serve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// App is what a mount function is given: the pool, a logger, and somewhere to
// say how the things it builds are shut down.
//
// A server is rarely only a server. There is a queue consumer, a cache, a
// metrics exporter, a client with a connection pool of its own — each built
// while the routes are being wired, and each needing to stop in a particular
// order relative to the HTTP server. Without somewhere to record that, a main
// function ends up with a package-level variable per dependency and a shutdown
// function that has to be kept in step with a constructor somewhere else.
type App struct {
	// Pool is the database, already connected and pinged.
	Pool *pgxpool.Pool

	// Logger is the server's, so a request line, a shutdown step and anything
	// a dependency says all land in the same place. Run always sets it, to
	// Config.Logger or the default.
	Logger *slog.Logger

	drain []step
	stop  []step

	// until is the deadline for the whole stop sequence, set when it begins.
	until time.Time
	// closed guards against tearing down twice, since the teardown runs from a
	// defer that also covers a failure during startup.
	closed bool
}

type step struct {
	name string
	fn   func(context.Context) error

	// timeout caps this step on its own, over and above the budget for the
	// whole sequence. Zero means the sequence's remaining time is the only
	// limit.
	timeout time.Duration
}

// Drain registers work for the start of the shutdown: after readiness turns
// false, before the server stops accepting requests.
//
// This is for anything that fetches its own work rather than being handed it —
// a queue consumer, a scheduler, a poller. Stopping those first means the
// process spends its drain window finishing what it has instead of picking up
// more it will not finish. Requests in flight are unaffected: the server is
// still serving.
func (a *App) Drain(name string, f func(ctx context.Context) error) {
	a.DrainWithin(name, 0, f)
}

// DrainWithin is Drain with a limit of its own.
//
// Use it for something whose stop is unbounded — a fetch loop mid-request, a
// long poll — so that the rest of the shutdown is not spent waiting on it.
func (a *App) DrainWithin(name string, timeout time.Duration, f func(ctx context.Context) error) {
	if f != nil {
		a.drain = append(a.drain, step{name: name, fn: f, timeout: timeout})
	}
}

// Close registers work for after the server has stopped answering.
//
// Registered functions run in reverse: the last thing built is the first thing
// closed, because the order things were built in is the order their
// dependencies came to exist. Everything registered runs even if one fails, and
// all of it runs before the pool closes — so a final write still has a database
// to write to.
//
// A dependency registered before the server ever listens is closed too. Failing
// halfway through startup is exactly when a half-built process most needs
// taking apart.
func (a *App) Close(name string, f func(ctx context.Context) error) {
	a.CloseWithin(name, 0, f)
}

// CloseWithin is Close with a limit of its own.
//
// Without one, a step is bounded only by what is left of MaxShutdown, which
// is the right default: the sequence as a whole cannot overrun, and a flush
// that legitimately takes ten seconds is not cut off at an arbitrary two. Give
// a step its own limit when it should not be allowed to spend the budget the
// steps after it will need.
func (a *App) CloseWithin(name string, timeout time.Duration, f func(ctx context.Context) error) {
	if f != nil {
		a.stop = append(a.stop, step{name: name, fn: f, timeout: timeout})
	}
}

// CloseFunc registers something whose shutdown takes no context, which is most
// third-party clients.
func (a *App) CloseFunc(name string, f func() error) {
	if f == nil {
		return
	}
	a.Close(name, func(context.Context) error { return f() })
}

// runDrain stops the things that pull their own work.
func (a *App) runDrain(ctx context.Context) error {
	var failed error
	for _, s := range a.drain {
		if err := a.run(ctx, s); err != nil {
			a.Logger.ErrorContext(ctx, "draining failed", "step", s.name, "error", err)
			failed = errors.Join(failed, fmt.Errorf("drain %s: %w", s.name, err))
		}
	}
	return failed
}

// runClose tears down in reverse, whatever happened.
//
// Every failure is both logged where it happens and returned. That looks like
// saying it twice, and the reason it is not is the step after it: one that
// hangs past the budget leaves the process to be killed from outside, and then
// the error this returns never reaches anybody. The log line is the record that
// survives; the returned error is what the caller decides about.
func (a *App) runClose(ctx context.Context, timeout time.Duration) error {
	if a.closed {
		return nil
	}
	a.closed = true

	if len(a.stop) == 0 {
		return nil
	}

	// The teardown outlives whatever cancelled the server: deriving from a
	// cancelled context would give every dependency zero time to close.
	closing, cancel := a.stopContext(ctx, timeout)
	defer cancel()

	var failed error
	for i := len(a.stop) - 1; i >= 0; i-- {
		s := a.stop[i]
		if err := a.run(closing, s); err != nil {
			a.Logger.ErrorContext(closing, "shutdown failed", "step", s.name, "error", err)
			failed = errors.Join(failed, fmt.Errorf("close %s: %w", s.name, err))
		}
	}
	return failed
}

// run gives one step its deadline and stops waiting when it passes.
//
// The wait is bounded whether or not the step is: a function that ignores the
// context it was handed would otherwise hold the process open until something
// outside kills it, and take the steps after it down with it. When that
// happens the goroutine is left behind — which is a leak in a process that is
// about to stop existing, and the better of the two outcomes.
func (a *App) run(ctx context.Context, s step) error {
	if s.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}

	done := make(chan error, 1)
	go func() { done <- s.fn(ctx) }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("gave up waiting: %w", ctx.Err())
	}
}

// declared is what the registered steps have asked for, and what each part of
// it is, for an error message that can be acted on.
//
// A step with no limit of its own asks for nothing here: it takes whatever is
// left, which is exactly why the ones that do declare have to fit.
func (a *App) declared(drainDelay time.Duration) (time.Duration, []string) {
	var (
		total time.Duration
		parts []string
	)

	if drainDelay > 0 {
		total += drainDelay
		parts = append(parts, fmt.Sprintf("drain delay %s", drainDelay))
	}
	for _, s := range a.drain {
		if s.timeout > 0 {
			total += s.timeout
			parts = append(parts, fmt.Sprintf("drain %s %s", s.name, s.timeout))
		}
	}
	for _, s := range a.stop {
		if s.timeout > 0 {
			total += s.timeout
			parts = append(parts, fmt.Sprintf("close %s %s", s.name, s.timeout))
		}
	}
	return total, parts
}

// checkShutdown refuses a shutdown whose parts do not fit inside its whole.
//
// It runs before the server listens, because that is the only time the answer
// is useful. A step given thirty seconds inside a twenty-second budget is a
// step that can never finish, and finding that out during an actual shutdown
// means finding out from a truncated flush in a process that is already going
// away.
func (a *App) checkShutdown(ctx context.Context, max, drainDelay time.Duration) error {
	total, parts := a.declared(drainDelay)

	if total > max {
		return fmt.Errorf(
			"serve: the shutdown asks for %s but MaxShutdown allows %s (%s): "+
				"raise MaxShutdown or lower a step",
			total, max, strings.Join(parts, " + "))
	}

	// Whatever is left over is what requests in flight get. None is legal and
	// almost never meant.
	if total == max && total > 0 {
		a.Logger.WarnContext(ctx, "the shutdown budget is fully spoken for, leaving nothing for requests in flight",
			"max", max, "declared", total)
	}
	return nil
}

// stopContext shares the deadline of a shutdown already under way, so that the
// budget is for the whole sequence rather than for each part of it.
func (a *App) stopContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if !a.until.IsZero() {
		return context.WithDeadline(context.WithoutCancel(ctx), a.until)
	}
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}
