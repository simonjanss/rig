//go:build docker

// `rig db up` managing the sync service beside the database.
//
//	go test -tags docker ./internal/cli/
//
// The unit tests cover the configuration — the defaults, the wal_level append —
// and this covers the part only a real engine can: two containers, one
// following the other over logical replication, brought up, reused, rebuilt and
// stopped by the same three commands that manage the database alone.
package cli_test

import (
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/simonjanss/rig/internal/dockerdb"
)

func TestDBUpManagesTheSyncService(t *testing.T) {
	root := t.TempDir()

	const (
		projectName = "rigElectricCLI"
		dbContainer = projectName + "-db"
		elContainer = projectName + "-electric"
	)

	removeContainer(t, elContainer)
	removeContainer(t, dbContainer)
	t.Cleanup(func() {
		removeContainer(t, elContainer)
		removeContainer(t, dbContainer)
	})

	step(t, "init", func() {
		_, stderr, code := run(t, "init", root, "--name", projectName, "--module", "example.com/electric")
		if code != 0 {
			t.Fatalf("init failed:\n%s", stderr)
		}
	})

	appendTo(t, filepath.Join(root, "rig.yaml"), fmt.Sprintf(
		"\ndatabase:\n  port: %d\n  electric:\n    enabled: true\n    port: %d\n",
		dockerdb.PortCLIElectricDB, dockerdb.PortCLIElectricSync))

	// The URL is read from the command's own output rather than built from the
	// constant, because under isolation the kernel picks the port and the
	// output is the only place it exists.
	syncURL := regexp.MustCompile(`sync service ready at (http://\S+)`)

	var url string
	step(t, "db up starts both", func() {
		_, stderr, code := run(t, "db", "up", "-C", root)
		if code != 0 {
			t.Fatalf("db up failed:\n%s", stderr)
		}
		if !strings.Contains(stderr, "database ready at") {
			t.Errorf("no database line:\n%s", stderr)
		}
		m := syncURL.FindStringSubmatch(stderr)
		if m == nil {
			t.Fatalf("no sync service line:\n%s", stderr)
		}
		url = m[1]

		res, err := (&http.Client{Timeout: 5 * time.Second}).Get(url + "/v1/health")
		if err != nil {
			t.Fatalf("the sync service does not answer: %v", err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if !strings.Contains(string(body), "active") {
			t.Errorf("health = %s, want an active replication stream", body)
		}
	})

	step(t, "a second up reuses both", func() {
		_, stderr, code := run(t, "db", "up", "-C", root)
		if code != 0 {
			t.Fatalf("db up failed:\n%s", stderr)
		}
		if strings.Contains(stderr, "creating container") {
			t.Errorf("the second up should reuse what the first made:\n%s", stderr)
		}
	})

	step(t, "reset rebuilds both", func() {
		_, stderr, code := run(t, "db", "reset", "-C", root)
		if code != 0 {
			t.Fatalf("db reset failed:\n%s", stderr)
		}
		if !strings.Contains(stderr, "database rebuilt at") {
			t.Errorf("no rebuild line:\n%s", stderr)
		}
		// The sync service has to come back too, and come back working: its
		// replication slot lived in the database that was just thrown away.
		m := syncURL.FindStringSubmatch(stderr)
		if m == nil {
			t.Fatalf("reset did not bring the sync service back:\n%s", stderr)
		}
		res, err := (&http.Client{Timeout: 5 * time.Second}).Get(m[1] + "/v1/health")
		if err != nil {
			t.Fatalf("the rebuilt sync service does not answer: %v", err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if !strings.Contains(string(body), "active") {
			t.Errorf("health after reset = %s, want active", body)
		}
	})

	step(t, "down stops both", func() {
		_, stderr, code := run(t, "db", "down", "-C", root)
		if code != 0 {
			t.Fatalf("db down failed:\n%s", stderr)
		}
		for _, name := range []string{elContainer, dbContainer} {
			if !strings.Contains(stderr, "stopped "+dockerdb.Qualify(name)) {
				t.Errorf("no stop line for %s:\n%s", name, stderr)
			}
		}
	})
}
