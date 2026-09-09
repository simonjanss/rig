package dockerdb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Starting a container that has to be given a host port, and what to do when
// the engine cannot take one.
//
// Under [IsolateEnv] the host port is left to the engine — see isolate.go, and
// [HostPort] for why a digest is not an option. The engine allocates it out of
// the range it read from /proc/sys/net/ipv4/ip_local_port_range, which on Linux
// is the same range the kernel hands to outbound connections, and it does not
// ask the kernel which of those are already spoken for. So a `docker run` that
// coincides with a test holding a pool of connections to 127.0.0.1 sometimes
// gets a port that is already somebody's, and fails at the bind.
//
// That is what reddened rig's own Docker Tests job about one push in five: the
// whole of internal/cachetest, or internal/electrictest, or internal/cli dying
// in eight seconds on one `docker run` whose error every test then reports.
// Nothing about the commit, and nothing a rerun would not fix.
//
// The engine allocates sequentially, so the next attempt gets a different port
// and that is the entire fix. Retrying also covers the other way to lose the
// race — a fixed port whose previous container is gone but whose proxy has not
// finished letting go of it.

// ErrPortInUse is what survives every attempt: the engine could not take a host
// port for the container.
//
// Worth matching with [errors.Is] rather than reading, because under isolation
// there is no port in the request to report — the one that was refused is the
// engine's own choice, and only its message names it.
var ErrPortInUse = errors.New("the container engine could not take a host port")

// createAttempts is how many times a create that lost a port race is tried.
//
// Five, with the backoff below, is under four seconds of waiting before giving
// up. It is a lot of attempts for a race that is normally won on the second,
// and the cost of being wrong is asymmetric: the retries cost seconds on a run
// that was already failing, and not having them costs a red main.
const createAttempts = 5

// Create starts a detached container, retrying while the engine cannot take a
// host port for it.
//
// args are everything after `run --detach --name <name>`. It is exported for
// the suites that start a sidecar of their own rather than going through
// [Start] or [StartElectric] — they publish under the same isolation and lose
// the same race. log may be nil.
func Create(ctx context.Context, rt Runtime, log io.Writer, name string, args ...string) error {
	return creator{rt: rt, log: log}.create(ctx, name, args...)
}

// creator is Create with the wait between attempts left open, so a test can run
// the loop without sleeping through it. A field rather than a package variable
// because the tests would otherwise have to agree not to run in parallel.
type creator struct {
	rt  Runtime
	log io.Writer
	// delay is how long to wait after attempt n failed. nil means createBackoff.
	delay func(attempt int) time.Duration
}

func (c creator) create(ctx context.Context, name string, args ...string) error {
	run := append([]string{"run", "--detach", "--name", name}, args...)

	var lastErr error
	for attempt := range createAttempts {
		_, err := c.rt.Run(ctx, run...)
		if err == nil {
			return nil
		}
		if !portInUse(err) {
			return err
		}
		lastErr = err

		// A `run` that got as far as the network leaves the container behind in
		// `created`, still holding the name. Without this the next attempt fails
		// as a name conflict, which reads as a bug in this package rather than
		// as the race it is. Its own error is ignored: there may be nothing to
		// remove, and the failure that matters is the one being retried.
		_, _ = c.rt.Run(ctx, "rm", "-f", "-v", name)

		if attempt == createAttempts-1 {
			break
		}
		c.logf("%s could not take a host port for %s; trying again (%d of %d)\n",
			c.rt.Name(), name, attempt+2, createAttempts)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.wait(attempt)):
		}
	}

	return fmt.Errorf("%w after %d attempts (container %s): %w",
		ErrPortInUse, createAttempts, name, lastErr)
}

func (c creator) wait(attempt int) time.Duration {
	if c.delay != nil {
		return c.delay(attempt)
	}
	return createBackoff(attempt)
}

// createBackoff ramps 250ms, 500ms, 1s, 2s.
//
// It starts at a quarter second rather than at zero on purpose: an immediate
// retry is the right shape for the engine picking a fresh port, and the wrong
// one for a port whose previous container is gone but whose proxy has not
// exited yet. This covers both.
func createBackoff(attempt int) time.Duration {
	d := time.Duration(250<<min(attempt, 3)) * time.Millisecond
	return min(d, 2*time.Second)
}

func (c creator) logf(format string, args ...any) {
	if c.log == nil {
		return
	}
	fmt.Fprintf(c.log, format, args...)
}

// portInUse reports whether err is the engine refusing to publish because it
// could not have the host port.
//
// On the message rather than on an exit code, for the reason inspectContainer
// checks "no such" that way: [CLIRuntime.Run] flattens stderr into the error,
// and the engines agree on no status that separates this from any other failure
// to start. Each string below is one engine's way of saying it.
func portInUse(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, s := range []string{
		// moby, when the kernel refuses the bind the daemon thought was free.
		// This is the one rig's CI produces, inside "driver failed programming
		// external connectivity on endpoint …".
		"address already in use",
		// The same, worded by BSD — Docker Desktop's virtual machine on macOS.
		"address in use",
		// moby's own allocator, when it is the one that knows the port is taken:
		// "Bind for 0.0.0.0:55492 failed: port is already allocated".
		"port is already allocated",
		// Docker Desktop on macOS and Windows, when something outside the VM has
		// the port.
		"ports are not available",
		// podman, with pasta or slirp4netns doing the forwarding.
		"failed to bind port",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}
