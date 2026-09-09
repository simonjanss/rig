package oauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/simonjanss/rig/auth/authlog"
	"github.com/simonjanss/rig/runtime/rigerr"
)

// stateCookie is the name of the cookie that carries a sign-in across the
// redirect. The __Host- prefix is a browser-enforced promise: the cookie is
// secure, path-scoped to /, and cannot have been set by a subdomain.
const stateCookie = "__Host-rig_oauth"

// insecureStateCookie is the same cookie without the prefix, for development
// over plain HTTP where a browser would refuse the real one.
const insecureStateCookie = "rig_oauth"

// pending is what has to survive the round trip to the provider.
type pending struct {
	State    string `json:"s"`
	Verifier string `json:"v"`
	Provider string `json:"p"`
	ReturnTo string `json:"r,omitempty"`
	// Tenant is which tenant the sign-in is for, decided at the start and carried
	// rather than resolved again — or the nil UUID when nothing named one, which
	// leaves the question to be settled after the callback.
	//
	// The callback URL is registered with the provider and fixed, so it cannot
	// carry anything: whatever the application's resolver reads from a request —
	// a header, a query parameter — is not there when the provider sends the
	// browser back. Only a host survives, which is why a subdomain deployment
	// never noticed and why a deployment with nothing but a header has no answer
	// to give at the start at all. Carrying it in the sealed cookie is what makes
	// the first kind work, and it also stops a callback being replayed against a
	// different tenant than the one it started for.
	//
	// Written unconditionally, nil UUID and all, and with no omitempty: the
	// field is the wire format of a cookie that outlives a deploy by up to
	// StateTTL, and a cookie sealed by an older binary has to keep opening on a
	// newer one. [Handler.callback] reads an empty string as nil for the same
	// reason, from the other side.
	Tenant  string `json:"t"`
	Expires int64  `json:"e"`
}

// start sends somebody to the provider.
func (h *Handler) start(w http.ResponseWriter, r *http.Request) {
	p, err := h.provider(r)
	if err != nil {
		h.fail(w, r, &Failure{Reason: ReasonUnknownProvider, Err: err})
		return
	}
	// Lowercased once, because that is how the sealed cookie and every failure
	// below name a provider, and the route's own spelling is the caller's.
	name := strings.ToLower(p.Name)

	// Nil means the application does not know yet, which is an ordinary answer
	// rather than a missing one — see [Config.Tenant].
	var tenantID uuid.UUID
	if h.cfg.Tenant != nil {
		id, err := h.cfg.Tenant(r)
		if err != nil {
			// The application wrote this resolver, so it is the one failure here
			// whose cause is not rig's — and the only reason it gets its own.
			f := failure(err, ReasonTenant)
			f.Provider = name
			h.fail(w, r, f)
			return
		}
		tenantID = id
	}

	returnTo, err := h.checkReturnTo(r.URL.Query().Get("returnTo"))
	if err != nil {
		h.fail(w, r, &Failure{Reason: ReasonReturnTo, Provider: name, Err: err})
		return
	}

	state, err := randomString()
	if err != nil {
		h.fail(w, r, &Failure{
			Reason: ReasonInternal, Provider: name, TenantID: tenantID,
			Err: rigerr.Internal(err, "generate state"),
		})
		return
	}
	verifier := oauth2.GenerateVerifier()

	value, err := h.seal(pending{
		State: state, Verifier: verifier, Provider: name,
		ReturnTo: returnTo, Tenant: tenantID.String(),
		Expires: h.now().Add(h.cfg.StateTTL).Unix(),
	})
	if err != nil {
		h.fail(w, r, &Failure{
			Reason: ReasonInternal, Provider: name, TenantID: tenantID,
			ReturnTo: returnTo, Err: rigerr.Internal(err, "seal state"),
		})
		return
	}
	http.SetCookie(w, h.cookie(value, h.cfg.StateTTL))

	cfg := p.config(h.redirectURI(r, p))
	// PKCE on a confidential client is belt and braces, and it is free: it
	// makes a stolen authorization code useless without the verifier, which
	// never left this server.
	http.Redirect(w, r, cfg.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.S256ChallengeOption(verifier),
	), http.StatusFound)
}

