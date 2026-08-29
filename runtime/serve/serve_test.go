package serve

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The environment supplies two values. Nothing else is supplied at all.
func TestTheEnvironmentIsReadAndNothingElseIsInvented(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://from-the-environment")
	t.Setenv("ADDR", "127.0.0.1:9999")

	got := Config{}.fromEnvironment()

	if got.DatabaseURL != "postgres://from-the-environment" {
		t.Errorf("DatabaseURL = %q", got.DatabaseURL)
	}
	if got.Addr != "127.0.0.1:9999" {
		t.Errorf("Addr = %q", got.Addr)
	}

	// Everything else is still exactly what the caller wrote, which was
	// nothing. A value here would be one this package chose.
	for _, f := range []struct {
		name string
		v    time.Duration
	}{
		{"MaxStartup", got.MaxStartup},
		{"ConnectTimeout", got.ConnectTimeout},
		{"ProbeTimeout", got.ProbeTimeout},
		{"ReadHeaderTimeout", got.ReadHeaderTimeout},
		{"ReadTimeout", got.ReadTimeout},
		{"WriteTimeout", got.WriteTimeout},
		{"IdleTimeout", got.IdleTimeout},
		{"MaxShutdown", got.MaxShutdown},
	} {
		if f.v != 0 {
			t.Errorf("%s = %s, want it left alone", f.name, f.v)
		}
	}
	if got.Logger != nil {
		t.Error("Logger should be left alone")
	}
	if got.LivenessPath != "" || got.ReadinessPath != "" {
		t.Errorf("probe paths = %q, %q, want them left alone", got.LivenessPath, got.ReadinessPath)
	}
}

// And what is left empty is refused, all of it at once, before anything opens
// or listens. One field per run of the binary would be nine runs.
func TestAnUnstatedConfigIsRefusedWithEveryFieldNamed(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("ADDR", "")

	err := Config{}.checkStated(true)
	if err == nil {
		t.Fatal("a config that stated nothing was accepted")
	}
	for _, want := range []string{
		"DatabaseURL", "Logger", "MaxStartup", "ConnectTimeout", "ProbeTimeout",
		"ReadHeaderTimeout", "ReadTimeout", "WriteTimeout", "IdleTimeout",
		"Addr", "LivenessPath", "ReadinessPath", "serve.NoProbe",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should name %q: %v", want, err)
		}
	}
}

// A task is not a server. Asking a migration subcommand for a write timeout and
// a liveness path would be seven values invented for a listener never started.
func TestATaskIsNotAskedForWhatOnlyAServerNeeds(t *testing.T) {
	cfg := Config{
		DatabaseURL:    "postgres://somewhere",
		Logger:         quiet(),
		MaxStartup:     time.Minute,
		ConnectTimeout: time.Second,
	}

	if err := cfg.checkStated(false); err != nil {
		t.Errorf("a task has everything it needs: %v", err)
	}
	if err := cfg.checkStated(true); err == nil {
		t.Error("a server needs more than that")
	}
}

// What the caller wrote down wins over the environment. The environment is the
// fallback, not an override — a value in the code that a stray variable can
// change is a deployment nobody can reason about.
func TestWhatWasSetIsKept(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://from-the-environment")
	t.Setenv("ADDR", ":9999")

	got := Config{
		DatabaseURL: "postgres://from-the-code",
		Addr:        "127.0.0.1:1234",
		MaxShutdown: time.Second,
	}.fromEnvironment()

	if got.DatabaseURL != "postgres://from-the-code" {
		t.Errorf("DatabaseURL = %q", got.DatabaseURL)
	}
	if got.Addr != "127.0.0.1:1234" {
		t.Errorf("Addr = %q", got.Addr)
	}
	if got.MaxShutdown != time.Second {
		t.Errorf("MaxShutdown = %s", got.MaxShutdown)
	}
}

// Zero means "I did not say", which is why it cannot also mean "no limit".
// Negative is how a caller says that, and the two stay apart until the refusal
// has run: net/http has only zero for the second, so they are folded together
// afterwards.
func TestANegativeTimeoutMeansNone(t *testing.T) {
	stated := Config{ReadTimeout: -1, WriteTimeout: time.Second}

	// Still told apart where it matters: a negative is an answer, so it is not
	// in the list of things nobody stated.
	if err := stated.checkStated(true); err == nil || strings.Contains(err.Error(), "ReadTimeout") {
		t.Errorf("a negative ReadTimeout is a stated one: %v", err)
	}

	got := stated.normalized()
	if got.ReadTimeout != 0 {
		t.Errorf("ReadTimeout = %s, want none", got.ReadTimeout)
	}
	if got.WriteTimeout != time.Second {
		t.Errorf("WriteTimeout = %s, want it untouched", got.WriteTimeout)
	}
}

