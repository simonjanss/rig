//go:build docker

package authtest

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/authhttp"
	"github.com/simonjanss/rig/auth/authlog"
	"github.com/simonjanss/rig/auth/session"
)

// The wire shapes, decoded the way a client would.
type auditPage struct {
	Data []struct {
		ID           uuid.UUID      `json:"id"`
		At           time.Time      `json:"at"`
		Event        string         `json:"event"`
		Outcome      string         `json:"outcome"`
		AccountID    *uuid.UUID     `json:"accountId"`
		EmailAddress string         `json:"emailAddress"`
		IPAddress    string         `json:"ipAddress"`
		SessionID    *uuid.UUID     `json:"sessionId"`
		Detail       map[string]any `json:"detail"`
	} `json:"data"`
	Pagination struct {
		Offset int   `json:"offset"`
		Limit  int   `json:"limit"`
		Total  int64 `json:"total"`
	} `json:"pagination"`
}

type sessionList struct {
	Data []struct {
		ID        uuid.UUID `json:"id"`
		AccountID uuid.UUID `json:"accountId"`
		Current   bool      `json:"current"`
	} `json:"data"`
}

// colleague adds a second person to the harness's tenant and returns their
// account.
//
// Two rows, because one identity may hold only one account per tenant —
// rig_account_tenant_identity_key — so a second account here is a second person
// rather than the same person twice.
func colleague(t *testing.T, h *harness) uuid.UUID {
	t.Helper()

	identity, account := uuid.New(), uuid.New()
	email := "colleague-" + uuid.NewString()[:8] + "@example.com"
	ctx := context.Background()

	if _, err := h.pool.Exec(ctx, `
		INSERT INTO rig_identity (id, email_address, display_name)
		VALUES ($1, $2, $3)`, identity, email, "Colleague"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO rig_account (id, tenant_id, identity_id, email_address, display_name)
		VALUES ($1, $2, $3, $4, $5)`,
		account, h.tenant, identity, email, "Colleague"); err != nil {
		t.Fatal(err)
	}
	return account
}

// The predicate is one line of SQL and everything about this endpoint's safety
// rests on it, so it is asserted against a real database rather than against a
// slice filtered in Go.
func TestTheTrailIsScopedToTheTenantOverRealSQL(t *testing.T) {
	a, b := setup(t), setup(t)

	pa, pb := a.login(t), b.login(t)
	grant(t, a, authhttp.PermissionReadAuthLogAll)

	res := a.do(t, "GET", "/auth/audit?scope=all", pa.AccessToken, "")
	if res.status != http.StatusOK {
		t.Fatalf("status %d\n%s", res.status, res.body)
	}

	var page auditPage
	res.decode(t, &page)
	if len(page.Data) == 0 {
		t.Fatal("the login that just happened should be in the tenant's trail")
	}
	for _, e := range page.Data {
		if e.AccountID != nil && *e.AccountID == b.account {
			t.Fatal("tenant A read tenant B's entries")
		}
		if e.EmailAddress == b.email {
			t.Fatal("tenant A read an address belonging to tenant B")
		}
	}

	// And the other way round, so this is isolation rather than an ordering
	// accident. B holds nothing, so its own trail is all it can ask for.
	res = b.do(t, "GET", "/auth/audit", pb.AccessToken, "")
	res.decode(t, &page)
	for _, e := range page.Data {
		if e.AccountID == nil || *e.AccountID != b.account {
			t.Fatalf("B's own trail contains an entry for %v", e.AccountID)
		}
	}
}

// The rows with no tenant are the ones the lockout counts, and no tenant has the
// standing to read them. This is the case the migration's comment is about, and
// the reason `--expose rig_auth_log` and this endpoint are different answers.
func TestTheTenantlessRowsAreInvisibleOverRealSQL(t *testing.T) {
	h := setup(t)
	p := h.login(t)
	grant(t, h, authhttp.PermissionReadAuthLogAll)

	// A sign-in that named no tenant, against an address nobody has: exactly
	// what the rate limiter needs and what nobody may read.
	stranger := "nobody-" + uuid.NewString()[:8] + "@example.com"
	res := h.doUnscoped(t, "POST", "/auth/login", "",
		fmt.Sprintf(`{"emailAddress":%q,"password":"whatever"}`, stranger))
	if res.status == http.StatusOK {
		t.Fatal("signing in as a stranger should not work")
	}

	// It was recorded, with no tenant on it.
	var tenantless int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM rig_auth_log WHERE tenant_id IS NULL AND email_address = $1`,
		stranger).Scan(&tenantless); err != nil {
		t.Fatal(err)
	}
	if tenantless == 0 {
		t.Fatal("an attempt that resolved to no tenant should still be recorded")
	}

	// And it is not readable, by scope or by filter.
	for _, path := range []string{
		"/auth/audit?scope=all",
		"/auth/audit?scope=all&event=" + authlog.EventLoginFailed,
		"/auth/audit",
	} {
		res := h.do(t, "GET", path, p.AccessToken, "")
		var page auditPage
		res.decode(t, &page)
		for _, e := range page.Data {
			if e.EmailAddress == stranger {
				t.Fatalf("%s reached a row with no tenant", path)
			}
		}
	}
}

