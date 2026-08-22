package dockerdb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Config describes the throwaway database.
type Config struct {
	Image     string
	Name      string
	Port      int
	Database  string
	User      string
	Password  string
	Runtime   string // "" picks docker, then podman
	Log       io.Writer
	StartWait time.Duration

	// Settings are extra server parameters, without the -c. Live sync needs
	// wal_level=logical, which cannot be set after the server has started.
	Settings []string

	// Bind is the host address the port is published on. Empty means
	// 127.0.0.1, which is the right answer for a database nobody else should
	// reach. A test that puts another container in front of this one needs
	// 0.0.0.0 instead: on Linux, a sibling container arrives over the bridge,
	// and a loopback-only publish refuses it.
	Bind string
}

func (c Config) bind() string {
	if c.Bind != "" {
		return c.Bind
	}
	return "127.0.0.1"
}

// publish is the --publish argument: a fixed host port when there is one, and an
// empty one — the engine's spelling of "pick a free port" — when there is not.
func (c Config) publish() string {
	if c.Port == 0 {
		return fmt.Sprintf("%s::5432", c.bind())
	}
	return fmt.Sprintf("%s:%d:5432", c.bind(), c.Port)
}

// portDescription is how the port reads in a log line before anything has been
// published, when under isolation there is no number to print yet.
func (c Config) portDescription() string {
	if c.Port == 0 {
		return "whatever is free"
	}
	return strconv.Itoa(c.Port)
}

func (c Config) startWait() time.Duration {
	if c.StartWait > 0 {
		return c.StartWait
	}
	return 60 * time.Second
}

// URL is the connection string for the container.
//
// The port is the configured one, so this answers for a [Config] and not for a
// running container: under isolation the port is zero here and the kernel picks
// the real one. [DB.URL] is what reports that, and is what a caller that has
// started something should use.
//
// TimeZone is pinned rather than inherited. A timestamptz holds an instant and no
// zone, so nothing about what is *stored* depends on this — but `::date`,
// `date_trunc`, and an input string with no offset are all read in the session's
// zone, so a daily aggregate silently buckets differently on a machine set to
// Stockholm than on one set to UTC. Introspection and every generated repository
// therefore agree on one answer, and it is the one that does not move twice a
// year. pgx passes an unrecognised parameter through as a startup setting, so
// this is all it takes.
func (c Config) URL() string {
	return fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/%s?sslmode=disable&TimeZone=UTC",
		c.User, c.Password, c.Port, c.Database)
}

// DB is a running throwaway database.
type DB struct {
	cfg     Config
	runtime Runtime
	// port is what the container actually publishes on, which is the configured
	// port unless there was none to configure. Resolved before anything tries to
	// connect, because under isolation it is the only way to.
	port int
	// fresh records that Start created the container rather than reusing one.
	// The data is on a tmpfs, so fresh means empty — and it means anything that
	// followed this database, a sync service's replication slot most of all, is
	// now following nothing and has to be rebuilt too.
	fresh bool
}

// Fresh reports whether Start created this container rather than reusing one.
func (d *DB) Fresh() bool { return d.fresh }

// Start brings the database up and waits until it accepts connections.
//
// An existing container is reused when it is running the same image on the same
// port. That is the common case — the second and every later command in a
// session — and it is what keeps `rig generate` fast. A container that does not
// match is replaced rather than adapted: it holds no state worth keeping.
func Start(ctx context.Context, cfg Config) (*DB, error) {
	rt, err := FindRuntime(ctx, cfg.Runtime)
	if err != nil {
		return nil, err
	}
	db := &DB{cfg: cfg, runtime: rt}

	state, err := db.inspect(ctx)
	if err != nil {
		return nil, err
	}

	switch {
	case state == nil:
		if err := db.create(ctx); err != nil {
			return nil, err
		}
		db.fresh = true

	case !state.matches(cfg):
		db.logf("replacing container %s: %s\n", cfg.Name, state.mismatch(cfg))
		if err := db.Remove(ctx); err != nil {
			return nil, err
		}
		if err := db.create(ctx); err != nil {
			return nil, err
		}
		db.fresh = true

	case !state.Running:
		db.logf("starting container %s\n", cfg.Name)
		if _, err := rt.Run(ctx, "start", cfg.Name); err != nil {
			return nil, err
		}
	}

	if err := db.resolvePort(ctx); err != nil {
		return nil, err
	}
	if err := db.waitReady(ctx); err != nil {
		return nil, err
	}
	return db, nil
}

