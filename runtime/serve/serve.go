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
}

// Main runs a server until it is asked to stop, then exits.
//
// It is Run with the signals wired up and a non-zero exit on failure — the
// whole of a main function, so that a main function can be the wiring and
// nothing else. Use Run directly to keep control of the process.
func Main(cfg Config, mount Mount) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	name, err := taskName(os.Args[1:], cfg.Tasks)
	if err != nil {
		logger(cfg).Error("cannot run that", "error", err)
		stop()
		os.Exit(2)
	}

	if name != "" {
		if err := Once(ctx, cfg, cfg.Tasks[name]); err != nil {
			logger(cfg).Error(name+" failed", "error", err)
			stop()
			os.Exit(1)
		}
		return
	}

	if err := Run(ctx, cfg, mount); err != nil {
		// A shutdown that went badly is not a server that failed. Every step
		// that failed has already said so, at the moment it failed, so there is
		// nothing to repeat here — and nothing worth telling the orchestrator
		// that the process crashed.
		if Unclean(err) {
			logger(cfg).Error("stopped, but not cleanly")
			return
		}

		logger(cfg).Error("server stopped", "error", err)
		stop()
		os.Exit(1)
	}
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
		cfg.Logger.Info("migrating")
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
	if err := app.checkShutdown(cfg.MaxShutdown, cfg.DrainDelay); err != nil {
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
	cfg.Logger.Info("listening", "addr", ln.Addr().String(), "started in", time.Since(began))

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
	cfg.Logger.Info("draining", "delay", cfg.DrainDelay, "timeout", cfg.MaxShutdown)

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

	if err := srv.Shutdown(stopping); err != nil {
		return &ShutdownError{Err: errors.Join(drained, fmt.Errorf("the server would not stop: %w", err))}
	}
	// Everything registered with App.Close runs from the deferred teardown,
	// after this, and before the pool closes.
	if drained != nil {
		return &ShutdownError{Err: drained}
	}
	return nil
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
		cfg.Logger.Warn("cannot reach the database yet", "addr", addr,
			"waiting", cfg.ConnectTimeout, "hint", cfg.Hint)
	} else {
		cfg.Logger.Warn("cannot reach the database yet", "addr", addr,
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
