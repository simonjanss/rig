//go:build docker

// The sync service connecting as the role rig's foundation creates, rather than as the
// superuser a laptop hands it.
//
//	go test -tags docker -run Role ./internal/electrictest/
//
// This is the case rig otherwise never runs. `rig db up` points the local sync service at
// `database.user`, which is the owner of the throwaway database — so on a laptop Electric
// can publish a table for itself, which is exactly the privilege a deployment withholds
// and exactly why RIG5090 exists. The migration and the password setter are written for
// the other environment, and the failure they guard against — a verifier Postgres will not
// accept, a grant that turns out to be too narrow — is one only a real server can report.
//
// So the suite next door proves what a shape returns. This one proves the credential
// works: the role exists with the attributes the migration gives it, a SCRAM verifier
// built in Go authenticates against it, and a shape served through that connection carries
// rows.
package electrictest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/internal/dockerdb"
	"github.com/simonjanss/rig/internal/scaffold"
	"github.com/simonjanss/rig/runtime/electric"
	electricfnd "github.com/simonjanss/rig/runtime/electric/foundation"
)

const (
	rolePgName   = "rigElectricRole-db"
	rolePgPort   = dockerdb.PortElectricRoleDB
	roleSyncName = "rigElectricRole-sync"
	roleSyncPort = dockerdb.PortElectricRoleSync

	// Alphanumeric, which is not incidental: SCRAM normalisation is the identity map
	// on that alphabet and electric.SetRolePassword refuses anything outside it. A
	// password with a symbol in it here would be testing the refusal instead.
	rolePassword = "s3cretRoleP4ssword"
)