// callback finishes a sign-in.
func (h *Handler) callback(w http.ResponseWriter, r *http.Request) {
	// Clear the cookie whatever happens next: it is single-use, and one left
	// behind is a state somebody else could replay. Above the provider lookup
	// rather than below it, because "whatever happens next" includes a callback
	// naming a provider this deployment does not have.
	http.SetCookie(w, h.cookie("", -time.Second))

	p, err := h.provider(r)
	if err != nil {
		h.fail(w, r, &Failure{Reason: ReasonUnknownProvider, Err: err})
		return
	}
	name := strings.ToLower(p.Name)

	query := r.URL.Query()
	if reported := query.Get("error"); reported != "" {
		// Somebody pressed cancel, or the provider refused. Neither is a
		// server failure, and neither should look like one.
		//
		// access_denied is the cancel button and the provider sends it for
		// nothing else, so it is the one value worth reading — matched exactly,
		// and answered with a reason of rig's own rather than by passing the
		// provider's word for it on to anybody.
		//
		// No tenant on the failure: it is in the cookie, and a refusal arrives
		// before there is any reason to trust what came back.
		reason := ReasonProviderRefused
		if reported == "access_denied" {
			reason = ReasonCancelled
		}
		h.fail(w, r, &Failure{
			Reason: reason, Provider: name, ProviderError: reported,
			Err: rigerr.BadRequest("%s did not complete the sign-in: %s", p.Name, reported),
		})
		return
	}

	state, err := h.open(r)
	if err != nil {
		h.fail(w, r, &Failure{Reason: ReasonState, Provider: name, Err: err})
		return
	}
	// From the cookie, not from the request. The cookie is signed, so this is the
	// tenant the sign-in actually started for and nobody can have changed it —
	// and nil is a tenant this sign-in never had, which resolve settles later.
	var tenantID uuid.UUID
	if state.Tenant != "" {
		id, err := uuid.Parse(state.Tenant)
		if err != nil {
			h.fail(w, r, &Failure{
				Reason: ReasonState, Provider: name, ReturnTo: state.ReturnTo,
				Err: rigerr.BadRequest("this sign-in did not start here"),
			})
			return
		}
		tenantID = id
	}
	// The double submit. A state that came back without a matching cookie is
	// somebody else's sign-in being finished in this browser.
	if !hmac.Equal([]byte(state.State), []byte(query.Get("state"))) {
		h.fail(w, r, &Failure{
			Reason: ReasonState, Provider: name, TenantID: tenantID, ReturnTo: state.ReturnTo,
			Err: rigerr.BadRequest("this sign-in did not start here"),
		})
		return
	}
	if state.Provider != name {
		h.fail(w, r, &Failure{
			Reason: ReasonState, Provider: name, TenantID: tenantID, ReturnTo: state.ReturnTo,
			Err: rigerr.BadRequest("this sign-in started with a different provider"),
		})
		return
	}

	code := query.Get("code")
	if code == "" {
		h.fail(w, r, &Failure{
			Reason: ReasonNoCode, Provider: name, TenantID: tenantID, ReturnTo: state.ReturnTo,
			Err: rigerr.BadRequest("%s returned no authorization code", p.Name),
		})
		return
	}

	cfg := p.config(h.redirectURI(r, p))
	token, err := cfg.Exchange(r.Context(), code, oauth2.VerifierOption(state.Verifier))
	if err != nil {
		// Wrapped rather than dropped. What the person is told is unchanged —
		// the provider's own words here are an OAuth error document nobody can
		// act on — but a wrong client secret refuses every sign-in identically,
		// and without the cause underneath there is nothing anywhere to say so.
		h.fail(w, r, &Failure{
			Reason: ReasonExchange, Provider: name, TenantID: tenantID, ReturnTo: state.ReturnTo,
			Err: rigerr.BadRequest("%s refused the authorization code", p.Name).Wrap(err),
		})
		return
	}

	profile, err := p.fetch(r.Context(), cfg, token)
	if err != nil {
		f := failure(err, ReasonProfile)
		f.Provider, f.TenantID, f.ReturnTo = name, tenantID, state.ReturnTo
		h.fail(w, r, f)
		return
	}

	in, err := h.resolve(r.Context(), tenantID, p, profile)
	if err != nil {
		// resolve says which refusal this was, because it is the only place that
		// knows: from out here the four of them are four sentences, and telling
		// them apart by reading one is a test that passes until somebody rewords
		// it. A Store refusing is not one of the four, and is internal.
		f := failure(err, ReasonInternal)
		f.Provider, f.TenantID, f.ReturnTo = name, tenantID, state.ReturnTo
		f.EmailAddress = profile.EmailAddress
		h.fail(w, r, f)
		return
	}

	// Written before OnSignIn rather than after, so a sign-in whose last step
	// fails still records that a provider authenticated somebody. The cost is
	// that a refusal inside OnSignIn — a tenant they turn out not to belong to —
	// reads as OAuthSignIn/Succeeded followed by LoginFailed/Failed. Those two
	// are not in conflict: the first says Google answered, the second says a
	// session was not issued.
	done := authlog.Entry{
		Event: authlog.EventOAuthSignIn, Outcome: authlog.Succeeded,
		EmailAddress: strings.ToLower(profile.EmailAddress),
		IPAddress:    remoteAddr(r), UserAgent: r.UserAgent(),
		Detail: map[string]any{
			"provider": p.Name, "provisioned": in.New, "new_identity": in.NewIdentity,
		},
	}
	if in.TenantID != uuid.Nil {
		done.TenantID, done.AccountID = &in.TenantID, &in.AccountID
	}
	h.write(r.Context(), done)

	in.ReturnTo = state.ReturnTo
	if err := h.cfg.OnSignIn(w, r, in); err != nil {
		// The Succeeded entry above stays beside the Failed one this writes, and
		// the two are not in conflict: the first says the provider answered, the
		// second says a session was not issued.
		f := failure(err, ReasonEnding)
		f.Provider, f.TenantID, f.ReturnTo = name, tenantID, state.ReturnTo
		f.EmailAddress = profile.EmailAddress
		h.fail(w, r, f)
	}
}