func TestRunRefusesWithoutADatabase(t *testing.T) {
	t.Setenv("DATABASE_URL", "")

	err := Run(t.Context(), Config{}, nil)
	if err == nil {
		t.Fatal("a server with nothing to connect to should refuse to start")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("the error should say what to set: %v", err)
	}
}

// Liveness and readiness answer different questions, and the difference is the
// whole reason there are two. A liveness probe that consulted the database
// would turn one slow database into every replica being restarted at once —
// which is not a recovery, it is the outage.
func TestLivenessDoesNotDependOnAnything(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)

	cfg := Config{LivenessPath: "/livez", ReadinessPath: "/readyz"}

	// A nil pool is the strongest form of "no database": readiness would panic
	// on it, so a liveness check that answers proves it never looked.
	h := withProbes(cfg, nil, &ready, http.NotFoundHandler())

	res := probe(t, h, "/livez")
	if res.Code != http.StatusOK {
		t.Errorf("liveness = %d, want 200", res.Code)
	}
}

// Readiness turns false the moment a shutdown begins, before anything stops
// accepting connections. That gap is what a load balancer needs to look away.
func TestReadinessIsFalseWhileShuttingDown(t *testing.T) {
	var ready atomic.Bool // zero value: not ready yet

	cfg := Config{LivenessPath: "/livez", ReadinessPath: "/readyz"}
	h := withProbes(cfg, nil, &ready, http.NotFoundHandler())

	res := probe(t, h, "/readyz")
	if res.Code != http.StatusServiceUnavailable {
		t.Errorf("readiness = %d, want 503", res.Code)
	}
	if body := res.Body.String(); !strings.Contains(body, "shutting down") {
		t.Errorf("the reason should say why: %q", body)
	}

	// Liveness is unaffected: the process is fine, it is just leaving.
	if got := probe(t, h, "/livez").Code; got != http.StatusOK {
		t.Errorf("liveness = %d during shutdown, want 200", got)
	}
}

func TestEverythingElsePassesThrough(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)

	cfg := Config{LivenessPath: "/livez", ReadinessPath: NoProbe}
	h := withProbes(cfg, nil, &ready, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	if got := probe(t, h, "/api/v1/todos").Code; got != http.StatusTeapot {
		t.Errorf("status = %d: the application should have answered", got)
	}
}

// With both paths declined the wrapper is not there at all, so a project that
// wants to answer /livez itself can. Declined rather than omitted: an empty path
// is a config that said nothing, and checkStated refuses that.
func TestNoProbesNoWrapper(t *testing.T) {
	var ready atomic.Bool
	inner := http.NotFoundHandler()
	cfg := Config{LivenessPath: NoProbe, ReadinessPath: NoProbe}

	if got := withProbes(cfg, nil, &ready, inner); got == nil {
		t.Fatal("the handler should still be there")
	}
}

// Half a pair is refused, because the failure it guards against is silent: a
// handler with nowhere to listen is a page nothing serves, and an address with
// no handler is a port answering 404 to the person who went looking. Either one
// reads as wired from the main that wrote it.
func TestTheMonitorNeedsBothHalves(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"a handler and nowhere to put it", Config{Logger: quiet(), Monitor: http.NotFoundHandler()}},
		{"an address and nothing to serve", Config{Logger: quiet(), MonitorAddr: "127.0.0.1:0"}},
	} {
		if _, err := tc.cfg.serveMonitor(t.Context()); err == nil {
			t.Errorf("%s was accepted", tc.name)
		}
	}
}

