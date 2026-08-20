package authhttp_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/authhttp"
	"github.com/simonjanss/rig/auth/authlog"
	"github.com/simonjanss/rig/auth/session"
)

// The wire shapes, spelled out rather than imported, so that a rename in
// authwire has to be a deliberate change here too.
type auditPage struct {
	Data []struct {
		ID           uuid.UUID      `json:"id"`
		At           time.Time      `json:"at"`
		Event        string         `json:"event"`
		Outcome      string         `json:"outcome"`
		AccountID    *uuid.UUID     `json:"accountId"`
		EmailAddress string         `json:"emailAddress"`
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

// grant gives the signed-in account a permission for the rest of the test.
func (f *fixture) grant(keys ...string) {
	f.grants[f.account.ID] = grants{permissions: keys}
}

// record writes an entry as though something in the foundation had.
func (f *fixture) record(e authlog.Entry) {
	if e.At.IsZero() {
		e.At = f.clock.at
	}
	if e.Event == "" {
		e.Event = authlog.EventLoginFailed
	}
	if e.Outcome == "" {
		e.Outcome = authlog.Failed
	}
	f.log.trail.Write(context.Background(), e)
}

// A project that does not hand over a reader gets no route, rather than a route
// that answers 403. There is nothing to probe, which is the same choice
// registration and tenant creation make.
func TestTheTrailIsAbsentWithoutAReader(t *testing.T) {
	t.Parallel()

	f := setup(t)
	p := f.login(t)

	srv := f.serve(t, authhttp.Config{Sessions: f.sessions})
	req, err := http.NewRequest("GET", srv.URL+"/auth/audit", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+p.AccessToken)

	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status %d, want 404: an unmounted route is absent, not forbidden", res.StatusCode)
	}
}

// Your own trail is self-service and costs no permission. It is the screen every
// product eventually wants — where have I signed in from, and did anything fail.
func TestYourOwnTrailNeedsNoPermission(t *testing.T) {
	t.Parallel()

	f := setup(t)
	p := f.login(t)

	somebodyElse := uuid.New()
	f.record(authlog.Entry{TenantID: &f.tenant, AccountID: &somebodyElse})

	res := f.do(t, "GET", "/auth/audit", p.AccessToken, "")
	if res.status != http.StatusOK {
		t.Fatalf("status %d, want 200\n%s", res.status, res.body)
	}

	var page auditPage
	res.decode(t, &page)
	if len(page.Data) == 0 {
		t.Fatal("the login that just happened should be in the caller's own trail")
	}
	for _, e := range page.Data {
		if e.AccountID == nil || *e.AccountID != f.account.ID {
			t.Errorf("own scope returned an entry for %v, want only %v", e.AccountID, f.account.ID)
		}
	}
	if page.Pagination.Total != int64(len(page.Data)) {
		t.Errorf("total %d does not match the %d rows returned", page.Pagination.Total, len(page.Data))
	}
}

// The tenant-wide trail is the permission, and the refusal is loud. Narrowing
// the answer instead would leave a caller unable to tell "you may not see that"
// from "there is nothing else".
func TestTheWideTrailNeedsThePermission(t *testing.T) {
	t.Parallel()

	f := setup(t)
	p := f.login(t)

	somebodyElse := uuid.New()
	f.record(authlog.Entry{TenantID: &f.tenant, AccountID: &somebodyElse})

	if res := f.do(t, "GET", "/auth/audit?scope=all", p.AccessToken, ""); res.status != http.StatusForbidden {
		t.Fatalf("status %d, want 403 without %s", res.status, authhttp.PermissionReadAuthLogAll)
	}

	f.grant(authhttp.PermissionReadAuthLogAll)
	res := f.do(t, "GET", "/auth/audit?scope=all", p.AccessToken, "")
	if res.status != http.StatusOK {
		t.Fatalf("status %d, want 200\n%s", res.status, res.body)
	}

	var page auditPage
	res.decode(t, &page)

	var sawTheOtherAccount bool
	for _, e := range page.Data {
		if e.AccountID != nil && *e.AccountID == somebodyElse {
			sawTheOtherAccount = true
		}
	}
	if !sawTheOtherAccount {
		t.Error("the wide trail should reach every account in the tenant")
	}
}

// The entries that resolved to no tenant are the ones a rate limit needs most,
// and no tenant has the standing to read them. Not with the permission, not by
// asking for the address they were recorded against.
func TestTheTenantlessEntriesAreInvisible(t *testing.T) {
	t.Parallel()

	f := setup(t)
	p := f.login(t)
	f.grant(authhttp.PermissionReadAuthLogAll)

	// A sign-in that named no tenant, against an address nobody has.
	f.record(authlog.Entry{EmailAddress: "nobody@example.com"})

	res := f.do(t, "GET", "/auth/audit?scope=all", p.AccessToken, "")
	if res.status != http.StatusOK {
		t.Fatalf("status %d\n%s", res.status, res.body)
	}

	var page auditPage
	res.decode(t, &page)
	for _, e := range page.Data {
		if e.EmailAddress == "nobody@example.com" {
			t.Fatal("an entry with no tenant reached a tenant's trail")
		}
	}
}

// Another tenant's rows are not reachable either, which is the same predicate
// doing the work rather than a second check.
func TestAnotherTenantsTrailIsInvisible(t *testing.T) {
	t.Parallel()

	f := setup(t)
	p := f.login(t)
	f.grant(authhttp.PermissionReadAuthLogAll)

	elsewhere, theirAccount := uuid.New(), uuid.New()
	f.record(authlog.Entry{TenantID: &elsewhere, AccountID: &theirAccount,
		EmailAddress: "them@example.com"})

	res := f.do(t, "GET", "/auth/audit?scope=all", p.AccessToken, "")
	var page auditPage
	res.decode(t, &page)

	for _, e := range page.Data {
		if e.AccountID != nil && *e.AccountID == theirAccount {
			t.Fatal("another tenant's entry reached this tenant's trail")
		}
	}
}

// Every filter refuses a value it does not understand. A misspelled event that
// answered with an empty page would read as "that never happened", and there is
// no way to tell those two apart from outside.
func TestBadFiltersAreRefused(t *testing.T) {
	t.Parallel()

	f := setup(t)
	p := f.login(t)

	for _, q := range []string{
		"?scope=world",
		"?event=DefinitelyNotAnEvent",
		"?outcome=Maybe",
		"?since=yesterday",
		"?until=soon",
		"?since=2026-03-02T00:00:00Z&until=2026-03-01T00:00:00Z",
		"?limit=lots",
		"?offset=back",
		"?accountId=not-a-uuid&scope=all",
	} {
		f.grant(authhttp.PermissionReadAuthLogAll)
		if res := f.do(t, "GET", "/auth/audit"+q, p.AccessToken, ""); res.status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400\n%s", q, res.status, res.body)
		}
	}
}

