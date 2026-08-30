// Package serve runs a rig server: a connection pool, an HTTP server, and a
// shutdown that finishes what it started.
//
// None of that is interesting, and all of it is easy to get subtly wrong — a
// pool that is never pinged so a bad password surfaces on the first request, a
// server with no header timeout, a SIGTERM that drops requests in flight. It is
// the same forty lines in every project, so it is here instead, and a main.go
// is left with the part that is actually about the application: which services
// are wired to which repositories, and how a caller is identified.
package serve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Mount builds the handler once the pool is open.
//
// It returns a handler rather than being handed a mux to fill in, and that is
// the extension point. Cross-cutting concerns — tracing, a request log, panic
// recovery, compression — are wrappers around a handler, so a mount function
// that ends in one line per concern reads in the order they run:
//
//	mux := http.NewServeMux()
//	api.Register(mux, api.Handlers{…})
//	return otelhttp.NewHandler(logRequests(mux), "todo"), nil
//
// It also leaves the router open: a project that wants a second API on another
// prefix, static files, or something other than [http.ServeMux] registers what
// it likes and returns that.
//
// What this package adds is deliberately outside whatever comes back. The
// liveness and readiness paths are answered before the returned handler is
// reached, so a probe every second is not a traced request, a line in the
// request log, or a row in a latency histogram.
//
// Anything it builds that has to be shut down is registered on the [App] it is
// given, next to the line that built it.
type Mount func(ctx context.Context, app *App) (http.Handler, error)

// Task is work that runs against the database and finishes: a migration, a
// backfill, a nightly job. It is the same shape everywhere in this package, so
// the function that migrates on the way up and the one behind a subcommand can
// be the same value.
type Task func(ctx context.Context, pool *pgxpool.Pool) error

// NoProbe is a LivenessPath or ReadinessPath that should not be served.
//
// A probe path has no zero value meaning "none": the empty string is what a
// config that said nothing looks like, and telling those two apart is the whole
// point. So not wanting one is said with this rather than by omission, and a
// project that forgot gets an error instead of a server with no way to be
// checked.
//
// Not a path, so it cannot collide with one: an HTTP server is only ever asked
// for a route beginning with a slash.
const NoProbe = "-"

