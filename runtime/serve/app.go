package serve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
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
	// checks are the dependencies readiness asks about, beyond the pool.
	checks []check

	// limits are [Config.Shutdown]'s numbers, keyed by step name, and they
	// replace whatever registers a step of that name. Nil is the ordinary case
	// and means every step keeps the limit it was registered with.
	limits map[string]time.Duration

	// until is the deadline for the whole stop sequence, set when it begins.
	until time.Time
	// closed guards against tearing down twice, since the teardown runs from a
	// defer that also covers a failure during startup.
	closed bool
}

// Step is one shutdown step named, and how long it may take.
//
// It is what [Shutdown] is a set of, and what [Config.Shutdown] carries: a
// number in it replaces the one whatever registered that step asked for. The
// name is the one given to [App.Drain], [App.DrainWithin], [App.Close] or
// [App.CloseWithin] — for rig's own steps, "traces", "notifications",
// "presence", "shapes" and "auth", which the generated api package's Shutdown
// spells as fields rather than as strings.
type Step struct {
	// Name is the step this is about, and a name nothing registers is refused
	// before the server listens rather than silently doing nothing.
	Name string

	// Timeout is how long that step may take. Zero leaves it alone, which is
	// what makes a partly-filled set mean "these, and the rest as generated".
	Timeout time.Duration
}

// Shutdown is how long each of rig's own shutdown steps may take, for a
// deployment that wants numbers other than the ones it was generated with.
//
// An interface rather than a slice field so that the thing filling it can be a
// struct with a field per step this project actually registers — which is what
// the generated api package emits, and what makes a step name a compile error
// rather than a string that quietly matches nothing:
//
//	serve.Config{
//		MaxShutdown: 47 * time.Second,
//		Shutdown: api.Shutdown{
//			Notifications: 10 * time.Second,
//			Presence:      2 * time.Second,
//		},
//	}
//
// It belongs to the deployment rather than to rig.yaml for the reason the span
// destination and the monitoring address do: one binary runs on a laptop, in CI
// and in production, and how long a stop may take is usually decided by a
// terminationGracePeriodSeconds somebody else set. A number here is one an
// environment can supply.
type Shutdown interface {
	// Steps is the set, and only the steps it names are affected.
	Steps() []Step
}

type step struct {
	// Step is the name, and the limit this step is capped at on its own, over
	// and above the budget for the whole sequence. A zero limit means the
	// sequence's remaining time is the only one.
	Step

	fn func(context.Context) error
}

// check is one dependency ReadinessPath asks about.
type check struct {
	name  string
	probe func(context.Context) error
}

// Ready registers a dependency this instance cannot serve without, so that
// [Config.ReadinessPath] fails while it is away and names it when it does.
//
// It is here rather than on the Config for the reason Drain and Close are: the
// thing being checked is built by the mount function, and a Config is filled in
// by a main that runs before the mount. [Config.Ready] is the same hook for a
// dependency that does exist by then.
//
// Register only what genuinely makes this instance not worth sending work to.
// The temptation is to list everything the server talks to, and the cost of
// giving in is the outage the probe was supposed to contain: one dependency
// being slow takes every replica out of the load balancer at once, including the
// requests that never touch it. A dependency the server degrades gracefully
// without — a sync service whose shapes fall back to a snapshot, a cache — is
// something to log and to show, not something to fail readiness on.
//
// The probes run after the pool ping and after Config.Ready, in the order they
// were registered, and all of them share [Config.ProbeTimeout].
func (a *App) Ready(name string, probe func(ctx context.Context) error) {
	if probe != nil {
		a.checks = append(a.checks, check{name: name, probe: probe})
	}
}

// readyChecks is what readiness runs, for the one caller in this package.
func (a *App) readyChecks() []check {
	if a == nil {
		return nil
	}
	return a.checks
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
		a.drain = append(a.drain, step{Step: a.within(name, timeout), fn: f})
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
		a.stop = append(a.stop, step{Step: a.within(name, timeout), fn: f})
	}
}