// A caller asking for their own events cannot point the filter at somebody else.
// Silently ignoring it would answer with the caller's own rows under a heading
// that says otherwise.
func TestOwnScopeCannotNameSomebodyElse(t *testing.T) {
	t.Parallel()

	f := setup(t)
	p := f.login(t)

	res := f.do(t, "GET", "/auth/audit?accountId="+uuid.NewString(), p.AccessToken, "")
	if res.status != http.StatusBadRequest {
		t.Errorf("status %d, want 400\n%s", res.status, res.body)
	}

	// Naming yourself is the same request written out, and it is fine.
	res = f.do(t, "GET", "/auth/audit?accountId="+f.account.ID.String(), p.AccessToken, "")
	if res.status != http.StatusOK {
		t.Errorf("status %d, want 200\n%s", res.status, res.body)
	}
}

// A limit is always applied and the ceiling is not negotiable. This is the table
// that grows with every login.
func TestThePageIsClamped(t *testing.T) {
	t.Parallel()

	f := setup(t)
	p := f.login(t)

	for _, tc := range []struct{ query, want string }{
		{"", "the default"},
		{"?limit=0", "the default"},
		{"?limit=-5", "the default"},
	} {
		res := f.do(t, "GET", "/auth/audit"+tc.query, p.AccessToken, "")
		var page auditPage
		res.decode(t, &page)
		if page.Pagination.Limit != 50 {
			t.Errorf("%q: limit %d, want 50 (%s)", tc.query, page.Pagination.Limit, tc.want)
		}
	}

	res := f.do(t, "GET", "/auth/audit?limit=9999", p.AccessToken, "")
	var page auditPage
	res.decode(t, &page)
	if page.Pagination.Limit != 500 {
		t.Errorf("limit %d, want the 500 ceiling", page.Pagination.Limit)
	}
}

func TestFiltersNarrow(t *testing.T) {
	t.Parallel()

	f := setup(t)
	p := f.login(t)
	mine := f.account.ID

	f.record(authlog.Entry{
		TenantID: &f.tenant, AccountID: &mine,
		Event: authlog.EventPasswordChanged, Outcome: authlog.Succeeded,
	})

	res := f.do(t, "GET", "/auth/audit?event="+authlog.EventPasswordChanged, p.AccessToken, "")
	var page auditPage
	res.decode(t, &page)

	if len(page.Data) != 1 {
		t.Fatalf("got %d entries for one event, want 1", len(page.Data))
	}
	if page.Data[0].Event != authlog.EventPasswordChanged {
		t.Errorf("event = %s", page.Data[0].Event)
	}

	// A window that ends before anything was recorded returns nothing, and says
	// so in the total rather than in an empty page with a total of five.
	res = f.do(t, "GET", "/auth/audit?until=2026-01-01T00:00:00Z", p.AccessToken, "")
	res.decode(t, &page)
	if page.Pagination.Total != 0 || len(page.Data) != 0 {
		t.Errorf("got %d of %d entries before anything happened", len(page.Data), page.Pagination.Total)
	}
}