// Config is everything the server needs that is not a route.
//
// Nothing here has a default. A value this package invented for a config that
// said nothing is one nobody chose, discovered only by what it costs when it is
// wrong — so every field that would otherwise be one is refused unset, all of
// them named at once, before anything opens or listens. What a deployment
// supplies rather than states is still read from the environment: DatabaseURL
// and Addr fall back to $DATABASE_URL and $ADDR, and are refused after that.
//
// The exceptions are the fields where saying nothing means there is nothing to
// do rather than something to pick — Ready, Pool, Monitor, the On… hooks, Hint,
// Tasks, Migrate and DrainDelay. Nil is the whole of what those have to say.
type Config struct {
	// DatabaseURL is the connection string. Empty means $DATABASE_URL, and
	// failing that the server refuses to start rather than guessing.
	DatabaseURL string

	// Addr is the listen address. Empty means $ADDR, and refused after that.
	Addr string

	// Monitor is served on a listener of its own, at MonitorAddr, in this same
	// process. Nil — the default — opens no second listener.
	//
	// It is an http.Handler and not rig's monitoring page, because this package
	// is imported by every generated server and rig/observe brings
	// OpenTelemetry with it. Pass observe's:
	//
	//	Monitor:     page.Handler(),
	//	MonitorAddr: page.Addr(),
	//
	// which are both zero when the page is unarmed, so a deployment with no
	// monitoring password opens no port rather than one that refuses.
	//
	// A separate listener rather than a route on the handler below, because a
	// bind address is a boundary the kernel keeps and a path is not: bound to
	// loopback, the page is reachable by this machine and by nothing else, in
	// every deployment and behind every proxy. Anything else that should be
	// reachable on those terms and not on the API's — a pprof mux, an operator
	// endpoint — belongs here too.
	Monitor http.Handler

	// MonitorAddr is where Monitor listens, as host:port. Required when Monitor
	// is set, and refused when it is not: the two say one thing together, and
	// either one alone is a monitoring page somebody believes is running.
	//
	// There is no default and no environment fallback here, unlike Addr. What
	// fills it in is observe's own $RIG_MONITOR_ADDR, resolved before the page
	// is built, so that the address and the page's own idea of it cannot
	// disagree.
	MonitorAddr string

	// LivenessPath answers whether the process is running, and nothing else.
	//
	// Required, like ReadinessPath: a path this package chose would be one an
	// orchestrator has to be told about anyway. [NoProbe] is how a project says
	// it wants none.
	// It never touches the database, and that is the whole point: a liveness
	// probe that fails when a dependency does turns one database blip into
	// every replica being killed at once, which is the outage the restart was
	// supposed to prevent.
	//
	// Empty registers nothing.
	LivenessPath string

	// ReadinessPath answers whether this instance should be sent traffic. It
	// pings the database, runs Ready if there is one, and turns false the
	// moment a shutdown begins — so a load balancer stops sending work before
	// the server stops accepting it.
	//
	// Empty registers nothing.
	ReadinessPath string

	// Ready is checked by ReadinessPath after the database, for a dependency
	// this package knows nothing about. An error means not ready, and its text
	// goes to the caller: a readiness probe is read by whoever is debugging the
	// deployment, not by the public.
	Ready func(ctx context.Context) error

	// DrainDelay is how long to keep serving after readiness turns false and
	// before the shutdown starts. Zero is none, which is a real answer here
	// rather than an absent one. It comes out of MaxShutdown either way.
	//
	// It exists because removing an instance from a load balancer is not
	// instant: the probe has to fail, and the change has to propagate. Requests
	// sent during that window arrive at a server that has already stopped
	// accepting them. A delay of a few seconds is what turns a rolling deploy
	// with dropped requests into one without.
	DrainDelay time.Duration

	// Logger receives the two lines this package has to say. Nil uses the
	// default logger.
	Logger *slog.Logger

	// Pool tunes the pool before it is opened — MaxConns, an AfterConnect
	// hook, a tracer.
	Pool func(*pgxpool.Config) error

	// Tasks are subcommands: an argument that names one runs it against an
	// open pool and exits, instead of starting the server.
	//
	//	Tasks: map[string]serve.Task{"migrate": migrate.Apply(migrations, opt)}
	//
	// It is here rather than in every main function because the alternative is
	// everybody writing the same three lines against os.Args, and because a
	// task and a server that must connect identically should be reading one
	// configuration.
	//
	// Only Main dispatches them. Run always serves.
	Tasks map[string]Task

	// Migrate runs before the server listens, with the pool already open. An
	// error stops the process before it has served anything.
	//
	// This package does not know what a migration is; rig/migrate does, and
	// keeping the two apart is why an application that migrates some other way
	// carries no dependency on it.
	//
	// Whether this should apply migrations or only refuse to start without them
	// is a real decision with a wrong answer at scale. rig/migrate's package
	// documentation lays out the three places migrations can run and what each
	// costs; migrate.Require is the one to reach for by default, and
	// migrate.Apply the one for a single instance.
	Migrate Task

	// MaxStartup is the longest the process may take between Run being called
	// and the server listening: opening the pool, the Migrate hook, and the
	// mount function.
	//
	// The counterpart of MaxShutdown, and for the same reason. A boot that
	// hangs on something slow is not obviously different from a boot that is
	// merely slow, and the difference matters most at three in the morning. A
	// project that migrates on the way up should raise this: the migration is
	// inside the budget.
	MaxStartup time.Duration

	// ConnectTimeout bounds the first connection, inside MaxStartup — soon
	// enough to say "the database" rather than "startup" when that is what went
	// wrong.
	//
	// Setting it longer than MaxStartup is refused: connecting alone would use
	// the whole budget.
	ConnectTimeout time.Duration

	// Hint is what to tell somebody whose database is not there — most often
	// how to start one:
	//
	//	Hint: "run `rig db up` to start a local Postgres, or set $DATABASE_URL"
	//
	// It is said as soon as the first connection attempt fails rather than at
	// the end of ConnectTimeout, and again with the error if the wait runs out.
	// The reason it is a field and not a sentence in this package is that only
	// the project knows how its database is meant to be started: a container in
	// development, a managed instance in production, and printing the wrong one
	// is worse than printing nothing.
	Hint string

	// ProbeTimeout bounds what a readiness check is allowed to wait for: a probe
	// that hangs is a probe that has already failed.
	ProbeTimeout time.Duration

	// ReadHeaderTimeout bounds how long a client may take to send its headers.
	// Worth stating rather than turning off: without it one open connection
	// sending nothing occupies a goroutine forever.
	ReadHeaderTimeout time.Duration
	// ReadTimeout bounds the whole request.
	ReadTimeout time.Duration
	// WriteTimeout bounds the response. Its clock starts when the request's
	// headers were read rather than when the body starts going out.
	//
	// It is the bound for an ordinary route. A route that legitimately outlives
	// it lifts it for its own request through http.NewResponseController — the
	// file transfers and the shape proxy both do — rather than this field being
	// raised to suit the slowest thing in the application, which would weaken
	// every other route to do it. That is why there is no per-route timeout
	// here and should not be one.
	WriteTimeout time.Duration
	// IdleTimeout bounds a kept-alive connection between requests.
	IdleTimeout time.Duration

	// MaxShutdown is the longest the whole stop sequence may take, from the
	// signal to the process being gone: the drain delay, the drain steps, the
	// requests in flight, and every dependency registered with App.Close.
	//
	// Required. It is the only field here with no default, because it is the
	// only one that leaves the program: this is the number that belongs in
	// Kubernetes' terminationGracePeriodSeconds, and a default would be a value
	// nobody wrote deciding how long something outside this process waits before
	// sending SIGKILL. A server that is stopped faster than its own budget does
	// not drain, it is killed mid-flight.
	//
	// It is a stated maximum rather than the sum of its parts so that it can be
	// read off and copied — adding up whatever the mount function happened to
	// register is not something anybody will do twice. The parts are checked
	// against it before the server listens, so the two cannot drift apart
	// quietly, and a config that states nothing is refused there with the total
	// and each part named. That refusal is where the number to write comes from.
	MaxShutdown time.Duration

	// Shutdown sizes the individual steps inside MaxShutdown, for a deployment
	// that wants numbers other than the ones they were registered with.
	//
	// The generated api package's Shutdown is what fills it, and it is a struct
	// with a field per step this project actually registers:
	//
	//	Shutdown: api.Shutdown{
	//		Notifications: 10 * time.Second,
	//		Presence:      2 * time.Second,
	//	},
	//
	// Optional, and the ordinary case is to leave it out: the numbers a step is
	// registered with are rig's answer to what that step costs, and MaxShutdown
	// is already the sum of them. This is for the deployment those numbers do
	// not suit — most often one whose terminationGracePeriodSeconds was decided
	// by somebody else, which is exactly the fact rig.yaml cannot carry because
	// the same build runs where it is thirty seconds and where it is five.
	//
	// It does not raise MaxShutdown. The parts are checked against the whole
	// before the server listens either way, so a set that outgrows the budget is
	// a process that refuses to start and names what no longer fits, and a step
	// named here that nothing registers is refused there too.
	Shutdown Shutdown

	// OnListen is called with the address actually bound, which is the only
	// way to learn it when Addr asks for port zero.
	OnListen func(net.Addr)

	// OnMonitorListen is OnListen for MonitorAddr, and is not called at all
	// when there is no second listener.
	//
	// A second callback rather than a role argument on the first, so that a
	// test waiting for one port cannot be handed the other and block until it
	// times out.
	OnMonitorListen func(net.Addr)

	// OnExit runs last, on every way out of Main, including the ones that exit
	// non-zero.
	//
	// It exists because `defer` does not survive os.Exit, and Main exits: a
	// flush written as a deferred call in a main function runs when the server
	// stopped cleanly and is skipped on the three paths where something went
	// wrong — a task that failed, a boot that failed, a subcommand that does
	// not exist. Those are the runs whose spans and logs somebody actually
	// wants.
	//
	// Only Main calls it. Run returns, so a caller using Run directly still has
	// its own defers.
	OnExit func()
}

