package dockerdb

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

// The sync service, run the way the database is.
//
// ElectricSQL follows Postgres over logical replication and serves shapes over
// HTTP, and a project doing live sync needs it up before a stream answers. It
// is a second container with the database's lifecycle — up with it, down with
// it — which is why it lives in this package rather than beside the proxy that
// forwards to it.

// ElectricSyncPort is the port the sync service listens on inside its
// container. Fixed by the image; the host side is [ElectricConfig.Port].
const ElectricSyncPort = 3000

// ElectricConfig describes the sync-service container.
type ElectricConfig struct {
	Image string
	Name  string
	// Port is the host port to publish on. Zero means the kernel picks, which
	// is what isolation asks for.
	Port int

	// DBPort is the host port the database really publishes on — [DB.Port],
	// not the configured number, because under isolation they differ. The sync
	// service reaches it through the container runtime's host gateway, which is
	// why the host port is the right one to hand over.
	DBPort     int
	DBName     string
	DBUser     string
	DBPassword string

	Runtime   string // "" picks docker, then podman
	Log       io.Writer
	StartWait time.Duration
}

// databaseURL is what the sync service is told to follow. host.docker.internal
// resolves to the host from inside a container — Docker Desktop routes it
// always, and the --add-host flag in create is what makes it true on Linux.
func (c ElectricConfig) databaseURL() string {
	return fmt.Sprintf("postgresql://%s:%s@host.docker.internal:%d/%s?sslmode=disable",
		c.DBUser, c.DBPassword, c.DBPort, c.DBName)
}

func (c ElectricConfig) startWait() time.Duration {
	if c.StartWait > 0 {
		return c.StartWait
	}
	return 60 * time.Second
}

// Electric is a running sync service.
type Electric struct {
	cfg     ElectricConfig
	runtime Runtime
	port    int
}

// StartElectric brings the sync service up and waits until it reports its
// replication stream active.
//
// An existing container is reused when it is running the same image on the same
// port against the same database URL. The last condition is the one specific to
// this container: a database that was recreated — a tmpfs restart, an isolated
// checkout on a new kernel-chosen port — leaves a reused sync service holding a
// replication slot into nothing, so anything stale is replaced rather than
// adapted. The caller that knows the database is fresh removes this container
// first; the URL check catches what it cannot see.
func StartElectric(ctx context.Context, cfg ElectricConfig) (*Electric, error) {
	rt, err := FindRuntime(ctx, cfg.Runtime)
	if err != nil {
		return nil, err
	}
	e := &Electric{cfg: cfg, runtime: rt}

	state, err := inspectContainer(ctx, rt, cfg.Name)
	if err != nil {
		return nil, err
	}

	switch {
	case state == nil:
		if err := e.create(ctx); err != nil {
			return nil, err
		}

	case !e.matches(state):
		e.logf("replacing container %s: %s\n", cfg.Name, e.mismatch(state))
		if err := e.Remove(ctx); err != nil {
			return nil, err
		}
		if err := e.create(ctx); err != nil {
			return nil, err
		}

	case !state.Running:
		e.logf("starting container %s\n", cfg.Name)
		if _, err := rt.Run(ctx, "start", cfg.Name); err != nil {
			return nil, err
		}
	}

	if err := e.resolvePort(ctx); err != nil {
		return nil, err
	}
	if err := e.waitReady(ctx); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *Electric) matches(s *containerState) bool {
	if s.Image != e.cfg.Image {
		return false
	}
	if e.cfg.Port != 0 && s.Port != e.cfg.Port {
		return false
	}
	return slices.Contains(s.Env, "DATABASE_URL="+e.cfg.databaseURL())
}

func (e *Electric) mismatch(s *containerState) string {
	var reasons []string
	if s.Image != e.cfg.Image {
		reasons = append(reasons, fmt.Sprintf("image is %s, want %s", s.Image, e.cfg.Image))
	}
	if e.cfg.Port != 0 && s.Port != e.cfg.Port {
		reasons = append(reasons, fmt.Sprintf("port is %d, want %d", s.Port, e.cfg.Port))
	}
	if !slices.Contains(s.Env, "DATABASE_URL="+e.cfg.databaseURL()) {
		reasons = append(reasons, "it follows a different database")
	}
	return strings.Join(reasons, "; ")
}