// within is what a step is registered as, once [Config.Shutdown] has had its
// say about the name.
//
// The deployment's number wins over the caller's, which is the whole point of
// the field: what registers rig's own steps is generated code, so there is
// nowhere else for a project to disagree with it. A step nobody named keeps
// what it asked for, and the names that were named but never registered are
// answered for in [App.checkShutdown] — here there is nothing yet to compare
// them against.
func (a *App) within(name string, timeout time.Duration) Step {
	if d, ok := a.limits[name]; ok {
		timeout = d
	}
	return Step{Name: name, Timeout: timeout}
}

// limit records what [Config.Shutdown] said, before the mount function
// registers anything.
//
// Zero timeouts are dropped rather than recorded: a set is a struct with a
// field per step, so most of its fields are zero most of the time, and a zero
// that overrode a real limit would make "I did not say" mean "no limit at all".
func (a *App) limit(shutdown Shutdown) {
	if shutdown == nil {
		return
	}
	for _, s := range shutdown.Steps() {
		if s.Timeout <= 0 {
			continue
		}
		if a.limits == nil {
			a.limits = make(map[string]time.Duration)
		}
		a.limits[s.Name] = s.Timeout
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

// RunDrain stops the things that pull their own work, in the order they were
// registered.
//
// [Run] calls it at the start of the shutdown, once readiness is already false
// and before the server stops accepting. It is exported for the caller that
// builds the shipped handler from a bare pool and owns the ending itself — an
// integration test standing an httptest.Server over what main mounts, whose
// Close blocks on exactly the long polls only this releases.
//
// Every failure is logged where it happens as well as returned, for the reason
// [App.runClose] gives. Logger is what that line goes to, and [Run] is what
// normally sets it — so an App somebody built themselves may have none, and
// [App.log] is why that is a default rather than a panic on the one path where
// something has already gone wrong.
func (a *App) RunDrain(ctx context.Context) error { return a.runDrain(ctx) }

// log is where this App says things, for the caller that built one without a
// logger. Run always sets the field; nothing makes a caller who did not go
// through it do the same, and discovering that from a nil dereference inside a
// failed shutdown step is the worst moment to discover it.
func (a *App) log() *slog.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return slog.Default()
}

// runDrain stops the things that pull their own work.
func (a *App) runDrain(ctx context.Context) error {
	var failed error
	for _, s := range a.drain {
		if err := a.run(ctx, s); err != nil {
			a.log().ErrorContext(ctx, "draining failed", "step", s.Name, "error", err)
			failed = errors.Join(failed, fmt.Errorf("drain %s: %w", s.Name, err))
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
			a.log().ErrorContext(closing, "shutdown failed", "step", s.Name, "error", err)
			failed = errors.Join(failed, fmt.Errorf("close %s: %w", s.Name, err))
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
	if s.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.Timeout)
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
		if s.Timeout > 0 {
			total += s.Timeout
			parts = append(parts, fmt.Sprintf("drain %s %s", s.Name, s.Timeout))
		}
	}
	for _, s := range a.stop {
		if s.Timeout > 0 {
			total += s.Timeout
			parts = append(parts, fmt.Sprintf("close %s %s", s.Name, s.Timeout))
		}
	}
	return total, parts
}

// reserved is what the teardown will need once the server has stopped answering.
//
// It is the close half of [App.declared], separately, because the two halves are
// spent at different times: the drain steps run before the server stops
// accepting and are already over by the time this matters, and these run after
// it. What is left between them is the requests in flight.
//
// A step with no limit of its own reserves nothing. That is the same rule
// declared uses and it means the same thing here: such a step takes whatever
// remains, so there is nothing to hold back on its behalf.
func (a *App) reserved() time.Duration {
	var total time.Duration
	for _, s := range a.stop {
		total += s.Timeout
	}
	return total
}

// checkLimits refuses a [Config.Shutdown] that named a step nothing registered.
//
// It is the same failure the configuration blocks are refused for: a number
// somebody set and believed in, read by nothing. Here it is worth more than
// elsewhere, because the belief is about a shutdown — the one part of a
// deployment nobody watches until the day it drops requests, and by then the
// evidence is a process that was killed rather than a value that did nothing.
//
// It can only be asked once the mount function has run, which is why it is part
// of [App.checkShutdown] rather than of the configuration's own checks: what a
// project registers depends on what its wiring built. A nil engine means no
// notification step, so a set that sizes one is a set describing a server this
// is not.
func (a *App) checkLimits() error {
	if len(a.limits) == 0 {
		return nil
	}

	registered := make(map[string]bool, len(a.drain)+len(a.stop))
	names := make([]string, 0, len(a.drain)+len(a.stop))
	for _, steps := range [][]step{a.drain, a.stop} {
		for _, s := range steps {
			if !registered[s.Name] {
				registered[s.Name] = true
				names = append(names, s.Name)
			}
		}
	}

	var unread []string
	for name := range a.limits {
		if !registered[name] {
			unread = append(unread, name)
		}
	}
	if len(unread) == 0 {
		return nil
	}
	sort.Strings(unread)

	had := "nothing registered a shutdown step at all"
	if len(names) > 0 {
		had = "what this server registered is " + strings.Join(names, ", ")
	}
	return fmt.Errorf(
		"serve: Config.Shutdown sizes %s, which nothing here registers, so the number is read by nobody: %s",
		strings.Join(unread, ", "), had)
}

// checkShutdown refuses a shutdown whose parts do not fit inside its whole, and
// one whose whole was never stated.
//
// It runs before the server listens, because that is the only time the answer
// is useful. A step given thirty seconds inside a twenty-second budget is a
// step that can never finish, and finding that out during an actual shutdown
// means finding out from a truncated flush in a process that is already going
// away.
//
// The two refusals are one question asked at one moment, which is why they are
// here together rather than split across [Config.checkStartup] and this: both
// are answered by what the mount function actually registered, and neither can
// be answered before it has. A budget nobody stated is the same failure as one
// that does not fit — a shutdown nobody has thought about — and it is worth more
// as a process that will not start than as a truncated flush six months later.
//
// Both messages carry the parts, because the number this produces is the one
// somebody has to write into a deployment, and the only alternative to reading
// it here is adding up constants across three files.
func (a *App) checkShutdown(ctx context.Context, max, drainDelay time.Duration) error {
	if err := a.checkLimits(); err != nil {
		return err
	}

	total, parts := a.declared(drainDelay)

	if max <= 0 {
		// A project with nothing of its own registered still has to state one:
		// what the budget buys there is entirely the requests in flight, and
		// how long those are worth waiting for is not this package's guess to
		// make.
		reserved := "nothing here reserves time of its own, so the whole of it is for the requests in flight"
		if len(parts) > 0 {
			reserved = fmt.Sprintf("the steps registered here reserve %s (%s), and the requests in flight get what is left",
				total, strings.Join(parts, " + "))
		}
		return fmt.Errorf(
			"serve: MaxShutdown is not set and has no default: %s. "+
				"State the longest the whole stop sequence may take — it is the "+
				"number that belongs in terminationGracePeriodSeconds",
			reserved)
	}

	if total > max {
		return fmt.Errorf(
			"serve: the shutdown asks for %s but MaxShutdown allows %s (%s): "+
				"raise MaxShutdown or lower a step",
			total, max, strings.Join(parts, " + "))
	}

	// Whatever is left over is what requests in flight get. None is legal and
	// almost never meant.
	if total == max && total > 0 {
		a.log().WarnContext(ctx, "the shutdown budget is fully spoken for, leaving nothing for requests in flight",
			"max", max, "declared", total)
	}
	return nil
}

// stopContext shares the deadline of a shutdown already under way, so that the
// budget is for the whole sequence rather than for each part of it.
//
// A timeout of zero or less is no deadline of its own rather than one already
// in the past. It is reachable from one place — [Run] defers its teardown before
// the mount function runs and therefore before [App.checkShutdown] has had a
// chance to refuse an unset MaxShutdown, so the steps that mount registered are
// closed under a budget that was never stated. Giving them an expired context
// there would answer a config error with a paragraph of "gave up waiting" from
// steps that were never the problem, joined onto the one message that was.
//
// Not a fallback duration, which would be the default this field deliberately
// does not have, reintroduced one layer down where nobody would find it.
func (a *App) stopContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if !a.until.IsZero() {
		return context.WithDeadline(context.WithoutCancel(ctx), a.until)
	}
	if timeout <= 0 {
		return context.WithCancel(context.WithoutCancel(ctx))
	}
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}