// TestTheSyncServiceRunsAsTheLeastPrivilegedRole is the whole file.
//
// One test rather than several because the steps are one arrangement — the role does not
// exist until the migration runs, cannot connect until the password is set, and cannot
// serve a shape until the table is published — and a suite that split them would either
// repeat the setup four times or share it and depend on the order.
func TestTheSyncServiceRunsAsTheLeastPrivilegedRole(t *testing.T) {
	ctx := context.Background()

	pg, sync := dockerdb.Qualify(rolePgName), dockerdb.Qualify(roleSyncName)
	remove(pg)
	remove(sync)
	t.Cleanup(func() { remove(sync); remove(pg) })

	db, err := dockerdb.Start(ctx, dockerdb.Config{
		Image: "postgres:17-alpine", Name: pg, Port: dockerdb.HostPort(rolePgPort),
		Database: "rig", User: "rig", Password: "rig",
		Settings:  []string{"wal_level=logical"},
		StartWait: startWait,
		Bind:      hostBind(),
	})
	if err != nil {
		t.Fatal(err)
	}

	pool, err := pgxpool.New(ctx, db.URL())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	// The project's own schema first, so that the grants below have something to cover
	// and so ALTER DEFAULT PRIVILEGES has a table created *after* it to prove itself on.
	if _, err := pool.Exec(ctx, schema); err != nil {
		t.Fatalf("create the table: %v", err)
	}

	// Step one: the foundation's migration, applied the way goose would apply it. Read
	// out of the set rather than pasted here, so this is a test of the SQL rig ships.
	roleSQL, err := electricfnd.Set().Read("electric_role")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, goosePlainUp(string(roleSQL))); err != nil {
		t.Fatalf("apply the electric role migration: %v", err)
	}

	// The attributes the migration is supposed to have given it. On this image there is
	// no rds_replication to grant, so the branch taken is ALTER ROLE ... REPLICATION —
	// which is the half a managed Postgres cannot run, and the half this image requires.
	var canLogin, canReplicate bool
	if err := pool.QueryRow(ctx,
		`SELECT rolcanlogin, rolreplication FROM pg_roles WHERE rolname = $1`, electric.Role,
	).Scan(&canLogin, &canReplicate); err != nil {
		t.Fatalf("the migration did not create the role %q: %v", electric.Role, err)
	}
	if !canLogin || !canReplicate {
		t.Errorf("role %q: login=%v replication=%v, want both", electric.Role, canLogin, canReplicate)
	}

	// And that it cannot log in yet, which is the property that lets the SQL be a file in
	// git. A role with a password in the migration would be a credential in the
	// repository.
	var hasPassword bool
	if err := pool.QueryRow(ctx,
		`SELECT rolpassword IS NOT NULL FROM pg_authid WHERE rolname = $1`, electric.Role,
	).Scan(&hasPassword); err != nil {
		t.Fatal(err)
	}
	if hasPassword {
		t.Error("the migration left a password on the role, so the SQL carries a credential")
	}

	// Step two: the password, out of the connection string the sync service will use.
	dsn := fmt.Sprintf("postgresql://%s:%s@host.docker.internal:%d/rig?sslmode=disable",
		electric.Role, rolePassword, db.Port())
	set, err := electric.SetRolePassword(ctx, pool, dsn, electric.Role)
	if err != nil {
		t.Fatalf("SetRolePassword: %v", err)
	}
	if !set {
		t.Fatal("SetRolePassword reported doing nothing")
	}

	// A SCRAM verifier and not the password, which is the point of the whole exercise:
	// what reached the server — and therefore the server's log — cannot be replayed to
	// log in.
	var stored string
	if err := pool.QueryRow(ctx,
		`SELECT rolpassword FROM pg_authid WHERE rolname = $1`, electric.Role,
	).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) < len("SCRAM-SHA-256$") || stored[:len("SCRAM-SHA-256$")] != "SCRAM-SHA-256$" {
		t.Errorf("what Postgres stored is not a SCRAM verifier: %q", stored)
	}

	// The verifier actually authenticates. This is the assertion the unit test against
	// RFC 7677's vector cannot make: a verifier can be byte-correct by that measure and
	// still be rejected if anything about the encoding is wrong.
	asRole, err := pgxpool.New(ctx, fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/rig?sslmode=disable",
		electric.Role, rolePassword, db.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer asRole.Close()
	if err := asRole.Ping(ctx); err != nil {
		t.Fatalf("the role cannot log in with the password the verifier was built from: %v", err)
	}

	// What the grants are for: SELECT on a table created *before* the migration ran,
	// which ON ALL TABLES covers, and one created *after*, which only ALTER DEFAULT
	// PRIVILEGES does. The second is the one that fails silently in a deployment — the
	// table is invisible and the shape comes back empty rather than failing.
	if _, err := pool.Exec(ctx, `CREATE TABLE later_table (id uuid PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"lesson", "later_table"} {
		var allowed bool
		if err := asRole.QueryRow(ctx,
			`SELECT has_table_privilege($1, $2, 'SELECT')`, electric.Role, table,
		).Scan(&allowed); err != nil {
			t.Fatal(err)
		}
		if !allowed {
			t.Errorf("the role cannot read %s; a shape over it would come back empty", table)
		}
	}

	// Step three: the publication and the identity, from the SQL rig writes.
	//
	// scaffold.Publish rather than a literal, so this is a test of what the command
	// produces: the ordering it depends on is exactly what is being checked. The
	// publication is created here, before the sync service has ever connected, so the
	// migration role owns it — which is what lets a role owning no tables stream, and
	// what RIG5090 reads to tell this apart from a publication the service made itself.
	if _, err := pool.Exec(ctx, goosePlainUp(scaffold.Publish(scaffold.PublishOptions{
		Create:   true,
		Add:      []string{"lesson"},
		Identity: []string{"lesson"},
	}))); err != nil {
		t.Fatalf("apply the publish-shapes migration: %v", err)
	}

	// It has to be ours. The whole arrangement turns on this one row.
	var owned bool
	if err := pool.QueryRow(ctx,
		`SELECT pubowner = current_user::regrole FROM pg_publication WHERE pubname = $1`,
		scaffold.DefaultPublication,
	).Scan(&owned); err != nil {
		t.Fatal(err)
	}
	if !owned {
		t.Fatalf("%s is not owned by the role that ran the migration", scaffold.DefaultPublication)
	}

	// Why the publication has to be that one and has to be created here, which is the
	// finding this test exists to pin. The sync service reads **only** its own
	// publication. A publication under a name of the project's own is never consulted,
	// so a table published there and nowhere else streams nothing for a role owning no
	// tables — and neither error says which publication it means:
	//
	//	must be owner of table lesson
	//
	// with the default settings, when the service tries to maintain its own publication
	// and lacks the ownership Postgres requires. And:
	//
	//	Database table "public.lesson" is missing from the publication
	//	"electric_publication_default" and the ELECTRIC_MANUAL_TABLE_PUBLISHING setting
	//	prevents Electric from adding it
	//
	// with manual publishing on, which does not mean "use the publication my migrations
	// wrote" but "the table must already be in mine, and I will not add it". Getting
	// there first is the only arrangement that answers both.

	// Step four: the sync service, as the role rather than as the superuser.
	out, err := exec.Command("docker", "run", "--detach",
		"--name", sync,
		"--publish", dockerdb.Publish("127.0.0.1", roleSyncPort, 3000),
		"--add-host", "host.docker.internal:host-gateway",
		"--env", "DATABASE_URL="+dsn,
		"--env", "ELECTRIC_INSECURE=true",
		"--env", "ELECTRIC_MANUAL_TABLE_PUBLISHING=true",
		"electricsql/electric:1.6.9",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("start the sync service: %v\n%s", err, out)
	}

	published, err := dockerdb.PortOf(ctx, "docker", sync)
	if err != nil {
		t.Fatal(err)
	}
	syncURL := fmt.Sprintf("http://127.0.0.1:%d", published)
	if err := waitReady(ctx, syncURL); err != nil {
		logs, _ := exec.Command("docker", "logs", "--tail", "40", sync).CombinedOutput()
		t.Fatalf("the sync service never became ready as the %s role: %v\n%s", electric.Role, err, logs)
	}

	// And a shape through it, which is the only assertion that covers every step at once:
	// the role, its password, its grants, its own publication and the identity.
	tenant := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO lesson (id, tenant_id, title) VALUES ($1, $2, $3)`,
		uuid.New(), tenant, "algebra",
	); err != nil {
		t.Fatal(err)
	}

	proxy, err := newProxy(electric.Config{URL: syncURL})
	if err != nil {
		t.Fatal(err)
	}

	where := &electric.Where{}
	where.Eq("tenant_id", tenant.String())

	deadline := time.Now().Add(30 * time.Second)
	for {
		rec := httptest.NewRecorder()
		proxy.Serve(rec, httptest.NewRequest(http.MethodGet, "/_stream?offset=-1", nil),
			electric.Shape{Table: "lesson", Where: where.SQL(), Params: where.Params()})

		body, _ := io.ReadAll(rec.Body)
		if rec.Code == http.StatusOK {
			var msgs []map[string]any
			if err := json.Unmarshal(body, &msgs); err == nil && rowsIn(msgs) == 1 {
				return
			}
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", "--tail", "40", sync).CombinedOutput()
			t.Fatalf("no shape came back through the %s role: %d %s\n%s",
				electric.Role, rec.Code, body, logs)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// rowsIn counts the insert messages in a shape's answer, ignoring the control message that
// ends it.
func rowsIn(msgs []map[string]any) int {
	var n int
	for _, m := range msgs {
		if _, ok := m["value"]; ok {
			n++
		}
	}
	return n
}

// goosePlainUp is the Up half of a goose migration, with the annotations removed.
//
// The migration is applied here rather than through rig/migrate because this test wants
// the SQL and not the bookkeeping: a set applied properly would need a Source, a table and
// a version, none of which is what is under test. Splitting on the Down marker is enough
// for a file this shape, and the assertion that it was enough is that the statements ran.
func goosePlainUp(sql string) string {
	up, _, _ := strings.Cut(sql, "-- +goose Down")

	var out []string
	for _, line := range strings.Split(up, "\n") {
		if strings.HasPrefix(line, "-- +goose") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
