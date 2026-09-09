package dockerdb

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// What a container start does when the engine cannot take a host port.
//
// The failure these cover is not reproducible on demand — it is the engine
// choosing a port the kernel had already given to somebody else, which happens
// on a busy machine and not on a quiet one. So the tests script the engine's
// answers instead, using the exact text CI produced.

// theCIFailure is verbatim from the Docker Tests job on main, endpoint name and
// all. Matching a paraphrase would prove nothing about the message that
// actually arrives.
const theCIFailure = "docker: Error response from daemon: failed to set up container networking: " +
	"driver failed programming external connectivity on endpoint rigCache-db-aa2b1fdb " +
	"(7288bdf2ea080fd34a3fdd94cd4d6c909fd9007604a5e5ca424d0691faa736c7): " +
	"failed to listen on TCP socket: address already in use"

// noWait is the delay a test uses, so the loop runs without its backoff.
func noWait(int) time.Duration { return 0 }

func TestAPortRaceIsRunAgain(t *testing.T) {
	t.Parallel()

	rt := &scriptRuntime{results: []scriptResult{{stderr: theCIFailure}, {out: "deadbeef"}}}
	c := creator{rt: rt, delay: noWait}

	if err := c.create(context.Background(), "rigCache-db", "--publish", "127.0.0.1::5432", "postgres:17"); err != nil {
		t.Fatalf("create: %v", err)
	}

	want := []string{
		"run --detach --name rigCache-db --publish 127.0.0.1::5432 postgres:17",
		// The half-made container has to go first. `docker run` that got as far
		// as the network leaves it behind in `created` still holding the name,
		// so without this the second attempt fails as a name conflict and the
		// port race is never retried at all.
		"rm -f -v rigCache-db",
		"run --detach --name rigCache-db --publish 127.0.0.1::5432 postgres:17",
	}
	got := rt.commands()
	if len(got) != len(want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("command %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// The one that keeps a real breakage fast: five attempts on a missing image
// would be four sleeps spent on an answer that is not going to change.
func TestAnyOtherFailureIsNotRunAgain(t *testing.T) {
	t.Parallel()

	const missing = "Unable to find image 'postgres:99' locally: manifest unknown"
	rt := &scriptRuntime{results: []scriptResult{{stderr: missing}}}
	c := creator{rt: rt, delay: noWait}

	err := c.create(context.Background(), "todo-db", "postgres:99")
	if err == nil {
		t.Fatal("create returned nil, want the engine's error")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error does not carry what the engine said: %v", err)
	}
	if errors.Is(err, ErrPortInUse) {
		t.Errorf("error is ErrPortInUse: %v", err)
	}
	if got := rt.commands(); len(got) != 1 {
		t.Errorf("commands = %v, want one", got)
	}
}

func TestGivingUpNamesTheContainerAndTheEngine(t *testing.T) {
	t.Parallel()

	rt := &scriptRuntime{results: []scriptResult{{stderr: theCIFailure}}}
	c := creator{rt: rt, delay: noWait}

	err := c.create(context.Background(), "rigCache-db", "postgres:17")
	if !errors.Is(err, ErrPortInUse) {
		t.Fatalf("error is not ErrPortInUse: %v", err)
	}
	for _, want := range []string{"5 attempts", "rigCache-db", "address already in use"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}

	// Five runs, and a removal between each pair.
	runs := 0
	for _, cmd := range rt.commands() {
		if strings.HasPrefix(cmd, "run ") {
			runs++
		}
	}
	if runs != createAttempts {
		t.Errorf("ran %d times, want %d", runs, createAttempts)
	}
}

// Every engine words it differently, and the whole retry hangs off recognising
// the wording. The positives are what each engine prints; the negatives are the
// two failures most likely to be swept up by a looser match.
func TestEveryEngineSaysItDifferently(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  string
		want bool
	}{
		{"moby on linux", theCIFailure, true},
		{"moby's own allocator", "docker: Error response from daemon: driver failed programming external " +
			"connectivity on endpoint todo-db: Bind for 0.0.0.0:55440 failed: port is already allocated", true},
		{"docker desktop", "docker: Error response from daemon: Ports are not available: " +
			"exposing port TCP 127.0.0.1:55440 -> 0.0.0.0:0: listen tcp 127.0.0.1:55440: bind: address already in use", true},
		{"a bsd bind", "Error: rootlessport listen tcp 127.0.0.1:55440: bind: address in use", true},
		{"podman with pasta", "Error: failed to bind port 127.0.0.1:55440/tcp: address already in use", true},

		{"no such container", "Error response from daemon: No such container: todo-db", false},
		{"a full disk", "docker: write /var/lib/docker: no space left on device", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := portInUse(errors.New(tt.msg)); got != tt.want {
				t.Errorf("portInUse = %v, want %v", got, tt.want)
			}
		})
	}
}

// The database's create is the one CI produced, and the one the tests above go
// through. This is the sync service's, so the delegation cannot be dropped from
// one of them without anything noticing.
func TestBothContainersRunAgain(t *testing.T) {
	t.Parallel()

	pg := &scriptRuntime{results: []scriptResult{{stderr: theCIFailure}, {out: "deadbeef"}}}
	db := &DB{cfg: Config{Name: "todo-db", Image: "postgres:17"}, runtime: pg, retryDelay: noWait}
	if err := db.create(context.Background()); err != nil {
		t.Errorf("DB.create: %v", err)
	}

	sync := &scriptRuntime{results: []scriptResult{{stderr: theCIFailure}, {out: "deadbeef"}}}
	e := &Electric{
		cfg:        ElectricConfig{Name: "todo-electric", Image: "electricsql/electric:latest"},
		runtime:    sync,
		retryDelay: noWait,
	}
	if err := e.create(context.Background()); err != nil {
		t.Errorf("Electric.create: %v", err)
	}
}

// A configured port is the only case a person can act on, so it is the only one
// the message spells out. Under isolation the port was the engine's own choice
// and there is no setting to point at.
func TestAConfiguredPortIsNamedAndAChosenOneIsNot(t *testing.T) {
	t.Parallel()

	fixed := &DB{
		cfg:        Config{Name: "todo-db", Image: "postgres:17", Port: 55440},
		runtime:    &scriptRuntime{results: []scriptResult{{stderr: theCIFailure}}},
		retryDelay: noWait,
	}
	err := fixed.create(context.Background())
	if err == nil || !strings.Contains(err.Error(), "database.port") {
		t.Errorf("error does not point at database.port: %v", err)
	}
	if !strings.Contains(err.Error(), "55440") {
		t.Errorf("error does not name the port: %v", err)
	}

	chosen := &DB{
		cfg:        Config{Name: "todo-db", Image: "postgres:17"},
		runtime:    &scriptRuntime{results: []scriptResult{{stderr: theCIFailure}}},
		retryDelay: noWait,
	}
	err = chosen.create(context.Background())
	if err == nil || strings.Contains(err.Error(), "database.port") {
		t.Errorf("error points at database.port for a port nobody configured: %v", err)
	}

	// And the sync service, which is the same rule under a different key. Both
	// halves are here so neither can lose the pointer on its own.
	fixedSync := &Electric{
		cfg:        ElectricConfig{Name: "todo-electric", Image: "electricsql/electric:latest", Port: PortDefaultElectric},
		runtime:    &scriptRuntime{results: []scriptResult{{stderr: theCIFailure}}},
		retryDelay: noWait,
	}
	err = fixedSync.create(context.Background())
	if err == nil || !strings.Contains(err.Error(), "database.electric.port") {
		t.Errorf("error does not point at database.electric.port: %v", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(PortDefaultElectric)) {
		t.Errorf("error does not name the sync service's port: %v", err)
	}

	chosenSync := &Electric{
		cfg:        ElectricConfig{Name: "todo-electric", Image: "electricsql/electric:latest"},
		runtime:    &scriptRuntime{results: []scriptResult{{stderr: theCIFailure}}},
		retryDelay: noWait,
	}
	err = chosenSync.create(context.Background())
	if err == nil || strings.Contains(err.Error(), "database.electric.port") {
		t.Errorf("error points at database.electric.port for a port nobody configured: %v", err)
	}
}