// The session list widens across accounts and never across tenants, and the
// caller's own session stays marked in the wide answer.
func TestSessionsWiden(t *testing.T) {
	t.Parallel()

	f := setup(t)
	p := f.login(t)

	somebodyElse := uuid.New()
	if _, err := f.sessions.Issue(context.Background(), session.IssueInput{
		TenantID: f.tenant, AccountID: somebodyElse, Client: session.ClientMobile,
	}); err != nil {
		t.Fatal(err)
	}

	res := f.do(t, "GET", "/auth/sessions", p.AccessToken, "")
	var list sessionList
	res.decode(t, &list)
	if len(list.Data) != 1 {
		t.Fatalf("got %d sessions in the narrow list, want only the caller's", len(list.Data))
	}
	if list.Data[0].AccountID != f.account.ID {
		t.Error("a session should say whose it is")
	}

	if res := f.do(t, "GET", "/auth/sessions?scope=all", p.AccessToken, ""); res.status != http.StatusForbidden {
		t.Fatalf("status %d, want 403 without %s", res.status, authhttp.PermissionReadSessionsAll)
	}

	f.grant(authhttp.PermissionReadSessionsAll)
	res = f.do(t, "GET", "/auth/sessions?scope=all", p.AccessToken, "")
	res.decode(t, &list)
	if len(list.Data) != 2 {
		t.Fatalf("got %d sessions in the tenant, want 2", len(list.Data))
	}

	var current int
	for _, s := range list.Data {
		if s.Current {
			current++
		}
	}
	if current != 1 {
		t.Errorf("%d sessions marked current, want exactly the caller's own", current)
	}
}

// The whole point of this pair of answers is that they stay different for
// different reasons: 403 says the caller may not reach past their own sessions,
// 404 says nothing at all about what exists.
func TestRevokingSomebodyElsesSession(t *testing.T) {
	t.Parallel()

	f := setup(t)
	p := f.login(t)

	somebodyElse := uuid.New()
	theirs, err := f.sessions.Issue(context.Background(), session.IssueInput{
		TenantID: f.tenant, AccountID: somebodyElse, Client: session.ClientWeb,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := theirs.RootTokenID.String()

	// Narrow, so somebody else's session is a 404 — the answer an identifier
	// nobody has gets, and the reason it cannot be probed.
	if res := f.do(t, "DELETE", "/auth/sessions/"+id, p.AccessToken, ""); res.status != http.StatusNotFound {
		t.Fatalf("status %d, want 404 for a session that is not the caller's", res.status)
	}

	// Wide without the permission is a 403, and it is refused before the
	// identifier is looked at, so it says nothing about that session either.
	if res := f.do(t, "DELETE", "/auth/sessions/"+id+"?scope=all", p.AccessToken, ""); res.status != http.StatusForbidden {
		t.Fatalf("status %d, want 403 without %s", res.status, authhttp.PermissionRevokeSessionsAll)
	}

	f.grant(authhttp.PermissionRevokeSessionsAll)

	// Held, and the identifier is invented: still a 404. Held, and the session
	// is real: it ends.
	if res := f.do(t, "DELETE", "/auth/sessions/"+uuid.NewString()+"?scope=all", p.AccessToken, ""); res.status != http.StatusNotFound {
		t.Errorf("status %d, want 404 for an identifier nobody has", res.status)
	}
	if res := f.do(t, "DELETE", "/auth/sessions/"+id+"?scope=all", p.AccessToken, ""); res.status != http.StatusNoContent {
		t.Fatalf("status %d, want 204\n%s", res.status, res.body)
	}

	// It is gone, and the trail says who ended it. A logout that records no
	// actor is the one thing an administrator's revocation must not look like.
	if _, err := f.sessions.Verify(context.Background(), theirs.Access.Token); err == nil {
		t.Error("the revoked session still verifies")
	}

	entry, ok := f.log.last(authlog.EventLogout)
	if !ok {
		t.Fatal("revoking a session should be recorded")
	}
	if entry.Detail["revokedBy"] != f.account.ID.String() {
		t.Errorf("revokedBy = %v, want the administrator %s", entry.Detail["revokedBy"], f.account.ID)
	}
	if entry.TenantID == nil || *entry.TenantID != f.tenant {
		t.Error("a logout with no tenant is invisible to the tenant's own trail")
	}
	if entry.AccountID == nil || *entry.AccountID != somebodyElse {
		t.Error("the entry should name whose session ended, not who ended it")
	}
}

// Ending your own session is not an administrative act and records no actor: a
// revokedBy on every logout would be a field a reader has to compare before it
// means anything.
func TestEndingYourOwnSessionNamesNobody(t *testing.T) {
	t.Parallel()

	f := setup(t)
	p := f.login(t)

	if res := f.do(t, "DELETE", "/auth/sessions/"+p.SessionID.String(), p.AccessToken, ""); res.status != http.StatusNoContent {
		t.Fatalf("status %d, want 204", res.status)
	}

	entry, ok := f.log.last(authlog.EventLogout)
	if !ok {
		t.Fatal("ending a session should be recorded")
	}
	if _, named := entry.Detail["revokedBy"]; named {
		t.Errorf("detail = %v, want no actor for somebody ending their own", entry.Detail)
	}
}