// Neither half is no second listener, which is what a deployment with no
// monitoring password gets: observe hands back a nil handler and an empty
// address together, and this is where that turns into nothing being opened.
func TestNoMonitorNoListener(t *testing.T) {
	stop, err := Config{Logger: quiet()}.serveMonitor(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	stop()
}

// The listener is its own, and what is on it is only the monitor: nothing the
// application mounted answers there, which is the whole of what binding it
// somewhere else is worth.
func TestTheMonitorIsOnItsOwnListener(t *testing.T) {
	var bound net.Addr
	cfg := Config{
		Logger: quiet(),
		Monitor: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		}),
		MonitorAddr:     "127.0.0.1:0",
		OnMonitorListen: func(a net.Addr) { bound = a },
	}

	stop, err := cfg.serveMonitor(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	if bound == nil {
		t.Fatal("the callback never said which port was bound")
	}
	res, err := http.Get("http://" + bound.String() + "/anything")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusTeapot {
		t.Errorf("the monitor listener answered %d, want the handler's 418", res.StatusCode)
	}
}

// Stopping it closes the port. It is deferred from Run before the pool is
// opened, so this is the last thing in the process to go — a drain can be
// watched while it happens.
func TestStoppingTheMonitorClosesThePort(t *testing.T) {
	var bound net.Addr
	cfg := Config{
		Logger:          quiet(),
		Monitor:         http.NotFoundHandler(),
		MonitorAddr:     "127.0.0.1:0",
		OnMonitorListen: func(a net.Addr) { bound = a },
	}

	stop, err := cfg.serveMonitor(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	stop()

	if _, err := http.Get("http://" + bound.String() + "/"); err == nil {
		t.Error("the port still answered after the listener was stopped")
	}
}

// A port already taken is an error from the call rather than a line in a log,
// for the reason the API's listener is: a monitoring page somebody believes is
// running is worse than one that refused to start.
func TestAMonitorPortAlreadyTakenIsAnError(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	cfg := Config{Logger: quiet(), Monitor: http.NotFoundHandler(), MonitorAddr: taken.Addr().String()}
	if stop, err := cfg.serveMonitor(t.Context()); err == nil {
		stop()
		t.Error("a port already in use was accepted")
	}
}

func probe(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// A leading argument that names a task is a command; a leading flag is not.
func TestTaskDispatch(t *testing.T) {
	tasks := map[string]Task{
		"migrate": func(context.Context, *pgxpool.Pool) error { return nil },
		"seed":    func(context.Context, *pgxpool.Pool) error { return nil },
	}

	for _, tc := range []struct {
		args    []string
		want    string
		wantErr bool
	}{
		{args: nil, want: ""},
		{args: []string{"migrate"}, want: "migrate"},
		{args: []string{"seed", "--force"}, want: "seed"},
		{args: []string{"-addr", ":9000"}, want: ""},
		{args: []string{"--addr=:9000"}, want: ""},
		{args: []string{"migrat"}, wantErr: true},
	} {
		got, err := taskName(tc.args, tasks)

		switch {
		case tc.wantErr && err == nil:
			t.Errorf("%v: a command nobody defined should not start the server instead", tc.args)
		case !tc.wantErr && err != nil:
			t.Errorf("%v: %v", tc.args, err)
		case got != tc.want:
			t.Errorf("%v: task = %q, want %q", tc.args, got, tc.want)
		}
	}

	// The refusal has to say what the binary does know, or the next thing the
	// operator tries is another guess.
	_, err := taskName([]string{"migrat"}, tasks)
	if err == nil || !strings.Contains(err.Error(), "migrate, seed") {
		t.Errorf("the error should list the commands: %v", err)
	}
}

// With no tasks configured, arguments are somebody else's business.
func TestNoTasksMeansNoDispatch(t *testing.T) {
	got, err := taskName([]string{"anything"}, nil)
	if err != nil || got != "" {
		t.Errorf("task = %q, err = %v", got, err)
	}
}

func TestOnceNeedsSomethingToDo(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://nowhere")

	if err := Once(t.Context(), Config{}, nil); err == nil {
		t.Fatal("a task that is not there should be an error, not a silent success")
	}
}

// The way in is bounded like the way out. A phase that ignores its context
// would otherwise leave a process neither serving nor failing, which is the
// state nothing alerts on.
func TestBoundedGivesUpOnAPhaseThatWillNot(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := bounded(ctx, "build the routes", func(context.Context) error {
		<-release
		return nil
	})

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a deadline", err)
	}
	// The message has to say which phase, or the operator is left guessing
	// between the database, the migration and the routes.
	if !strings.Contains(err.Error(), "build the routes") {
		t.Errorf("the error should name the phase: %v", err)
	}
	if !strings.Contains(err.Error(), "MaxStartup") {
		t.Errorf("the error should name the budget: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %s on a phase that never returns", elapsed)
	}
}

func TestBoundedPassesTheFailureThrough(t *testing.T) {
	broken := errors.New("no route for you")

	err := bounded(t.Context(), "build the routes", func(context.Context) error { return broken })

	if !errors.Is(err, broken) {
		t.Errorf("err = %v, want the failure itself", err)
	}
	if !strings.Contains(err.Error(), "build the routes") {
		t.Errorf("the error should still name the phase: %v", err)
	}
}

// Connecting is part of starting, so a connection budget larger than the whole
// is a configuration that cannot mean what it says.
func TestConnectTimeoutMustFitInsideMaxStartup(t *testing.T) {
	err := Config{MaxStartup: 5 * time.Second, ConnectTimeout: 30 * time.Second}.checkStartup()

	if err == nil {
		t.Fatal("a connection budget larger than the startup budget should be refused")
	}
	for _, want := range []string{"ConnectTimeout", "MaxStartup", "30s", "5s"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should mention %q: %v", want, err)
		}
	}

	// A connection budget that fits is not refused. It used to be that a short
	// MaxStartup silently pulled ConnectTimeout down with it; both are stated
	// now, so the only thing left to check is that they agree.
	fits := Config{MaxStartup: 200 * time.Millisecond, ConnectTimeout: 200 * time.Millisecond}
	if err := fits.checkStartup(); err != nil {
		t.Errorf("the adjusted default should be accepted: %v", err)
	}
}

