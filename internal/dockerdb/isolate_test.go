package dockerdb

import (
	"strings"
	"testing"
)

// The default is one checkout, because that is what a project is. A rig user with
// one copy of their repository must see the container name they wrote down and
// the port their .env already carries.
func TestWithoutIsolationNothingMoves(t *testing.T) {
	t.Setenv(IsolateEnv, "")

	if Isolated() {
		t.Error("isolated with nothing set")
	}
	if got := Qualify("todo-db"); got != "todo-db" {
		t.Errorf("name = %q, want it untouched", got)
	}
	if got := HostPort(PortExampleTodo); got != PortExampleTodo {
		t.Errorf("port = %d, want the configured %d", got, PortExampleTodo)
	}
}

// A checkout that says so gets a name of its own and no port of its own, which
// is the whole mechanism: the name is what stops two checkouts sharing a
// database, and leaving the port out is what stops them fighting over one.
func TestIsolationRenamesAndUnpinsThePort(t *testing.T) {
	t.Setenv(IsolateEnv, "/Users/x/workspaces/rig/dushanbe")

	if !Isolated() {
		t.Fatal("not isolated with a token set")
	}

	name := Qualify("todo-db")
	if !strings.HasPrefix(name, "todo-db-") {
		t.Errorf("name = %q, want it to still read as todo-db", name)
	}
	if name == "todo-db" {
		t.Error("name unchanged, so both checkouts still share the container")
	}
	if got := HostPort(PortExampleTodo); got != 0 {
		t.Errorf("port = %d, want 0: the kernel is what allocates without colliding", got)
	}
}

// Two checkouts must not land on one name, and the same checkout must land on
// the same name every command — a name that moved between `rig generate` and
// `rig db psql` would be a fresh empty database each time.
func TestTheNameIsStablePerCheckoutAndDistinctBetweenThem(t *testing.T) {
	t.Setenv(IsolateEnv, "/workspaces/rig/dushanbe")
	first := Qualify("todo-db")
	if second := Qualify("todo-db"); second != first {
		t.Errorf("same checkout got %q then %q", first, second)
	}

	t.Setenv(IsolateEnv, "/workspaces/rig/khartoum")
	if other := Qualify("todo-db"); other == first {
		t.Errorf("two checkouts both got %q", other)
	}
}

// Surrounding space is what a shell leaves behind, and a token of nothing but
// space is not somebody asking to be isolated.
func TestABlankTokenIsNotIsolation(t *testing.T) {
	t.Setenv(IsolateEnv, "   \n")

	if Isolated() {
		t.Error("blank token read as isolation")
	}
	if got := Qualify("todo-db"); got != "todo-db" {
		t.Errorf("name = %q, want it untouched", got)
	}
}

// Publish is the same rule for a sidecar somebody starts by hand — the sync
// service in internal/electrictest — so it cannot drift from what the database
// beside it does.
func TestPublishFollowsTheSameRule(t *testing.T) {
	t.Setenv(IsolateEnv, "")
	if got, want := Publish("127.0.0.1", PortElectricSync, 3000), "127.0.0.1:55490:3000"; got != want {
		t.Errorf("publish = %q, want %q", got, want)
	}

	t.Setenv(IsolateEnv, "/workspaces/rig/dushanbe")
	if got, want := Publish("127.0.0.1", PortElectricSync, 3000), "127.0.0.1::3000"; got != want {
		t.Errorf("publish = %q, want %q: an empty host port is the engine's "+
			"spelling of pick one", got, want)
	}
}

// A configured port is published as configured; no port asks the engine for one.
// The difference is one colon, and getting it wrong is a container that either
// does not start or starts somewhere nobody looks.
func TestConfigPublish(t *testing.T) {
	if got, want := (Config{Port: 55440}).publish(), "127.0.0.1:55440:5432"; got != want {
		t.Errorf("publish = %q, want %q", got, want)
	}
	if got, want := (Config{}).publish(), "127.0.0.1::5432"; got != want {
		t.Errorf("publish = %q, want %q", got, want)
	}
	if got, want := (Config{Bind: "0.0.0.0"}).publish(), "0.0.0.0::5432"; got != want {
		t.Errorf("publish = %q, want %q", got, want)
	}
}

// A warm container is worth keeping. With no port configured there is nothing
// for an existing one to disagree with, so it is adopted at whatever port it
// already has rather than destroyed for having an arbitrary number that differs
// from the arbitrary number a new one would get.
func TestAContainerWithNoConfiguredPortIsAdopted(t *testing.T) {
	cfg := Config{Image: "postgres:17-alpine"}

	if !(containerState{Image: cfg.Image, Port: 49213}).matches(cfg) {
		t.Error("an existing container was rejected for the port the kernel gave it")
	}
	if (containerState{Image: "postgres:16-alpine", Port: 49213}).matches(cfg) {
		t.Error("the wrong image matched, and the image is what the schema was built on")
	}

	// A configured port still has to be the published one, or every command
	// after this connects somewhere the container is not.
	pinned := Config{Image: cfg.Image, Port: 55440}
	if (containerState{Image: cfg.Image, Port: 49213}).matches(pinned) {
		t.Error("a container on the wrong port matched a configured one")
	}
}