// Port is the host port the container publishes on.
func (d *DB) Port() int { return d.port }

// URL is the connection string, on the port the container really has.
func (d *DB) URL() string {
	cfg := d.cfg
	cfg.Port = d.port
	return cfg.URL()
}

// resolvePort settles which port to connect on.
//
// A configured port is taken as read: it is what was published, and asking the
// engine to confirm it would be a round trip per command for an answer already
// on hand. A container published with no port has one the kernel chose, and the
// engine is the only place that knows it.
//
// The poll is for the gap between `run --detach` returning and the binding
// appearing in the container's network settings. It is normally zero attempts
// long; a busy daemon makes it one or two.
func (d *DB) resolvePort(ctx context.Context) error {
	if d.cfg.Port != 0 {
		d.port = d.cfg.Port
		return nil
	}

	for attempt := range 20 {
		state, err := d.inspect(ctx)
		if err != nil {
			return err
		}
		if state != nil && state.Port != 0 {
			d.port = state.Port
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff(attempt)):
		}
	}

	return fmt.Errorf("container %s published no port that could be read back", d.cfg.Name)
}

// Runtime is the engine backing this database.
func (d *DB) Runtime() Runtime { return d.runtime }

// Stop stops the container without removing it, so the next start is quick.
func (d *DB) Stop(ctx context.Context) error {
	_, err := d.runtime.Run(ctx, "stop", d.cfg.Name)
	return err
}

// Remove deletes the container and its data.
func (d *DB) Remove(ctx context.Context) error {
	_, err := d.runtime.Run(ctx, "rm", "-f", "-v", d.cfg.Name)
	return err
}

// containerState is the part of an inspect result rig cares about.
type containerState struct {
	Running bool
	Image   string
	Port    int
	// Cmd is what the container was started with, which for Postgres carries
	// the -c server parameters. It is how a reuse can tell that a setting has
	// since been asked for: wal_level cannot be turned on after the server has
	// started, so a container without it has to be replaced, not adapted.
	Cmd []string
	// Env is the container's environment, which for the sync service carries
	// the DATABASE_URL it follows. A reuse has to check it, because under
	// isolation the database's port moves and a sync service pointed at the old
	// one is following nothing.
	Env []string
}

// matches reports whether an existing container is the one asked for.
//
// A configured port has to be the one published, or the container is answering
// somewhere the caller is not going to look. With no port configured there is
// nothing to disagree with: the kernel chose one, whatever it chose is where this
// container is, and replacing a perfectly good database to be given a different
// arbitrary number would throw away the warm container isolation exists to keep.
func (s containerState) matches(cfg Config) bool {
	if s.Image != cfg.Image {
		return false
	}
	if len(s.missingSettings(cfg)) > 0 {
		return false
	}
	return cfg.Port == 0 || s.Port == cfg.Port
}

func (s containerState) mismatch(cfg Config) string {
	var reasons []string
	if s.Image != cfg.Image {
		reasons = append(reasons, fmt.Sprintf("image is %s, want %s", s.Image, cfg.Image))
	}
	if cfg.Port != 0 && s.Port != cfg.Port {
		reasons = append(reasons, fmt.Sprintf("port is %d, want %d", s.Port, cfg.Port))
	}
	if missing := s.missingSettings(cfg); len(missing) > 0 {
		reasons = append(reasons, fmt.Sprintf("started without %s", strings.Join(missing, ", ")))
	}
	return strings.Join(reasons, "; ")
}

// missingSettings are the requested server parameters this container was not
// started with. Extra ones it does carry are left alone: a setting removed from
// the configuration stops mattering, and replacing a warm database over it
// would cost the caller everything for nothing.
func (s containerState) missingSettings(cfg Config) []string {
	var missing []string
	for _, want := range cfg.Settings {
		if !slices.Contains(s.Cmd, strings.TrimSpace(want)) {
			missing = append(missing, want)
		}
	}
	return missing
}

// inspect returns the container's state, or nil when it does not exist.
func (d *DB) inspect(ctx context.Context) (*containerState, error) {
	return inspectContainer(ctx, d.runtime, d.cfg.Name)
}