func (e *Electric) create(ctx context.Context) error {
	e.logf("creating container %s (%s)\n", e.cfg.Name, e.cfg.Image)

	_, err := e.runtime.Run(ctx,
		"run", "--detach",
		"--name", e.cfg.Name,
		"--publish", Publish("127.0.0.1", e.cfg.Port, ElectricSyncPort),
		// The service reaches Postgres through the host: the database publishes
		// on a host port, and naming the host is what works on every engine
		// without a shared network to manage. Docker Desktop routes
		// host.docker.internal on its own; this flag is what makes it resolve
		// on Linux.
		"--add-host", "host.docker.internal:host-gateway",
		"--env", "DATABASE_URL="+e.cfg.databaseURL(),
		// The throwaway pair has no TLS and no API secret, exactly like the
		// database beside it. Nothing about this is a deployment.
		"--env", "ELECTRIC_INSECURE=true",
		e.cfg.Image,
	)
	if err != nil {
		return fmt.Errorf("cannot start the sync-service container: %w", err)
	}
	return nil
}

// resolvePort settles which host port the service answers on, the same dance
// the database does: a configured port is taken as read, and one the kernel
// chose is read back from the engine.
func (e *Electric) resolvePort(ctx context.Context) error {
	if e.cfg.Port != 0 {
		e.port = e.cfg.Port
		return nil
	}

	for attempt := range 20 {
		state, err := inspectContainer(ctx, e.runtime, e.cfg.Name)
		if err != nil {
			return err
		}
		if state != nil && state.Port != 0 {
			e.port = state.Port
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff(attempt)):
		}
	}
	return fmt.Errorf("container %s published no port that could be read back", e.cfg.Name)
}

// Port is the host port the service publishes on.
func (e *Electric) Port() int { return e.port }

// URL is where the shape proxy should forward to.
func (e *Electric) URL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", e.port)
}

// Stop stops the container without removing it.
func (e *Electric) Stop(ctx context.Context) error {
	_, err := e.runtime.Run(ctx, "stop", e.cfg.Name)
	return err
}

// Remove deletes the container.
func (e *Electric) Remove(ctx context.Context) error {
	_, err := e.runtime.Run(ctx, "rm", "-f", "-v", e.cfg.Name)
	return err
}

// waitReady polls the service's health endpoint until the replication stream is
// active. Answering HTTP is not enough: the service comes up before it has
// connected to Postgres, reports its own state in the body, and a shape request
// in the gap would hang rather than fail.
func (e *Electric) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(e.cfg.startWait())
	client := &http.Client{Timeout: 5 * time.Second}
	url := e.URL() + "/v1/health"

	var last string
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		res, err := client.Do(req)
		if err == nil {
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode == http.StatusOK && strings.Contains(string(body), "active") {
				return nil
			}
			last = fmt.Sprintf("status %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
		} else {
			last = err.Error()
		}

		if attempt == 4 {
			e.logf("waiting for %s to connect to the database\n", e.cfg.Name)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff(attempt)):
		}
	}

	logs, _ := e.runtime.Run(ctx, "logs", "--tail", "40", e.cfg.Name)
	return fmt.Errorf("sync service not ready within %s (last answer: %s)\n%s",
		e.cfg.startWait(), last, logs)
}

// AttachElectric builds a handle without starting anything, for commands that
// stop or remove the container.
func AttachElectric(ctx context.Context, cfg ElectricConfig) (*Electric, error) {
	rt, err := FindRuntime(ctx, cfg.Runtime)
	if err != nil {
		return nil, err
	}
	return &Electric{cfg: cfg, runtime: rt}, nil
}

func (e *Electric) logf(format string, args ...any) {
	if e.cfg.Log == nil {
		return
	}
	fmt.Fprintf(e.cfg.Log, format, args...)
}
