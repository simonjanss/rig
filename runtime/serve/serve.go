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

// Config is everything the server needs that is not a route.
//
// Every field has a defensible default, and the two that vary per environment
// read from the environment when they are left empty.
type Config struct {
	// DatabaseURL is the connection string. Empty means $DATABASE_URL, and
	// failing that the server refuses to start rather than guessing.
	DatabaseURL string

	// Addr is the listen address. Empty means $ADDR, then ":8080".
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
	// before the shutdown starts. Default none. It comes out of MaxShutdown.
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
	// mount function. Default 60s.
	//
	// The counterpart of MaxShutdown, and for the same reason. A boot that
	// hangs on something slow is not obviously different from a boot that is
	// merely slow, and the difference matters most at three in the morning. A
	// project that migrates on the way up should raise this: the migration is
	// inside the budget.
	MaxStartup time.Duration

	// ConnectTimeout bounds the first connection, inside MaxStartup. Default
	// 10s, or MaxStartup when that is shorter — soon enough to say "the
	// database" rather than "startup" when that is what went wrong.
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

	// ProbeTimeout bounds what a readiness check is allowed to wait for.
	// Default 2s: a probe that hangs is a probe that has already failed.
	ProbeTimeout time.Duration

	// ReadHeaderTimeout bounds how long a client may take to send its headers.
	// Default 5s, and never zero: without it one open connection sending
	// nothing occupies a goroutine forever.
	ReadHeaderTimeout time.Duration
	// ReadTimeout bounds the whole request. Default 30s.
	ReadTimeout time.Duration
	// WriteTimeout bounds the response. Default 30s.
	WriteTimeout time.Duration
	// IdleTimeout bounds a kept-alive connection between requests. Default 2m.
	IdleTimeout time.Duration

	// MaxShutdown is the longest the whole stop sequence may take, from the
	// signal to the process being gone: the drain delay, the drain steps, the
	// requests in flight, and every dependency registered with App.Close.
	// Default 20s.
	//
	// It is a stated maximum rather than the sum of its parts so that it can be
	// read off and copied — this is the number that belongs in Kubernetes'
	// terminationGracePeriodSeconds, and adding up whatever the mount function
	// happened to register is not something anybody will do twice. The parts
	// are checked against it before the server listens, so the two cannot drift
	// apart quietly.
	MaxShutdown time.Duration

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
		logger(cfg).ErrorContext(ctx, "cannot run that", "error", err)
		exit(2)
	}

	if name != "" {
		if err := Once(ctx, cfg, cfg.Tasks[name]); err != nil {
			logger(cfg).ErrorContext(ctx, name+" failed", "error", err)
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
			logger(cfg).ErrorContext(ctx, "stopped, but not cleanly")
			done()
			return
		}

		logger(cfg).ErrorContext(ctx, "server stopped", "error", err)
		exit(1)
	}
	done()
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

	cfg = cfg.withDefaults()
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
	cfg = cfg.withDefaults()

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
	handler = withProbes(cfg, pool, &ready, handler)

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
	inflight, stopServing := context.WithDeadline(stopping, serveUntil(app))
	defer stopServing()

	if err := srv.Shutdown(inflight); err != nil {
		return &ShutdownError{Err: errors.Join(drained, fmt.Errorf(
			"the server would not stop within the %s left for requests in flight: %w",
			cfg.MaxShutdown-app.reserved(), err))}
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
func withProbes(cfg Config, pool *pgxpool.Pool, ready *atomic.Bool, next http.Handler) http.Handler {
	if cfg.LivenessPath == "" && cfg.ReadinessPath == "" {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case cfg.LivenessPath:
			// No dependency is consulted. Answering at all is the answer.
			writeProbe(w, http.StatusOK, "alive")

		case cfg.ReadinessPath:
			if err := readiness(r.Context(), cfg, pool, ready); err != nil {
				writeProbe(w, http.StatusServiceUnavailable, err.Error())
				return
			}
			writeProbe(w, http.StatusOK, "ready")

		default:
			next.ServeHTTP(w, r)
		}
	})
}

// readiness is everything that has to be true for this instance to be worth
// sending a request to.
func readiness(ctx context.Context, cfg Config, pool *pgxpool.Pool, ready *atomic.Bool) error {
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
	return nil
}

func writeProbe(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(message + "\n"))
}

// withDefaults fills in everything that was left out.
func (c Config) withDefaults() Config {
	if c.DatabaseURL == "" {
		c.DatabaseURL = os.Getenv("DATABASE_URL")
	}
	if c.Addr == "" {
		c.Addr = os.Getenv("ADDR")
	}
	if c.Addr == "" {
		c.Addr = ":8080"
	}

	c.Logger = logger(c)

	c.MaxStartup = orDefault(c.MaxStartup, 60*time.Second)

	// A default that does not fit inside a stated budget yields to it. Only a
	// connection budget somebody actually wrote is worth refusing over, and
	// making every short MaxStartup require a second field would be a footnote
	// nobody reads until it bites them.
	c.ConnectTimeout = orDefault(c.ConnectTimeout, min(10*time.Second, c.MaxStartup))
	c.ProbeTimeout = orDefault(c.ProbeTimeout, 2*time.Second)
	c.ReadHeaderTimeout = orDefault(c.ReadHeaderTimeout, 5*time.Second)
	c.ReadTimeout = orDefault(c.ReadTimeout, 30*time.Second)
	c.WriteTimeout = orDefault(c.WriteTimeout, 30*time.Second)
	c.IdleTimeout = orDefault(c.IdleTimeout, 2*time.Minute)
	c.MaxShutdown = orDefault(c.MaxShutdown, 20*time.Second)

	return c
}

func logger(c Config) *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

// orDefault treats zero as "not set". A caller who really wants no timeout says
// so with a negative duration, which the standard library reads as none.
func orDefault(v, fallback time.Duration) time.Duration {
	switch {
	case v == 0:
		return fallback
	case v < 0:
		return 0
	default:
		return v
	}
}