// Own scope needs no permission and reaches exactly one account's events.
func TestYourOwnTrailOverRealSQL(t *testing.T) {
	h := setup(t)
	p := h.login(t)

	// A second account in the same tenant, with an event of its own.
	other := colleague(t, h)
	if _, err := h.sessions.Issue(context.Background(), session.IssueInput{
		TenantID: h.tenant, AccountID: other, Client: session.ClientWeb,
	}); err != nil {
		t.Fatal(err)
	}

	res := h.do(t, "GET", "/auth/audit", p.AccessToken, "")
	if res.status != http.StatusOK {
		t.Fatalf("status %d, want 200 with no permission held\n%s", res.status, res.body)
	}

	var page auditPage
	res.decode(t, &page)
	if len(page.Data) == 0 {
		t.Fatal("the caller's own login should be there")
	}
	for _, e := range page.Data {
		if e.AccountID == nil || *e.AccountID != h.account {
			t.Errorf("own scope returned an entry for %v", e.AccountID)
		}
	}
}

// The two refusals stay different for different reasons, and this is the
// assertion that says so: side by side, in one test, so that a change which
// collapses them into one answer fails here.
func TestForbiddenAndNotFoundStayDifferentOverRealSQL(t *testing.T) {
	h := setup(t)
	p := h.login(t)

	// Somebody else's session, in the same tenant.
	other := colleague(t, h)
	theirs, err := h.sessions.Issue(context.Background(), session.IssueInput{
		TenantID: h.tenant, AccountID: other, Client: session.ClientWeb,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := theirs.RootTokenID.String()

	// Asking to read the whole tenant without the key.
	if res := h.do(t, "GET", "/auth/audit?scope=all", p.AccessToken, ""); res.status != http.StatusForbidden {
		t.Errorf("audit scope=all: status %d, want 403", res.status)
	}
	if res := h.do(t, "GET", "/auth/sessions?scope=all", p.AccessToken, ""); res.status != http.StatusForbidden {
		t.Errorf("sessions scope=all: status %d, want 403", res.status)
	}
	// Ending somebody else's without it: still 403, and it says nothing about
	// whether that session is real, because it is refused before the lookup.
	if res := h.do(t, "DELETE", "/auth/sessions/"+id+"?scope=all", p.AccessToken, ""); res.status != http.StatusForbidden {
		t.Errorf("revoke scope=all: status %d, want 403", res.status)
	}
	// Narrow, so somebody else's session is a 404 — the same answer an
	// identifier nobody has gets.
	if res := h.do(t, "DELETE", "/auth/sessions/"+id, p.AccessToken, ""); res.status != http.StatusNotFound {
		t.Errorf("revoke own scope: status %d, want 404", res.status)
	}

	grant(t, h, authhttp.PermissionRevokeSessionsAll)

	// Held, and the identifier is invented: 404, indistinguishable from the
	// answer for a real session in another tenant.
	if res := h.do(t, "DELETE", "/auth/sessions/"+uuid.NewString()+"?scope=all", p.AccessToken, ""); res.status != http.StatusNotFound {
		t.Errorf("invented identifier: status %d, want 404", res.status)
	}

	elsewhere := setup(t)
	pe := elsewhere.login(t)
	if res := h.do(t, "DELETE", "/auth/sessions/"+pe.SessionID.String()+"?scope=all",
		p.AccessToken, ""); res.status != http.StatusNotFound {
		t.Errorf("another tenant's session: status %d, want 404", res.status)
	}
	if !elsewhere.authenticated(t, pe.AccessToken) {
		t.Error("the other tenant's session should be untouched")
	}
}

// An administrator ending somebody's session kills the family and leaves an
// entry naming who did it — the one question asked about a session that ended
// without its holder doing anything.
func TestRevokingAnotherAccountsSessionOverRealSQL(t *testing.T) {
	h := setup(t)
	p := h.login(t)
	grant(t, h, authhttp.PermissionRevokeSessionsAll)
	grant(t, h, authhttp.PermissionReadSessionsAll)

	other := colleague(t, h)
	theirs, err := h.sessions.Issue(context.Background(), session.IssueInput{
		TenantID: h.tenant, AccountID: other, Client: session.ClientMobile,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The wide list sees it, and says whose it is.
	res := h.do(t, "GET", "/auth/sessions?scope=all", p.AccessToken, "")
	var list sessionList
	res.decode(t, &list)

	var found bool
	for _, s := range list.Data {
		if s.ID == theirs.RootTokenID {
			found = true
			if s.AccountID != other {
				t.Errorf("session says accountId %v, want %v", s.AccountID, other)
			}
			if s.Current {
				t.Error("somebody else's session should not be marked current")
			}
		}
	}
	if !found {
		t.Fatal("the wide list should include another account's session")
	}

	if res := h.do(t, "DELETE", "/auth/sessions/"+theirs.RootTokenID.String()+"?scope=all",
		p.AccessToken, ""); res.status != http.StatusNoContent {
		t.Fatalf("status %d, want 204\n%s", res.status, res.body)
	}

	// The whole family is dead, not only the root.
	if h.authenticated(t, theirs.Access.Token) {
		t.Error("the revoked session still authenticates")
	}
	var live int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM rig_account_token WHERE root_token_id = $1 AND revoked_at IS NULL`,
		theirs.RootTokenID).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Errorf("%d tokens of the family are still live", live)
	}

	// And the entry is readable, which is the part that was broken as long as
	// nothing read this table: a Logout stamped with no tenant is invisible to
	// the tenant's own trail.
	grant(t, h, authhttp.PermissionReadAuthLogAll)
	res = h.do(t, "GET", "/auth/audit?scope=all&event="+authlog.EventLogout, p.AccessToken, "")

	var page auditPage
	res.decode(t, &page)
	if len(page.Data) == 0 {
		t.Fatal("the revocation should be in the tenant's trail")
	}

	var named bool
	for _, e := range page.Data {
		if e.Detail["revokedBy"] == h.account.String() {
			named = true
			if e.AccountID == nil || *e.AccountID != other {
				t.Errorf("the entry names %v as the subject, want %v", e.AccountID, other)
			}
		}
	}
	if !named {
		t.Error("the entry should say who ended the session")
	}
}

// Two pages of one query are disjoint and cover everything, which is a claim
// about ORDER BY and not about Go: created_at is not unique here — a login
// writes its own entry and the session's in the same instant — so the identifier
// breaks the tie.
func TestPagingTheTrailOverRealSQL(t *testing.T) {
	h := setup(t)
	p := h.login(t)

	// Enough entries to page through, all in one instant on purpose.
	at := time.Now().UTC()
	for range 7 {
		h.stores.Log.Write(context.Background(), authlog.Entry{
			At: at, Event: authlog.EventLoginFailed, Outcome: authlog.Failed,
			TenantID: &h.tenant, AccountID: &h.account,
			EmailAddress: h.email,
		})
	}

	seen := map[uuid.UUID]bool{}
	var total int64
	for offset := 0; ; offset += 3 {
		res := h.do(t, "GET",
			fmt.Sprintf("/auth/audit?limit=3&offset=%d", offset), p.AccessToken, "")
		if res.status != http.StatusOK {
			t.Fatalf("status %d\n%s", res.status, res.body)
		}

		var page auditPage
		res.decode(t, &page)
		total = page.Pagination.Total

		if page.Pagination.Offset != offset || page.Pagination.Limit != 3 {
			t.Fatalf("pagination = %+v, want offset %d limit 3", page.Pagination, offset)
		}
		for _, e := range page.Data {
			if seen[e.ID] {
				t.Fatalf("entry %s appeared on two pages", e.ID)
			}
			seen[e.ID] = true
		}
		if len(page.Data) < 3 {
			break
		}
	}

	if int64(len(seen)) != total {
		t.Errorf("walked %d entries, and the total said %d", len(seen), total)
	}
	if total < 7 {
		t.Errorf("total = %d, want at least the seven written", total)
	}
}

// Retention deletes what it should and keeps what a limit still counts.
//
// The second half is the one that matters. This table is what the lockout counts
// from, so a prune reaching inside a limit's window would clear a lockout by
// deleting the failures behind it — and the limiter would go on answering
// "allowed" with nothing to say it had stopped working.
func TestPruningTheTrailOverRealSQL(t *testing.T) {
	h := setup(t)
	ctx := context.Background()

	// Two entries: one old enough to go, one inside the hour the password-reset
	// limit reaches back.
	now := time.Now().UTC()
	old, recent := now.Add(-90*24*time.Hour), now.Add(-30*time.Minute)
	for _, at := range []time.Time{old, recent} {
		h.stores.Log.Write(ctx, authlog.Entry{
			At: at, Event: authlog.EventLoginFailed, Outcome: authlog.Failed,
			TenantID: &h.tenant, AccountID: &h.account, EmailAddress: h.email,
		})
	}

	before := h.events(t, authlog.EventLoginFailed)
	if before < 2 {
		t.Fatalf("wrote two entries and the table has %d", before)
	}

	// A 60-day window: clear of every limit, and older than the first entry.
	gone, err := h.stores.Log.Prune(ctx, now.Add(-60*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if gone != 1 {
		t.Errorf("pruned %d entries, want the one older than the window", gone)
	}

	// The recent one is still there, which is what keeps the limit countable.
	var kept int
	if err := h.pool.QueryRow(ctx, `
		SELECT count(*) FROM rig_auth_log
		 WHERE tenant_id = $1 AND created_at > $2`, h.tenant, now.Add(-time.Hour)).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept == 0 {
		t.Error("pruning removed an entry inside a rate limit's window, so a lockout would have been cleared")
	}

	// Nothing to do is not a failure, and it reports nothing done.
	gone, err = h.stores.Log.Prune(ctx, now.Add(-100*365*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if gone != 0 {
		t.Errorf("pruned %d entries older than a century ago", gone)
	}
}

// An instant leaves the database in UTC, which matters here because these are
// rendered straight into JSON: a process in another zone would otherwise report
// the whole trail shifted.
func TestTheTrailIsUTC(t *testing.T) {
	h := setup(t)
	p := h.login(t)

	res := h.do(t, "GET", "/auth/audit", p.AccessToken, "")
	var page auditPage
	res.decode(t, &page)

	if len(page.Data) == 0 {
		t.Fatal("nothing to check")
	}
	for _, e := range page.Data {
		if name, offset := e.At.Zone(); offset != 0 {
			t.Errorf("entry %s is at %s (%s, offset %d), want UTC", e.ID, e.At, name, offset)
		}
	}
}
