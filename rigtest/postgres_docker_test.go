//go:build docker

// The Postgres container the suite beside this file runs against.
//
//	go test -tags docker ./rigtest/
//
// It is ~a hundred lines internal/dockerdb already has, and the duplication is
// deliberate for the reason rigs3/minio_docker_test.go states: that package
// lives under the CLI's module, and a published module that imported it would
// require the whole of rig to build. So this borrows the two ideas it cannot do
// without and nothing else — a container name qualified by a digest of
// RIG_DB_ISOLATE, so two checkouts do not adopt each other's schema, and a
// published port the kernel picks under isolation and reads back afterwards.
//
// The number in pgPort is dockerdb.PortRigTest, written out because this module
// cannot import the list it comes from. It is declared there so every port a
// suite in this repository takes is still allocated from one place.
package rigtest_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	pgImage = "postgres:17-alpine"
	pgName  = "rigTest-db"
	// pgPort is dockerdb.PortRigTest. See this file's doc comment.
	pgPort    = 55447
	startWait = 60 * time.Second
)

var (
	once     sync.Once
	dsn      string
	startErr error
)

// database brings Postgres up once for the package and reports its URL.
func database(t *testing.T) string {
	t.Helper()

	once.Do(func() { dsn, startErr = startPostgres() })
	if startErr != nil {
		t.Fatal(startErr)
	}
	return dsn
}

// startPostgres brings the container up and waits until it answers a query.
// Nothing tears it down: the container is left warm, the way every other suite
// in this repository leaves its own.
func startPostgres() (string, error) {
	ctx := context.Background()

	bin, err := runtimeBin()
	if err != nil {
		return "", err
	}
	name := qualify(pgName)

	// A schema left behind by an earlier run of a different branch would make
	// these pass, or fail, for the wrong reason.
	_ = exec.Command(bin, "rm", "-f", "-v", name).Run()

	// Retried, because under isolation the port is the engine's to pick and it
	// picks without asking the kernel — see internal/dockerdb/create.go. A run
	// that reached the network leaves the name taken, so each attempt removes
	// it first.
	const attempts = 5
	var runErr error
	for attempt := range attempts {
		var out []byte
		out, runErr = exec.Command(bin, "run", "--detach",
			"--name", name,
			"--publish", publish(pgPort, 5432),
			"--env", "POSTGRES_DB=rig",
			"--env", "POSTGRES_USER=rig",
			"--env", "POSTGRES_PASSWORD=rig",
			pgImage,
		).CombinedOutput()
		if runErr == nil {
			break
		}
		runErr = fmt.Errorf("%w\n%s", runErr, out)
		if !portInUse(string(out)) {
			return "", fmt.Errorf("start the database: %w", runErr)
		}
		_ = exec.Command(bin, "rm", "-f", "-v", name).Run()
		if attempt < attempts-1 {
			time.Sleep(time.Duration(250<<min(attempt, 3)) * time.Millisecond)
		}
	}
	if runErr != nil {
		return "", fmt.Errorf("start the database, after %d attempts: %w", attempts, runErr)
	}

	port, err := publishedPort(bin, name)
	if err != nil {
		return "", err
	}

	url := fmt.Sprintf("postgres://rig:rig@127.0.0.1:%d/rig?sslmode=disable&TimeZone=UTC", port)
	if err := waitReady(ctx, url); err != nil {
		logs, _ := exec.Command(bin, "logs", "--tail", "40", name).CombinedOutput()
		return "", fmt.Errorf("%w\n%s", err, logs)
	}
	return url, nil
}

// runtimeBin picks a container engine the way internal/dockerdb does: docker,
// then podman.
func runtimeBin() (string, error) {
	for _, bin := range []string{"docker", "podman"} {
		if path, err := exec.LookPath(bin); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("no container engine found; this suite needs docker or podman")
}

// portInUse reports whether the engine's output is it refusing to publish
// because it could not have the host port. The wording differs per engine, so a
// substring is the only thing there is to match on.
func portInUse(out string) bool {
	msg := strings.ToLower(out)
	for _, s := range []string{
		"address already in use",
		"address in use",
		"port is already allocated",
		"ports are not available",
		"failed to bind port",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// qualify keeps this checkout's container away from another checkout's, by
// digesting the same variable internal/dockerdb reads.
func qualify(name string) string {
	token := strings.TrimSpace(os.Getenv("RIG_DB_ISOLATE"))
	if token == "" {
		return name
	}
	sum := sha256.Sum256([]byte(token))
	return name + "-" + hex.EncodeToString(sum[:4])
}

// publish pins the port when this is the only checkout and lets the kernel
// choose when it is not — a port from the registry as a request rather than a
// requirement, which is the rule internal/dockerdb/isolate.go states.
func publish(host, container int) string {
	if strings.TrimSpace(os.Getenv("RIG_DB_ISOLATE")) != "" {
		return fmt.Sprintf("127.0.0.1::%d", container)
	}
	return fmt.Sprintf("127.0.0.1:%d:%d", host, container)
}

// publishedPort asks the engine which port the container really got, which
// under isolation is the only way to know.
func publishedPort(bin, name string) (int, error) {
	out, err := exec.Command(bin, "port", name, "5432/tcp").Output()
	if err != nil {
		return 0, fmt.Errorf("read the database's port: %w", err)
	}

	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	_, port, ok := strings.Cut(line, ":")
	if !ok {
		return 0, fmt.Errorf("the database published no port that could be read back: %q", out)
	}
	n, err := strconv.Atoi(strings.TrimSpace(port))
	if err != nil {
		return 0, fmt.Errorf("the database published %q, which is not a port", port)
	}
	return n, nil
}

// waitReady polls until a query answers. Accepting the connection is not enough:
// the official image starts a throwaway server to run its init scripts and then
// restarts, so a connection in that gap succeeds and the next one is refused.
func waitReady(ctx context.Context, url string) error {
	deadline := time.Now().Add(startWait)

	var last error
	for time.Now().Before(deadline) {
		pool, err := pgxpool.New(ctx, url)
		if err == nil {
			last = pool.Ping(ctx)
			pool.Close()
			if last == nil {
				return nil
			}
		} else {
			last = err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("the database did not answer within %s: %w", startWait, last)
}