// Main runs a server until it is asked to stop, then exits.
//
// It is Run with the signals wired up and a non-zero exit on failure — the
// whole of a main function, so that a main function can be the wiring and
// nothing else. Use Run directly to keep control of the process.
func Main(cfg Config, mount Mount) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The first signal starts the shutdown; the second one should end it.
	//
	// Until stop is called the handler is still installed, so a second SIGTERM
	// goes into a channel nobody is reading and does nothing at all. That is
	// the wrong answer for the case it arrives in: somebody watching a drain
	// that is not progressing, whose only remaining move would otherwise be to
	// find the pid and send SIGKILL. Giving the signal back to the default
	// disposition here means the second one is the one that kills.
	go func() {
		<-ctx.Done()
		stop()
	}()

	// Every exit below goes through this, because a deferred call would not:
	// os.Exit runs no defers, and the paths that exit are the ones worth having
	// a trace of.
	exit := func(code int) {
		if cfg.OnExit != nil {
			cfg.OnExit()
		}
		stop()
		os.Exit(code)
	}
	done := func() {
		if cfg.OnExit != nil {
			cfg.OnExit()
		}
	}

	name, err := taskName(os.Args[1:], cfg.Tasks)
	if err != nil {
		reporter(cfg).ErrorContext(ctx, "cannot run that", "error", err)
		exit(2)
	}

	if name != "" {
		if err := Once(ctx, cfg, cfg.Tasks[name]); err != nil {
			reporter(cfg).ErrorContext(ctx, name+" failed", "error", err)
			exit(1)
		}
		done()
		return
	}

	if err := Run(ctx, cfg, mount); err != nil {
		// A shutdown that went badly is not a server that failed. Every step
		// that failed has already said so, at the moment it failed, so there is
		// nothing to repeat here — and nothing worth telling the orchestrator
		// that the process crashed.
		if Unclean(err) {
			reporter(cfg).ErrorContext(ctx, "stopped, but not cleanly")
			done()
			return
		}

		reporter(cfg).ErrorContext(ctx, "server stopped", "error", err)
		exit(1)
	}
	done()
}

