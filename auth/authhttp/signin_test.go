package authhttp_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/oauth"
	"github.com/simonjanss/rig/runtime/authwire"
	"github.com/simonjanss/rig/runtime/rigerr"
)

// signIn calls the default OnSignIn the way oauth's callback does: directly,
// with a resolved link and whatever the flow could say about a tenant.
func (f *fixture) signIn(t *testing.T, in oauth.SignIn) (*httptest.ResponseRecorder, error) {
	t.Helper()

	if in.Link == nil {
		in.Link = &oauth.Link{ID: uuid.New(), IdentityID: f.identity.ID, Provider: "Google"}
	}
	if in.Provider == "" {
		in.Provider = "Google"
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/auth/oauth/google/callback", nil)
	r.Header.Set("User-Agent", "Mozilla/5.0")
	return w, f.handler.SignIn(w, r, in)
}

func decodeSignIn(t *testing.T, w *httptest.ResponseRecorder) authwire.SignInResponse {
	t.Helper()

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var out authwire.SignInResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", w.Body, err)
	}
	return out
}

// The sign-in a header-based deployment could not have. The tenant was not
// known before the redirect and is not knowable at the callback, so it is
// answered here from the person's own memberships — which is what a password
// login has always done.
func TestTheDefaultProviderSignInAnswersLikeALogin(t *testing.T) {
	t.Parallel()

	f := setup(t)
	w, err := f.signIn(t, oauth.SignIn{})
	if err != nil {
		t.Fatal(err)
	}
	out := decodeSignIn(t, w)

	// Every field the old body carried, which is what makes this a superset
	// rather than a different shape.
	switch {
	case out.AccessToken == "":
		t.Error("no access token")
	case out.RefreshToken == "":
		t.Error("no refresh token")
	case out.ExpiresAt.IsZero() || out.RefreshExpiresAt.IsZero():
		t.Error("no expiry")
	case out.SessionID == uuid.Nil:
		t.Error("no session id")
	}

	// And the three it did not: this is the half a provider sign-in used to have
	// no way to answer.
	if out.IdentityToken == "" {
		t.Error("no identity token, so there is nothing for a picker to run on")
	}
	if len(out.Tenants) != 1 {
		t.Fatalf("%d tenants, want 1", len(out.Tenants))
	}
	if out.Tenants[0].TenantID != f.tenant || !out.Tenants[0].Current {
		t.Errorf("tenant %v, want %s marked current", out.Tenants[0], f.tenant)
	}
}

// Nowhere to be, which is a 200 and not a 403. Somebody with an invitation
// waiting is the case, and refusing it made the flow impossible.
func TestTheDefaultProviderSignInForSomebodyWithNoTenant(t *testing.T) {
	t.Parallel()

	f := setup(t)
	f.account.IsActive = false
	f.store.Put(f.account)

	w, err := f.signIn(t, oauth.SignIn{})
	if err != nil {
		t.Fatal(err)
	}
	out := decodeSignIn(t, w)

	if out.AccessToken != "" || out.SessionID != uuid.Nil {
		t.Error("there was nowhere to land, so there is no session to hand out")
	}
	if out.IdentityToken == "" {
		t.Fatal("the identity token is the whole answer here")
	}
	if out.Tenants == nil {
		t.Error("an empty list, not a null: a client draws a picker from it")
	}
}

// A tenant that was named — a host per tenant — and the person is not in it.
// The error is returned rather than written, because oauth's own handler renders
// it: that is the contract OnSignIn has.
func TestTheDefaultProviderSignInRefusesATenantYouAreNotIn(t *testing.T) {
	t.Parallel()

	f := setup(t)
	w, err := f.signIn(t, oauth.SignIn{TenantID: uuid.New(), AccountID: uuid.New()})
	if rigerr.CodeOf(err) != rigerr.CodeForbidden {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if w.Body.Len() != 0 {
		t.Errorf("it wrote a body as well: %s", w.Body)
	}
}

// The cheap "the default did not move" check. A deployment that names its tenant
// and has one to name gets what it got before, plus fields.
func TestTheDefaultProviderSignInIsUnchangedForANamedTenant(t *testing.T) {
	t.Parallel()

	f := setup(t)
	w, err := f.signIn(t, oauth.SignIn{TenantID: f.tenant, AccountID: f.account.ID})
	if err != nil {
		t.Fatal(err)
	}
	out := decodeSignIn(t, w)

	tok, err := f.sessions.Verify(t.Context(), out.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if tok.TenantID != f.tenant || tok.AccountID != f.account.ID {
		t.Errorf("session is for %s/%s, want %s/%s",
			tok.TenantID, tok.AccountID, f.tenant, f.account.ID)
	}
}

// The remember-me box a provider sign-in has nowhere to draw.
//
// It is asked for with `?remember=` on the start route and carried across the
// round trip in the sealed state cookie, so by the time the ending runs it is
// an ordinary field. What it buys is the same thing it buys a password login:
// RememberTTL instead of RefreshTTL.
func TestARememberedProviderSignInGetsTheLongerSession(t *testing.T) {
	t.Parallel()

	f := setup(t)

	ordinary, err := f.signIn(t, oauth.SignIn{})
	if err != nil {
		t.Fatal(err)
	}
	remembered, err := f.signIn(t, oauth.SignIn{Remember: true})
	if err != nil {
		t.Fatal(err)
	}

	short := decodeSignIn(t, ordinary).RefreshExpiresAt
	long := decodeSignIn(t, remembered).RefreshExpiresAt
	if !long.After(short) {
		t.Errorf("remembered session ends %s, no later than the ordinary one at %s", long, short)
	}
}
