//go:build docker

package main

import (
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/examples/auth/services/note"
	"github.com/simonjanss/rig/notify"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// The claim this whole design is arranged around: the audience is computed when
// a notification is sent, not when it was written.
//
// A note scheduled for later notifies whoever can read it later, and somebody
// who joined in between is one of those people. Asserted here beside its
// inverse, because the pair is the point — an account that was gone by the time
// it resolved is not told, and a design that got only the first half right would
// be a design that never resolved anything twice.
func TestTheAudienceIsComputedWhenTheNotificationIsSent(t *testing.T) {
	api := newServer(t)
	tenant := api.seed(t)

	author := api.accountID(t, tenant, SeedEmail)
	ctx := tenancy.NewContext(context.Background(),
		tenancy.Claims{TenantID: tenant, AccountID: author, Subject: tenancy.SubjectAccount})

	engine, notifier := api.notifications(t)

	// A note published in the past, so it is due the moment it is written.
	published := time.Now().Add(-time.Minute).UTC()
	noteID := api.note(t, ctx, "The office is closed on Friday", &published)

	// Somebody who did not exist when the note was written.
	latecomer := api.addAccount(t, tenant, "latecomer")

	report, err := engine.Resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if report.Resolved == 0 {
		t.Fatalf("nothing was resolved: %s", report)
	}
	if report.Empty > 0 {
		// The silent failure mode of this design, and the reason the count
		// exists: an audience of nobody looks exactly like a notification
		// nobody was owed.
		t.Errorf("a resolution found nobody to tell: %s", report)
	}

	if !api.wasTold(t, tenant, latecomer, noteID) {
		t.Error("an account created after the note was written should still be told: " +
			"the audience is built when the notification is sent")
	}
	// The author is deliberately not: NotifyWho excludes whoever caused the
	// change, which is an application's decision and exactly the kind a path
	// expression could not have carried.
	if api.wasTold(t, tenant, author, noteID) {
		t.Error("the author caused the change and should not be told about it")
	}

	// And the inverse. An account that is gone by the time a second
	// announcement resolves does not hear about it.
	api.deactivate(t, latecomer)
	second := api.note(t, ctx, "The office is closed on Monday too", &published)
	if _, err := engine.Resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.wasTold(t, tenant, latecomer, second) {
		t.Error("an account that left before the resolve should not be told")
	}

	_ = notifier
}

// The arithmetic every application gets subtly wrong, over a real database.
func TestTheInboxCollapsesAndRecovers(t *testing.T) {
	api := newServer(t)
	tenant := api.seed(t)

	author := api.accountID(t, tenant, SeedEmail)
	reader := api.addAccount(t, tenant, "reader")
	ctx := tenancy.NewContext(context.Background(),
		tenancy.Claims{TenantID: tenant, AccountID: author, Subject: tenancy.SubjectAccount})

	engine, _ := api.notifications(t)
	published := time.Now().Add(-time.Minute).UTC()

	t.Run("a fan-out run twice writes one line", func(t *testing.T) {
		id := api.note(t, ctx, "Once", &published)
		if _, err := engine.Resolve(context.Background()); err != nil {
			t.Fatal(err)
		}
		// Resolving again is what a dispatcher that died before committing
		// does, and what two replicas racing the same nudge do. The unique
		// index absorbs it, which is what lets NotifyWho be documented as a
		// pure read that may be called again.
		api.reopen(t, id)
		if _, err := engine.Resolve(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := api.lineCount(t, tenant, reader, id); got != 1 {
			t.Errorf("%d inbox lines for one notification, want 1", got)
		}
	})

	t.Run("several events about one note are one line saying how many", func(t *testing.T) {
		id := api.note(t, ctx, "Again and again", &published)
		for range 3 {
			api.touch(t, ctx, id)
		}
		if _, err := engine.Resolve(context.Background()); err != nil {
			t.Fatal(err)
		}

		count, events := api.linesFor(t, tenant, reader, id)
		if count != 1 {
			t.Errorf("%d lines for one note, want 1: they should have collapsed", count)
		}
		if events < 2 {
			t.Errorf("event count = %d; several events should have joined one line", events)
		}
	})
}

// The routes, and the one thing they must never do.
func TestTheInboxRoutesAnswerOnlyTheCaller(t *testing.T) {
	api := newServer(t)
	tenant := api.seed(t)

	author := api.accountID(t, tenant, SeedEmail)
	reader := api.addAccount(t, tenant, "reader2")
	readerEmail := api.emailOf(t, reader)
	api.setPassword(t, readerEmail, SeedPassword)
	api.grantEverything(t, tenant, reader)

	ctx := tenancy.NewContext(context.Background(),
		tenancy.Claims{TenantID: tenant, AccountID: author, Subject: tenancy.SubjectAccount})
	engine, _ := api.notifications(t)

	published := time.Now().Add(-time.Minute).UTC()
	api.note(t, ctx, "Something to be told about", &published)
	if _, err := engine.Resolve(context.Background()); err != nil {
		t.Fatal(err)
	}

	readerToken := api.login(t, tenant, readerEmail, SeedPassword)
	ownerToken := api.login(t, tenant, SeedEmail, SeedPassword)

	t.Run("the badge counts what is unread", func(t *testing.T) {
		res := api.do(t, request{method: http.MethodGet, path: "/notifications/_unread-count",
			tenant: tenant, token: readerToken})
		if res.status != http.StatusOK {
			t.Fatalf("status %d: %s", res.status, res.body)
		}
		var body struct{ Unread int }
		res.decode(t, &body)
		if body.Unread == 0 {
			t.Error("the reader was told something and the badge says nothing")
		}
	})

	t.Run("the author who caused it has an empty inbox", func(t *testing.T) {
		res := api.do(t, request{method: http.MethodGet, path: "/notifications/_unread-count",
			tenant: tenant, token: ownerToken})
		var body struct{ Unread int }
		res.decode(t, &body)
		if body.Unread != 0 {
			t.Errorf("unread = %d for the account that was told nothing", body.Unread)
		}
	})

	t.Run("marking all read clears the badge and nobody else's", func(t *testing.T) {
		res := api.do(t, request{method: http.MethodPost, path: "/notifications/_read-all",
			tenant: tenant, token: readerToken})
		if res.status != http.StatusOK {
			t.Fatalf("status %d: %s", res.status, res.body)
		}

		res = api.do(t, request{method: http.MethodGet, path: "/notifications/_unread-count",
			tenant: tenant, token: readerToken})
		var body struct{ Unread int }
		res.decode(t, &body)
		if body.Unread != 0 {
			t.Errorf("unread = %d after marking all read", body.Unread)
		}
	})

	t.Run("an inbox needs an account, and a credential without one is refused", func(t *testing.T) {
		// Not narrowed to nothing, which is the wrong kind of correct: an empty
		// inbox and an inbox nobody could have looked at read the same.
		svc := notify.NewService(notify.Config{DB: api.pool})
		system := tenancy.NewContext(context.Background(), tenancy.System(tenant))
		if _, err := svc.UnreadCount(system); err == nil {
			t.Error("a credential with no account should be refused, not answered with zero")
		}
	})
}

// A note that goes away takes its notifications with it, which is the failure
// mode — "somebody commented on ⟨deleted⟩" — that the link table alone does not
// fix.
func TestDeletingANoteTakesItsNotifications(t *testing.T) {
	api := newServer(t)
	tenant := api.seed(t)

	author := api.accountID(t, tenant, SeedEmail)
	reader := api.addAccount(t, tenant, "reader3")
	ctx := tenancy.NewContext(context.Background(),
		tenancy.Claims{TenantID: tenant, AccountID: author, Subject: tenancy.SubjectAccount})

	engine, _ := api.notifications(t)
	published := time.Now().Add(-time.Minute).UTC()

	id := api.note(t, ctx, "Briefly true", &published)
	if _, err := engine.Resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.lineCount(t, tenant, reader, id) == 0 {
		t.Fatal("the fixture should have produced an inbox line")
	}

	// A hard delete, which the link row's foreign key would refuse on 23503
	// until something cleared it. Nothing here clears it by hand: that is
	// generated into the writer and runs inside this transaction.
	api.hardDelete(t, ctx, id)

	if got := api.lineCount(t, tenant, reader, id); got != 0 {
		t.Errorf("%d inbox lines survive a note that does not, want 0", got)
	}
}

// notifications builds the engine the same way main does.
func (s *server) notifications(t *testing.T) (*notify.Engine, *notify.Service) {
	t.Helper()
	_, engine, err := newAPI(context.Background(), s.pool, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	return engine, nil
}

// note writes one through the service layer, which is what runs the hooks.
func (s *server) note(t *testing.T, ctx context.Context, title string, publishAt *time.Time) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO note (id, tenant_id, title, publish_at, created_by_account_id)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		uuid.New(), notifyTenantOf(t, ctx), title, publishAt, notifyAccountOf(t, ctx)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	s.announce(t, ctx, id)
	return id
}

// announce says a note happened, through the same service the hook calls.
func (s *server) announce(t *testing.T, ctx context.Context, id uuid.UUID) {
	t.Helper()

	svc := notify.NewService(notify.Config{DB: s.pool, Registry: s.registry(t)})
	if _, err := svc.Announce(ctx, notify.Announcement{
		Kind:    note.KindNotePublished,
		Subject: subjectOf(id),
		// Due in the past, which is what the note's own NotifyAt would have
		// answered for a note whose publish_at has been and gone.
		At:    time.Now().Add(-time.Minute).UTC(),
		Due:   true,
		Group: notify.GroupBy("note:" + id.String()),
	}); err != nil {
		t.Fatal(err)
	}
}

func (s *server) touch(t *testing.T, ctx context.Context, id uuid.UUID) {
	t.Helper()
	s.announce(t, ctx, id)
}

// reopen puts a resolved notification back to pending, which is what a
// dispatcher that died before committing leaves behind.
func (s *server) reopen(t *testing.T, noteID uuid.UUID) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), `
		UPDATE rig_notification SET state = 'Pending', resolved_at = NULL
		WHERE id IN (SELECT notification_id FROM note_notification WHERE note_id = $1)`,
		noteID); err != nil {
		t.Fatal(err)
	}
}

