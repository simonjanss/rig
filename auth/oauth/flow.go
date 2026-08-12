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
	// rather than resolved again.
	//
	// The callback URL is registered with the provider and fixed, so it cannot
	// carry anything: whatever the application's resolver reads from a request —
	// a header, a query parameter — is not there when the provider sends the
	// browser back. Only a host survives, which is why a subdomain deployment
	// never noticed. Carrying it in the sealed cookie means every deployment
	// works, and it also stops a callback being replayed against a different
	// tenant than the one it started for.
	Tenant  string `json:"t"`
	Expires int64  `json:"e"`
}

// start sends somebody to the provider.
func (h *Handler) start(w http.ResponseWriter, r *http.Request) {
	p, err := h.provider(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	tenantID, err := h.cfg.Tenant(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	returnTo, err := h.checkReturnTo(r.URL.Query().Get("returnTo"))
	if err != nil {
		h.fail(w, r, err)
		return
	}

	state, err := randomString()
	if err != nil {
		h.fail(w, r, rigerr.Internal(err, "generate state"))
		return
	}
	verifier := oauth2.GenerateVerifier()

	value, err := h.seal(pending{
		State: state, Verifier: verifier, Provider: strings.ToLower(p.Name),
		ReturnTo: returnTo, Tenant: tenantID.String(),
		Expires: h.now().Add(h.cfg.StateTTL).Unix(),
	})
	if err != nil {
		h.fail(w, r, rigerr.Internal(err, "seal state"))
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
	p, err := h.provider(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// Clear the cookie whatever happens next: it is single-use, and one left
	// behind is a state somebody else could replay.
	http.SetCookie(w, h.cookie("", -time.Second))

	query := r.URL.Query()
	if reason := query.Get("error"); reason != "" {
		// Somebody pressed cancel, or the provider refused. Neither is a
		// server failure, and neither should look like one.
		//
		// No tenant on the entry: it is in the cookie, and a refusal arrives
		// before there is any reason to trust what came back.
		h.write(r.Context(), authlog.Entry{
			Event: authlog.EventOAuthSignIn, Outcome: authlog.Failed,
			Detail: map[string]any{"provider": p.Name, "reason": reason},
		})
		h.fail(w, r, rigerr.BadRequest("%s did not complete the sign-in: %s", p.Name, reason))
		return
	}

	state, err := h.open(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// From the cookie, not from the request. The cookie is signed, so this is the
	// tenant the sign-in actually started for and nobody can have changed it.
	tenantID, err := uuid.Parse(state.Tenant)
	if err != nil {
		h.fail(w, r, rigerr.BadRequest("this sign-in did not start here"))
		return
	}
	// The double submit. A state that came back without a matching cookie is
	// somebody else's sign-in being finished in this browser.
	if !hmac.Equal([]byte(state.State), []byte(query.Get("state"))) {
		h.fail(w, r, rigerr.BadRequest("this sign-in did not start here"))
		return
	}
	if state.Provider != strings.ToLower(p.Name) {
		h.fail(w, r, rigerr.BadRequest("this sign-in started with a different provider"))
		return
	}

	code := query.Get("code")
	if code == "" {
		h.fail(w, r, rigerr.BadRequest("%s returned no authorization code", p.Name))
		return
	}

	cfg := p.config(h.redirectURI(r, p))
	token, err := cfg.Exchange(r.Context(), code, oauth2.VerifierOption(state.Verifier))
	if err != nil {
		h.fail(w, r, rigerr.BadRequest("%s refused the authorization code", p.Name))
		return
	}

	profile, err := p.fetch(r.Context(), cfg, token)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	in, err := h.resolve(r.Context(), tenantID, p, profile)
	if err != nil {
		h.write(r.Context(), authlog.Entry{
			Event: authlog.EventOAuthSignIn, Outcome: authlog.Failed,
			TenantID: &tenantID, EmailAddress: strings.ToLower(profile.EmailAddress),
			IPAddress: remoteAddr(r), UserAgent: r.UserAgent(),
			Detail: map[string]any{"provider": p.Name, "reason": err.Error()},
		})
		h.fail(w, r, err)
		return
	}

	h.write(r.Context(), authlog.Entry{
		Event: authlog.EventOAuthSignIn, Outcome: authlog.Succeeded,
		TenantID: &in.TenantID, AccountID: &in.AccountID,
		EmailAddress: strings.ToLower(profile.EmailAddress),
		IPAddress:    remoteAddr(r), UserAgent: r.UserAgent(),
		Detail: map[string]any{"provider": p.Name, "provisioned": in.New},
	})

	in.ReturnTo = state.ReturnTo
	if err := h.cfg.OnSignIn(w, r, in); err != nil {
		h.fail(w, r, err)
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
