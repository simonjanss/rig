package dockerdb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// Keeping one checkout's containers out of another's.
//
// A throwaway database is identified by a container name and reached on a host
// port, and both are written down: the name defaults from project.name and the
// port comes out of rig.yaml. That is the right answer for a project, because a
// name a person can type and a port their .env already carries are the whole
// point of a fixed pair.
//
// It is the wrong answer for two checkouts of one project on one machine. They
// agree on the name, so the second command to run adopts the first's container
// and applies its own migrations on top of a schema from another branch; and
// they agree on the port, so whichever loses the race cannot publish at all.
// What arrives is neither of those facts. It is `rig check` reporting tables the
// branch never introduced, or a migration numbered lower than the database
// version, or a column with no comment in a table nobody wrote — diagnostics
// about the current branch, produced by somebody else's schema.
//
// So a checkout can say it is not the only one. RIG_DB_ISOLATE carries a token
// naming this checkout — rig's own Makefile passes the repository root — and
// when it is set the container name gains a short digest of that token and the
// host port is left to the kernel. Nothing about a project changes: the variable
// is unset everywhere except where somebody has more than one copy of the same
// project open at once.

// IsolateEnv is the environment variable that names this checkout.
//
// Its value is opaque and only has to differ between checkouts; a path is the
// obvious thing to use and is what rig's Makefile passes. Unset or empty means
// one checkout, which is every project rig generates and every machine with one
// copy of it.
const IsolateEnv = "RIG_DB_ISOLATE"

// Isolated reports whether this checkout keeps its containers to itself.
func Isolated() bool { return token() != "" }

// Qualify is the container name to use for one whose configured name is name.
//
// Unchanged when this is the only checkout, so `docker ps` still shows todo-db
// and `rig db psql` is still the way in. Suffixed with a digest of the isolation
// token when it is not — long enough that two checkouts colliding is not a thing
// that happens, short enough to still read as the name it came from.
func Qualify(name string) string {
	t := token()
	if t == "" {
		return name
	}
	sum := sha256.Sum256([]byte(t))
	return name + "-" + hex.EncodeToString(sum[:4])
}

// HostPort is the port to publish a container on, given the one configured.
//
// Zero means "whatever is free", which is the only answer that cannot collide.
// A digest would have been the same trick used for the name, and it does not
// work here: a port is sixteen bits, rig's own suites want a block of them, and
// with a dozen checkouts on one machine the birthday collision is likelier than
// not. The kernel already allocates ports without collisions, so under isolation
// it does the allocating and [DB.URL] reports what it chose.
func HostPort(port int) int {
	if Isolated() {
		return 0
	}
	return port
}

// Publish is the --publish argument for a container this package does not
// manage, so that a test starting a sidecar by hand gets the same treatment its
// database got.
func Publish(bind string, port, containerPort int) string {
	if p := HostPort(port); p != 0 {
		return fmt.Sprintf("%s:%d:%d", bind, p, containerPort)
	}
	return fmt.Sprintf("%s::%d", bind, containerPort)
}

// PortOf reports the host port a container publishes, for one this package did
// not start. Under isolation that is the only way to know it.
func PortOf(ctx context.Context, runtimeBin, name string) (int, error) {
	db, err := Attach(ctx, Config{Name: name, Runtime: runtimeBin})
	if err != nil {
		return 0, err
	}
	if err := db.resolvePort(ctx); err != nil {
		return 0, err
	}
	return db.port, nil
}

func token() string { return strings.TrimSpace(os.Getenv(IsolateEnv)) }