// A database that is not there is the commonest thing to go wrong the first time
// somebody runs a project, and the connection error alone does not say what to
// do about it. The hint does, and it says it twice: once as soon as the first
// attempt fails, and once in the error if the wait runs out.
func TestTheHintIsSaidEarlyAndAgainInTheError(t *testing.T) {
	var log strings.Builder

	cfg := Config{
		// Port 1 is nobody, so this refuses immediately rather than hanging.
		DatabaseURL:    "postgres://rig:secret@127.0.0.1:1/rig?sslmode=disable",
		ConnectTimeout: 300 * time.Millisecond,
		MaxStartup:     time.Second,
		Hint:           "run `rig db up` to start a local Postgres",
		Logger:         slog.New(slog.NewTextHandler(&log, nil)),
	}

	_, err := open(context.Background(), cfg)
	if err == nil {
		t.Fatal("connecting to nothing should fail")
	}

	if !strings.Contains(err.Error(), cfg.Hint) {
		t.Errorf("the error should carry the hint: %v", err)
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("the error should say where it tried: %v", err)
	}

	said := log.String()
	if !strings.Contains(said, "cannot reach the database yet") {
		t.Errorf("it should say something before the wait is over:\n%s", said)
	}
	if !strings.Contains(said, cfg.Hint) {
		t.Errorf("the early word should carry the hint:\n%s", said)
	}

	// A connection string carries a password, so neither the log nor the error
	// is allowed to hold one.
	for _, where := range []string{said, err.Error()} {
		if strings.Contains(where, "secret") {
			t.Errorf("the credentials leaked:\n%s", where)
		}
	}
}

// Without a hint there is nothing to add, and the error still says where it
// tried — which is the part no caller can be expected to guess.
func TestNoHintNoInvention(t *testing.T) {
	cfg := Config{
		DatabaseURL:    "postgres://rig:secret@127.0.0.1:1/rig?sslmode=disable",
		ConnectTimeout: 300 * time.Millisecond,
		MaxStartup:     time.Second,
		Logger:         slog.New(slog.DiscardHandler),
	}

	_, err := open(context.Background(), cfg)
	if err == nil {
		t.Fatal("connecting to nothing should fail")
	}
	if !strings.Contains(err.Error(), "connect to the database at 127.0.0.1:1") {
		t.Errorf("unexpected error: %v", err)
	}
	// The hint is appended in brackets at the end, so with no hint the message
	// ends where the driver's own does. (Brackets appear inside it too — the
	// driver names the address it resolved — so the end is what to look at.)
	if strings.HasSuffix(err.Error(), ")") {
		t.Errorf("nothing should be appended when there is no hint: %v", err)
	}
}
