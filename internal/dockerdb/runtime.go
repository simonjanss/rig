// Package dockerdb manages the throwaway Postgres rig runs migrations against.
//
// The container is deliberately long-lived: it is started on the first command
// that needs it and left running, so the next command finds it warm. Starting
// and stopping a database on every invocation would make the inner loop several
// seconds slower for no benefit — nothing in it is precious, and `rig db reset`
// throws it away when the schema needs to start over.
//
// Commands are run by shelling out to docker or podman rather than by linking a
// client library. Four operations — inspect, run, start, remove — do not justify
// the dependency, and shelling out is what makes podman work for free.
package dockerdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Runtime is a container engine.
type Runtime interface {
	// Name is the executable, for messages.
	Name() string
	// Run executes a command and returns its standard output.
	Run(ctx context.Context, args ...string) (string, error)
	// Available reports whether the engine can be reached.
	Available(ctx context.Context) error
}

// ErrNoRuntime is returned when no container engine is usable.
var ErrNoRuntime = errors.New("no container engine found")

// CLIRuntime drives a container engine through its command-line tool.
type CLIRuntime struct{ Bin string }

// Name implements [Runtime].
func (r CLIRuntime) Name() string { return r.Bin }

// Run implements [Runtime].
func (r CLIRuntime) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, r.Bin, args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg != "" {
			return "", fmt.Errorf("%s %s: %s", r.Bin, args[0], msg)
		}
		return "", fmt.Errorf("%s %s: %w", r.Bin, args[0], err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Available implements [Runtime]. It asks the daemon for its version rather
// than only checking that the binary exists, because an installed docker with
// no daemon behind it is the common failure and deserves a clear message.
func (r CLIRuntime) Available(ctx context.Context) error {
	if _, err := exec.LookPath(r.Bin); err != nil {
		return fmt.Errorf("%s is not installed: %w", r.Bin, err)
	}
	if _, err := r.Run(ctx, "version", "--format", "{{.Server.Version}}"); err != nil {
		return fmt.Errorf("%s is installed but not responding; is the daemon running? %w", r.Bin, err)
	}
	return nil
}

// FindRuntime picks a container engine.
//
// An explicit choice is honored even if it turns out not to work, so the error
// names what the caller asked for rather than silently falling back to
// something else.
func FindRuntime(ctx context.Context, preferred string) (Runtime, error) {
	if preferred != "" {
		r := CLIRuntime{Bin: preferred}
		if err := r.Available(ctx); err != nil {
			return nil, err
		}
		return r, nil
	}

	var problems []string
	for _, bin := range []string{"docker", "podman"} {
		r := CLIRuntime{Bin: bin}
		if err := r.Available(ctx); err != nil {
			problems = append(problems, "  "+err.Error())
			continue
		}
		return r, nil
	}

	return nil, fmt.Errorf("%w; rig needs one to run migrations:\n%s\n\n"+
		"Alternatively, point rig at a database you manage by setting database.url in rig.yaml",
		ErrNoRuntime, strings.Join(problems, "\n"))
}