// checkReturnTo bounds where a sign-in may send somebody afterwards.
//
// An unchecked returnTo is an open redirect, and an open redirect on a sign-in
// endpoint is how a phishing link gets to wear your domain.
func (h *Handler) checkReturnTo(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", rigerr.BadRequest("returnTo is not a valid URL")
	}
	// A relative path on this origin is always fine, and is what an
	// application uses ninety per cent of the time.
	if u.Scheme == "" && u.Host == "" && strings.HasPrefix(u.Path, "/") &&
		!strings.HasPrefix(u.Path, "//") {
		return u.String(), nil
	}
	if slices.Contains(h.cfg.AllowedReturnTo, u.Scheme+"://"+u.Host) {
		return u.String(), nil
	}
	return "", rigerr.BadRequest("returnTo is not an allowed destination")
}

func (h *Handler) cookieName() string {
	if h.cfg.Insecure {
		return insecureStateCookie
	}
	return stateCookie
}

func (h *Handler) cookie(value string, maxAge time.Duration) *http.Cookie {
	return &http.Cookie{
		Name:  h.cookieName(),
		Value: value,
		Path:  "/",
		// The provider redirects back with a top-level GET, which Lax allows
		// and Strict would drop — turning every sign-in into a dead end.
		SameSite: http.SameSiteLaxMode,
		HttpOnly: true,
		Secure:   !h.cfg.Insecure,
		MaxAge:   int(maxAge.Seconds()),
	}
}

// seal encodes and signs the pending sign-in.
func (h *Handler) seal(p pending) (string, error) {
	body, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(body)
	return encoded + "." + h.sign(encoded), nil
}

// open reads the cookie back.
func (h *Handler) open(r *http.Request) (pending, error) {
	// One message for every way this can fail, on purpose: which check refused is
	// not a client's business. It does not claim expiry, because the commonest
	// cause is a cookie that was never sent — a callback delivered to a host other
	// than the one the sign-in started at, which a deployment with a tenant per
	// subdomain will meet on its first day.
	invalid := rigerr.BadRequest("this sign-in did not start here, or has expired; start again")

	c, err := r.Cookie(h.cookieName())
	if err != nil || c.Value == "" {
		return pending{}, invalid
	}

	encoded, signature, found := strings.Cut(c.Value, ".")
	if !found {
		return pending{}, invalid
	}
	// Constant time, because the signature is the only thing stopping somebody
	// planting a state of their own.
	if !hmac.Equal([]byte(signature), []byte(h.sign(encoded))) {
		return pending{}, invalid
	}

	body, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return pending{}, invalid
	}

	var p pending
	if err := json.Unmarshal(body, &p); err != nil {
		return pending{}, invalid
	}
	if h.now().Unix() > p.Expires {
		return pending{}, invalid
	}
	return p, nil
}

func (h *Handler) sign(s string) string {
	mac := hmac.New(sha256.New, h.cfg.SigningKey)
	mac.Write([]byte(s))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func randomString() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("oauth: read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// remoteAddr is the peer's address, without a port.
//
// No forwarded header is read: this is written to the log, and a log that
// records whatever a client claimed is a log that cannot be used as evidence.
func remoteAddr(r *http.Request) string {
	host, _, found := strings.Cut(r.RemoteAddr, ":")
	if !found {
		return r.RemoteAddr
	}
	return host
}
