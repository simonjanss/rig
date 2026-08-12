//go:build docker

package authtest

import (
	"context"
	"slices"
	"testing"

	"github.com/simonjanss/rig/auth/authlog"
)

// Every event the Go code can write has to be a value the column can hold.
//
// This exists because the two drifted and nothing noticed. `Log.Write` swallows
// its error on purpose — an entry describing a failed login must not fail the
// request it describes — so an event missing from the enum is not an error
// anywhere: it is a row that silently never appears, discovered weeks later by
// somebody asking why the audit trail has a hole in it.
//
// Reading the live type rather than the SQL text is the point. It is the database
// the application will actually talk to.
func TestEveryAuthEventIsInTheEnum(t *testing.T) {
	pool := database(t)

	rows, err := pool.Query(context.Background(), `
		SELECT enumlabel FROM pg_enum
		  JOIN pg_type ON pg_type.oid = pg_enum.enumtypid
		 WHERE pg_type.typname = 'auth_event'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var labels []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			t.Fatal(err)
		}
		labels = append(labels, label)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(labels) == 0 {
		t.Fatal("no auth_event type in the database")
	}

	for _, event := range authlog.Events() {
		if !slices.Contains(labels, event) {
			t.Errorf("authlog can write %q and the auth_event enum has no such value; "+
				"every entry with it is silently dropped", event)
		}
	}

	// And the other direction, which is only untidiness but is worth knowing: a
	// value nothing writes is a value somebody added and then did not use.
	for _, label := range labels {
		if !slices.Contains(authlog.Events(), label) {
			t.Errorf("the enum has %q and no code writes it", label)
		}
	}
}