// reporter is what [Main] says something went wrong with.
//
// Not a default for [Config.Logger] — that one is refused unset, and this is
// how the refusal gets out. A config with no logger is exactly the case where
// there is nothing to report through, and answering it with silence would make
// the one error that explains every other error the only one nobody sees.
//
// So it is scoped to that: Main's four error paths, none of which is reached by
// a config that stated one, because Run and Once use cfg.Logger.
func reporter(c Config) *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

// taskName picks the task the arguments ask for, if they ask for one.
//
// A leading argument that is not a flag is a command. Refusing an unknown one
// rather than starting the server anyway is the whole value of this: a typo in
// a deployment script should be a process that exits, not one that quietly
// serves without having migrated.
func taskName(args []string, tasks map[string]Task) (string, error) {
	if len(tasks) == 0 || len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", nil
	}

	name := args[0]
	if _, found := tasks[name]; found {
		return name, nil
	}

	known := make([]string, 0, len(tasks))
	for t := range tasks {
		known = append(known, t)
	}
	sort.Strings(known)

	return "", fmt.Errorf("no command %q: this binary knows %s", name, strings.Join(known, ", "))
}

// Once opens the pool, runs f, and closes it again.
//
// For the work that is not a server: applying migrations, a backfill, a job
// that runs and exits. It reads the same configuration a server would, so the
// task and the server it ships with connect the same way, to the same place,
// with the same tuning.
func Once(ctx context.Context, cfg Config, task Task) error {
	if task == nil {
		return errors.New("serve: no task to run")
	}

	cfg = cfg.fromEnvironment()
	if err := cfg.checkStated(false); err != nil {
		return err
	}
	cfg = cfg.normalized()
	if err := cfg.checkStartup(); err != nil {
		return err
	}

	// Connecting is bounded; the work is not. A migration that takes ten
	// minutes is a migration, not a hang.
	connecting, done := context.WithTimeout(ctx, cfg.MaxStartup)
	defer done()

	pool, err := open(connecting, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	done()

	return task(ctx, pool)
}

// Run opens the pool, serves until the context is cancelled, and shuts down.
//
// Cancelling the context is the ordinary way to stop: the stop sequence gets
// MaxShutdown to finish, and only then does the pool close. Run returns nil
// when that completes, and an error when it does not.
func Run(ctx context.Context, cfg Config, mount Mount) (err error) {
	cfg = cfg.fromEnvironment()

	if err := cfg.checkStated(true); err != nil {
		return err
	}
	cfg = cfg.normalized()
	if err := cfg.checkStartup(); err != nil {
		return err
	}

	// The monitoring listener, first and stopped last.
	//
	// First, because everything below this can fail slowly — a database that
	// will not answer, a migration that will not finish — and those are the
	// startups somebody most wants to look at the log page during. It needs
	// nothing from the pool: the page reads files.
	//
	// Stopped last, which is what registering the defer here rather than after
	// the pool buys. Deferred teardown runs in reverse, so this closes after
	// the API has drained, after the App.Close hooks and after the pool — and a
	// drain that can be watched while it happens is most of the reason the page
	// is on a listener of its own.
	//
	// A port already taken is an error from Run for the reason the API's is: a
	// page somebody believes is running is worse than one that refused to
	// start.
	stopMonitor, err := cfg.serveMonitor(ctx)
	if err != nil {
		return err
	}
	defer stopMonitor()

	// Everything up to the first accepted connection happens under one budget.
	// It is released before serving: a deadline that outlived the boot would
	// shut the server down the moment it passed.
	starting, done := context.WithTimeout(ctx, cfg.MaxStartup)
	defer done()

	began := time.Now()

	pool, err := open(starting, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	if cfg.Migrate != nil {
		cfg.Logger.InfoContext(starting, "migrating")
		if err := bounded(starting, "migrate", func(c context.Context) error {
			return cfg.Migrate(c, pool)
		}); err != nil {
			return err
		}
	}

	app := &App{Pool: pool, Logger: cfg.Logger}
	// Before the mount function, because a step's limit is decided where it is
	// registered and everything that registers one runs inside it.
	app.limit(cfg.Shutdown)

	// Whatever the mount function built is taken apart, whether or not the
	// server ever started. Failing halfway through startup is when a half-built
	// process most needs it.
	defer func() {
		if failed := app.runClose(ctx, cfg.MaxShutdown); failed != nil {
			err = errors.Join(err, &ShutdownError{Err: failed})
		}
	}()

	var handler http.Handler
	if err := bounded(starting, "build the routes", func(c context.Context) error {
		built, err := mount(c, app)
		handler = built
		return err
	}); err != nil {
		return err
	}
	if handler == nil {
		return errors.New("serve: the mount function returned no handler")
	}
	// Readiness is false until the server is listening and false again the
	// moment it starts to stop, which is what a load balancer watches.
	var ready atomic.Bool
	handler = withProbes(cfg, app, pool, &ready, handler)

	// The shutdown is checked now, while the answer can still change anything:
	// a step that cannot fit in the budget is a truncated flush during an
	// actual shutdown, which is the worst time to learn about it.
	if err := app.checkShutdown(ctx, cfg.MaxShutdown, cfg.DrainDelay); err != nil {
		return err
	}

	// Listening happens before serving so that a port already in use is an
	// error from Run rather than a line in a log nobody is reading yet.
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Addr, err)
	}
	if cfg.OnListen != nil {
		cfg.OnListen(ln.Addr())
	}

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}

	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	// The boot is over, so the budget for it goes away before it can cut short
	// anything the server is serving.
	done()

	ready.Store(true)
	cfg.Logger.InfoContext(ctx, "listening", "addr", ln.Addr().String(), "started in", time.Since(began))

	select {
	case err := <-served:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	// The stop sequence outlives the context that triggered it. Deriving from a
	// cancelled context would give requests in flight no time at all, which is
	// the opposite of what a graceful shutdown is for.
	app.until = time.Now().Add(cfg.MaxShutdown)
	stopping, cancel := context.WithDeadline(context.WithoutCancel(ctx), app.until)
	defer cancel()

	// Announced before anything stops, so that whatever is routing traffic has
	// a chance to look away first.
	ready.Store(false)
	cfg.Logger.InfoContext(stopping, "draining", "delay", cfg.DrainDelay, "timeout", cfg.MaxShutdown)

	// Then the things that fetch their own work, which should stop fetching
	// while the server is still finishing what it has.
	drained := app.runDrain(stopping)

	if cfg.DrainDelay > 0 {
		timer := time.NewTimer(cfg.DrainDelay)
		defer timer.Stop()

		select {
		case <-timer.C:
		case <-stopping.Done():
			// The budget ran out during the delay. Stopping now beats being
			// killed halfway through the shutdown.
		}
	}

	// The requests in flight get what is left once the teardown has been set
	// aside, rather than the whole budget.
	//
	// checkShutdown has already said this is what the leftover is for. Without a
	// deadline of its own here it was only a description: one handler that will
	// not return — a long poll, a client that stopped reading — spends the whole
	// of MaxShutdown, and then every step registered with Close runs against a
	// deadline that has already passed and reports having given up waiting. The
	// worst of those is a write. So the server is cut short instead, which loses
	// the responses that were never going to arrive rather than the work that
	// was.
	serving := serveUntil(app)
	inflight, stopServing := context.WithDeadline(stopping, serving)
	defer stopServing()

	// Measured rather than derived from MaxShutdown, because the drain steps and
	// the delay have already been spent out of the same budget: the difference
	// between the two numbers is what this shutdown actually did, and naming a
	// window nobody had is the wrong thing to hand somebody reading the line.
	left := time.Until(serving).Round(time.Millisecond)

	if err := srv.Shutdown(inflight); err != nil {
		return &ShutdownError{Err: errors.Join(drained, fmt.Errorf(
			"the server would not stop within the %s left for requests in flight: %w",
			left, err))}
	}
	// Everything registered with App.Close runs from the deferred teardown,
	// after this, and before the pool closes.
	if drained != nil {
		return &ShutdownError{Err: drained}
	}
	return nil
}