// inspectContainer is inspect for any container this package manages — the
// database's, and the sync service's beside it.
func inspectContainer(ctx context.Context, rt Runtime, name string) (*containerState, error) {
	out, err := rt.Run(ctx, "inspect", name)
	if err != nil {
		// Every engine words "not found" differently, so the check is on the
		// substring rather than on an exit code.
		if strings.Contains(strings.ToLower(err.Error()), "no such") ||
			strings.Contains(strings.ToLower(err.Error()), "not found") {
			return nil, nil
		}
		return nil, err
	}

	var inspected []struct {
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
		Config struct {
			Image string   `json:"Image"`
			Cmd   []string `json:"Cmd"`
			Env   []string `json:"Env"`
		} `json:"Config"`
		HostConfig struct {
			PortBindings map[string][]portBinding `json:"PortBindings"`
		} `json:"HostConfig"`
		NetworkSettings struct {
			Ports map[string][]portBinding `json:"Ports"`
		} `json:"NetworkSettings"`
	}
	if err := json.Unmarshal([]byte(out), &inspected); err != nil {
		return nil, fmt.Errorf("cannot read container state: %w", err)
	}
	if len(inspected) == 0 {
		return nil, nil
	}

	c := inspected[0]
	state := &containerState{
		Running: c.State.Running,
		Image:   c.Config.Image,
		Cmd:     c.Config.Cmd,
		Env:     c.Config.Env,
	}

	// The published port is read from the request, not the result. A container
	// that exists but is stopped — or is still starting — has no live port
	// bindings, and reading zero there would make it look like a mismatch and
	// get a perfectly good container destroyed.
	state.Port = firstHostPort(c.HostConfig.PortBindings)
	if state.Port == 0 {
		state.Port = firstHostPort(c.NetworkSettings.Ports)
	}

	return state, nil
}

// portBinding is one published port in an inspect result.
type portBinding struct {
	HostPort string `json:"HostPort"`
}

func firstHostPort(bindings map[string][]portBinding) int {
	for _, list := range bindings {
		for _, b := range list {
			if p, err := strconv.Atoi(b.HostPort); err == nil && p > 0 {
				return p
			}
		}
	}
	return 0
}

func (d *DB) create(ctx context.Context) error {
	d.logf("creating container %s (%s) on port %s\n", d.cfg.Name, d.cfg.Image, d.cfg.portDescription())

	args := []string{
		"run", "--detach",
		"--name", d.cfg.Name,
		"--publish", d.cfg.publish(),
		"--env", "POSTGRES_DB=" + d.cfg.Database,
		"--env", "POSTGRES_USER=" + d.cfg.User,
		"--env", "POSTGRES_PASSWORD=" + d.cfg.Password,
		// Durability buys nothing here and costs a great deal: this database is
		// rebuilt from migrations whenever anyone asks.
		"--tmpfs", "/var/lib/postgresql/data",
	}
	args = append(args, d.cfg.Image,
		"-c", "fsync=off",
		"-c", "full_page_writes=off",
		"-c", "synchronous_commit=off",
	)
	for _, setting := range d.cfg.Settings {
		args = append(args, "-c", setting)
	}

	_, err := d.runtime.Run(ctx, args...)
	if err != nil {
		return fmt.Errorf("cannot start the database container: %w", err)
	}
	return nil
}

// waitReady polls until the database accepts a connection.
//
// Postgres images start, initialize, and then restart once during first-time
// setup, so a container reported as running is not yet a database that answers.
// Polling a real connection is the only reliable signal.
func (d *DB) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(d.cfg.startWait())

	var lastErr error
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		conn, err := pgx.Connect(ctx, d.URL())
		if err == nil {
			pingErr := conn.Ping(ctx)
			_ = conn.Close(ctx)
			if pingErr == nil {
				return nil
			}
			lastErr = pingErr
		} else {
			lastErr = err
		}

		if attempt == 4 {
			d.logf("waiting for %s to accept connections\n", d.cfg.Name)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff(attempt)):
		}
	}

	return fmt.Errorf("database did not become ready within %s: %w", d.cfg.startWait(), lastErr)
}

// backoff ramps from a tight poll to a relaxed one: a warm container answers
// almost immediately, and a cold one takes seconds.
func backoff(attempt int) time.Duration {
	d := time.Duration(50<<min(attempt, 5)) * time.Millisecond
	return min(d, 500*time.Millisecond)
}

func (d *DB) logf(format string, args ...any) {
	if d.cfg.Log == nil {
		return
	}
	fmt.Fprintf(d.cfg.Log, format, args...)
}

// Attach builds a handle to a container without starting it, for commands that
// stop or remove one.
func Attach(ctx context.Context, cfg Config) (*DB, error) {
	rt, err := FindRuntime(ctx, cfg.Runtime)
	if err != nil {
		return nil, err
	}
	return &DB{cfg: cfg, runtime: rt}, nil
}