func (s *server) wasTold(t *testing.T, tenant, account, noteID uuid.UUID) bool {
	t.Helper()
	return s.lineCount(t, tenant, account, noteID) > 0
}

func (s *server) lineCount(t *testing.T, tenant, account, noteID uuid.UUID) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM rig_notification_recipient r
		JOIN note_notification l ON l.notification_id = r.notification_id
		WHERE r.tenant_id = $1 AND r.account_id = $2 AND l.note_id = $3 AND r.deleted_at IS NULL`,
		tenant, account, noteID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (s *server) linesFor(t *testing.T, tenant, account, noteID uuid.UUID) (lines, events int) {
	t.Helper()
	if err := s.pool.QueryRow(context.Background(), `
		SELECT count(*), coalesce(max(r.event_count), 0)
		FROM rig_notification_recipient r
		JOIN note_notification l ON l.notification_id = r.notification_id
		WHERE r.tenant_id = $1 AND r.account_id = $2 AND l.note_id = $3 AND r.deleted_at IS NULL`,
		tenant, account, noteID).Scan(&lines, &events); err != nil {
		t.Fatal(err)
	}
	return lines, events
}

// addAccount makes somebody new, with an address nobody has had before.
//
// Unique per run rather than fixed, because this database is not thrown away
// between runs and the point of several of these tests is that an account
// appeared at a particular moment.
func (s *server) addAccount(t *testing.T, tenant uuid.UUID, name string) uuid.UUID {
	t.Helper()
	email := name + "+" + uuid.NewString()[:8] + "@example.test"

	var identityID uuid.UUID
	if err := s.pool.QueryRow(context.Background(), `
		INSERT INTO rig_identity (id, email_address, display_name)
		VALUES ($1, $2, $2) RETURNING id`, uuid.New(), email).Scan(&identityID); err != nil {
		t.Fatal(err)
	}

	var id uuid.UUID
	if err := s.pool.QueryRow(context.Background(), `
		INSERT INTO rig_account (id, tenant_id, identity_id, email_address, display_name)
		VALUES ($1, $2, $3, $4, $4) RETURNING id`,
		uuid.New(), tenant, identityID, email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (s *server) emailOf(t *testing.T, account uuid.UUID) string {
	t.Helper()
	var email string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT email_address FROM rig_account WHERE id = $1`, account).Scan(&email); err != nil {
		t.Fatal(err)
	}
	return email
}

func (s *server) deactivate(t *testing.T, account uuid.UUID) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE rig_account SET is_active = false WHERE id = $1`, account); err != nil {
		t.Fatal(err)
	}
}

func (s *server) hardDelete(t *testing.T, ctx context.Context, id uuid.UUID) {
	t.Helper()

	svc := notify.NewService(notify.Config{DB: s.pool})
	if err := svc.Deleted(ctx, subjectOf(id)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM note WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
}

func (s *server) registry(t *testing.T) *notify.Registry {
	t.Helper()
	return notify.NewRegistry()
}

func subjectOf(id uuid.UUID) notify.Subject {
	return notify.Subject{
		Table:     "note",
		LinkTable: "note_notification",
		Column:    "note_id",
		ID:        id,
	}
}

func notifyTenantOf(t *testing.T, ctx context.Context) uuid.UUID {
	t.Helper()
	claims, err := tenancy.FromContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return claims.TenantID
}

func notifyAccountOf(t *testing.T, ctx context.Context) uuid.UUID {
	t.Helper()
	claims, err := tenancy.FromContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return claims.AccountID
}