// monitorGrace is how long the monitoring listener gets to finish what it has.
//
// It is a constant and not a budget the caller divides up, because the page
// serves six routes that read a bounded slice of a file and answer: there is no
// request here that legitimately takes longer, and nothing downstream is
// waiting on it either. It is deliberately outside MaxShutdown, which
// checkShutdown polices — that budget is for work the application declared, and
// this listener is stopped after all of it precisely so that the drain can be
// watched while it happens.
const monitorGrace = 2 * time.Second

// serveUntil is when the server has to stop answering so that the teardown
// still fits.
//
// Never in the past: a budget the steps have entirely spoken for is a warning
// checkShutdown has already given, not a reason to shut down a server that is
// still holding requests without letting http.Server look at them at all.
// Shutdown with a deadline of now still closes the listeners and still returns
// immediately for a server with nothing in flight.
func serveUntil(app *App) time.Time {
	deadline := app.until.Add(-app.reserved())
	if now := time.Now(); deadline.Before(now) {
		return now
	}
	return deadline
}

// serveMonitor opens the second listener and returns the function that closes
// it, which does nothing when there is no second listener.
//
// The two fields are refused separately from the rest of the configuration
// because the failure they guard against is silent: a Monitor with no
// MonitorAddr is a page nothing serves, and a MonitorAddr with no Monitor is a
// port that answers 404 to the person who went looking for the page. Either one
// looks wired from the main that wrote it.
func (c Config) serveMonitor(ctx context.Context) (func(), error) {
	switch {
	case c.Monitor == nil && c.MonitorAddr == "":
		return func() {}, nil
	case c.Monitor == nil:
		return nil, fmt.Errorf("serve: MonitorAddr is %q and Monitor is nil: "+
			"pass the handler too, or leave both empty", c.MonitorAddr)
	case c.MonitorAddr == "":
		return nil, errors.New("serve: Monitor is set and MonitorAddr is empty: " +
			"say where it listens, for example MonitorAddr: \"127.0.0.1:9090\"")
	}

	ln, err := net.Listen("tcp", c.MonitorAddr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s for the monitoring page: %w", c.MonitorAddr, err)
	}
	if c.OnMonitorListen != nil {
		c.OnMonitorListen(ln.Addr())
	}

	// A server of its own, so that draining the API leaves this one answering —
	// but the application's own timeouts on it, rather than a second set only
	// this listener obeys. They are what this process was told to hold a
	// connection open for, and every request here is lighter than the ones they
	// were sized for: a bounded slice of a file, or one of three assets.
	srv := &http.Server{
		Handler:           c.Monitor,
		ReadHeaderTimeout: c.ReadHeaderTimeout,
		ReadTimeout:       c.ReadTimeout,
		WriteTimeout:      c.WriteTimeout,
		IdleTimeout:       c.IdleTimeout,
	}
	// WithoutCancel because both of the things that log through it happen after
	// the context that started the stop is already cancelled: a listener that
	// died on its own, and the shutdown below.
	stopping := context.WithoutCancel(ctx)

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			c.Logger.ErrorContext(stopping, "the monitoring listener stopped",
				"addr", ln.Addr().String(), "error", err)
		}
	}()
	c.Logger.InfoContext(ctx, "monitoring", "addr", ln.Addr().String())

	return func() {
		// A deadline off the cancelled context would give this no time at all,
		// which is the same reason Run derives its own shutdown from one.
		ctx, cancel := context.WithTimeout(stopping, monitorGrace)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			c.Logger.ErrorContext(stopping, "the monitoring listener would not stop", "error", err)
		}
	}, nil
}

// checkStartup refuses a budget that cannot hold its own parts.
func (c Config) checkStartup() error {
	if c.ConnectTimeout > c.MaxStartup {
		return fmt.Errorf(
			"serve: ConnectTimeout %s is longer than MaxStartup %s: connecting alone "+
				"would use the whole budget", c.ConnectTimeout, c.MaxStartup)
	}
	return nil
}

// bounded runs one phase of the boot and stops waiting when the budget is gone.
//
// Waiting is bounded whether or not the phase is. A mount function that dials
// something slow and ignores the context it was handed would otherwise leave a
// process that is neither serving nor failing, which is the state nothing
// alerts on and everybody has to go and look at.
func bounded(ctx context.Context, what string, f func(context.Context) error) error {
	finished := make(chan error, 1)
	go func() { finished <- f(ctx) }()

	select {
	case err := <-finished:
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%s did not finish before MaxStartup: %w", what, ctx.Err())
	}
}

// open connects and proves it.
//
// The ping is the point: a pool hands out connections lazily, so without it a
// wrong host or a wrong password is a 500 on the first request rather than a
// refusal to start.
func open(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	if cfg.DatabaseURL == "" {
		return nil, errors.New("serve: no database url: set Config.DatabaseURL or $DATABASE_URL")
	}

	pc, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse the database url: %w", err)
	}
	if cfg.Pool != nil {
		if err := cfg.Pool(pc); err != nil {
			return nil, fmt.Errorf("configure the pool: %w", err)
		}
	}

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("open the pool: %w", err)
	}

	connecting, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()

	if err := ping(connecting, cfg, pool, address(pc)); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// ping proves the connection, and says something before the whole budget is
// gone.
//
// Two attempts asking the same question: a short one, then the rest of the
// wait. The short one is there for the case that is not a slow database but an
// absent one — nobody started it, or the url points somewhere there is nothing.
// That is worth hearing in a second rather than in ten, and it is the commonest
// thing to go wrong the first time somebody runs a project. A database that is
// merely slow to accept connections — a container coming up, a failover — still
// gets the full ConnectTimeout, because that wait is the reason it exists.
func ping(ctx context.Context, cfg Config, pool *pgxpool.Pool, addr string) error {
	first, cancel := context.WithTimeout(ctx, min(time.Second, cfg.ConnectTimeout))
	defer cancel()

	if err := pool.Ping(first); err == nil {
		return nil
	}

	// The address, never the url: a connection string carries a password.
	if cfg.Hint != "" {
		cfg.Logger.WarnContext(ctx, "cannot reach the database yet", "addr", addr,
			"waiting", cfg.ConnectTimeout, "hint", cfg.Hint)
	} else {
		cfg.Logger.WarnContext(ctx, "cannot reach the database yet", "addr", addr,
			"waiting", cfg.ConnectTimeout)
	}

	if err := pool.Ping(ctx); err != nil {
		if cfg.Hint != "" {
			return fmt.Errorf("connect to the database at %s: %w (%s)", addr, err, cfg.Hint)
		}
		return fmt.Errorf("connect to the database at %s: %w", addr, err)
	}
	return nil
}

// address is where the pool is pointed, without the credentials that came with
// it.
func address(pc *pgxpool.Config) string {
	c := pc.ConnConfig
	if c == nil || c.Host == "" {
		return "the configured host"
	}
	return net.JoinHostPort(c.Host, strconv.Itoa(int(c.Port)))
}

// withProbes answers the two probe paths itself and passes everything else
// through.
//
// They are deliberately different questions. Liveness asks whether to restart
// this process; readiness asks whether to send it work. Answering both from one
// check means either a wedged process nobody restarts, or a whole fleet
// restarted because the database was slow for thirty seconds.
func withProbes(cfg Config, app *App, pool *pgxpool.Pool, ready *atomic.Bool, next http.Handler) http.Handler {
	if cfg.LivenessPath == NoProbe && cfg.ReadinessPath == NoProbe {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case cfg.LivenessPath:
			// No dependency is consulted. Answering at all is the answer.
			writeProbe(w, http.StatusOK, "alive")

		case cfg.ReadinessPath:
			if err := readiness(r.Context(), cfg, app, pool, ready); err != nil {
				writeProbe(w, http.StatusServiceUnavailable, err.Error())
				return
			}
			writeProbe(w, http.StatusOK, readyBody(app))

		default:
			next.ServeHTTP(w, r)
		}
	})
}

// readiness is everything that has to be true for this instance to be worth
// sending a request to.
func readiness(ctx context.Context, cfg Config, app *App, pool *pgxpool.Pool, ready *atomic.Bool) error {
	if !ready.Load() {
		return errors.New("shutting down")
	}

	ctx, cancel := context.WithTimeout(ctx, cfg.ProbeTimeout)
	defer cancel()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("database: %w", err)
	}
	if cfg.Ready != nil {
		if err := cfg.Ready(ctx); err != nil {
			return err
		}
	}
	return runChecks(ctx, app)
}

// runChecks asks every dependency the mount function registered, and names the
// one that said no.
//
// They share the single budget readiness already took, rather than each getting
// one: three dependencies with two seconds apiece is a readiness check that
// takes six, which is a probe that has already failed by the time it answers.
func runChecks(ctx context.Context, app *App) error {
	for _, c := range app.readyChecks() {
		if err := c.probe(ctx); err != nil {
			return fmt.Errorf("%s: %w", c.name, err)
		}
	}
	return nil
}

// readyBody is what a passing readiness check says: what was checked, rather
// than only that something was.
//
// The endpoint is read by whoever is debugging a deployment, and "ready" alone
// leaves them with no way to tell an instance that checked its dependencies from
// one that has none registered — which is exactly the difference they came to
// find out.
func readyBody(app *App) string {
	checks := app.readyChecks()
	names := make([]string, 0, len(checks)+1)
	names = append(names, "database")
	for _, c := range checks {
		names = append(names, c.name)
	}
	return "ready\n" + strings.Join(names, "\n")
}

func writeProbe(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(message + "\n"))
}

// fromEnvironment reads the two values a deployment supplies rather than states.
//
// The whole of what this used to be. Everything else it filled in was a value
// rig invented for a config that said nothing — a timeout nobody chose, a port
// nobody picked — and a default is only ever discovered by whatever it costs
// when it is wrong. What is left here is not that: $DATABASE_URL and $ADDR are
// how the thing running this binary hands it a value, so reading them is the
// config being supplied rather than substituted for.
//
// [Config.checkStated] refuses what is still empty afterwards.
func (c Config) fromEnvironment() Config {
	if c.DatabaseURL == "" {
		c.DatabaseURL = os.Getenv("DATABASE_URL")
	}
	if c.Addr == "" {
		c.Addr = os.Getenv("ADDR")
	}
	return c
}

// checkStated refuses a config that left a value for this package to invent.
//
// All of them at once, rather than the first one found. Filling these in is one
// sitting, and answering it one field per run of the binary would be six runs to
// learn six numbers.
//
// Only the fields where saying nothing would mean this package choosing. A nil
// Ready, Pool, OnListen or Monitor means there is nothing to do rather than
// something to pick, and there is no way to state that more plainly than by
// leaving it out — requiring `Ready: nil` would be noise that reads like a
// decision. What is here is every value that would otherwise be one.
//
// A negative duration is a stated answer, not a missing one: it is how a caller
// says a timeout should not apply at all, which the standard library reads as
// none. Zero is the value nobody wrote.
func (c Config) checkStated(serving bool) error {
	var missing []string

	if c.DatabaseURL == "" {
		missing = append(missing, "DatabaseURL (or $DATABASE_URL)")
	}
	if c.Logger == nil {
		missing = append(missing, "Logger")
	}
	if c.MaxStartup == 0 {
		missing = append(missing, "MaxStartup")
	}
	if c.ConnectTimeout == 0 {
		missing = append(missing, "ConnectTimeout")
	}

	// The rest are about answering requests, and [Once] answers none. A
	// subcommand that applies a migration and exits opens the same pool and is
	// not a server, so asking it for a write timeout and a liveness path would
	// be seven values invented for a listener that never starts.
	if serving {
		if c.Addr == "" {
			missing = append(missing, "Addr (or $ADDR)")
		}
		for _, f := range []struct {
			name string
			v    time.Duration
		}{
			{"ProbeTimeout", c.ProbeTimeout},
			{"ReadHeaderTimeout", c.ReadHeaderTimeout},
			{"ReadTimeout", c.ReadTimeout},
			{"WriteTimeout", c.WriteTimeout},
			{"IdleTimeout", c.IdleTimeout},
		} {
			if f.v == 0 {
				missing = append(missing, f.name)
			}
		}
		if c.LivenessPath == "" {
			missing = append(missing, "LivenessPath (or serve.NoProbe)")
		}
		if c.ReadinessPath == "" {
			missing = append(missing, "ReadinessPath (or serve.NoProbe)")
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf(
			"serve: these have no value and this package will not invent one: %s. "+
				"State each in the serve.Config; a duration that should not apply at "+
				"all is a negative one, and a probe that should not be served is "+
				"serve.NoProbe",
			strings.Join(missing, ", "))
	}
	return nil
}

// normalized turns a stated "no timeout" into the zero the standard library
// reads as one.
//
// [Config.checkStated] needs zero and negative to mean different things — one is
// a field nobody filled in, the other is a caller saying this timeout should not
// apply — and net/http has only zero for the second. So they stay apart until
// the check has run, and are folded together here, after it and before anything
// uses them.
func (c Config) normalized() Config {
	for _, f := range []*time.Duration{
		&c.MaxStartup, &c.ConnectTimeout, &c.ProbeTimeout,
		&c.ReadHeaderTimeout, &c.ReadTimeout, &c.WriteTimeout, &c.IdleTimeout,
	} {
		if *f < 0 {
			*f = 0
		}
	}
	return c
}
