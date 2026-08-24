// Package presencehttp is the HTTP shape of presence.
//
// It is here rather than in generated code for the reason the auth routes, the
// file routes and the inbox routes are: the table is rig's own and is the same
// in every project, so there is nothing for a generator to vary. Three routes
// regenerated identically into every application would be three routes to keep
// in step.
//
// # Nothing here checks a permission, and that is deliberate
//
// Presence is not privileged: everybody who may look at a board may say that
// they are looking at it. A valid session is the whole requirement.
//
// It is worth stating because the alternative has a trap in it. A custom
// endpoint's *derived* permission key is its own method name lowercased, and the
// role policies rig's examples ship hand an ordinary member only the keys ending
// `.read` and `.write` — so a generated `presence.heartbeat` would silently be an
// administrator's permission, and the failure would be a 403 for every ordinary
// member of a feature that worked perfectly for whoever wrote it. Hand-written
// routes never reach that derivation.
//
// # There is nowhere to name somebody else
//
// [Beat] has no account field and neither does the leave. Who is present is read
// from the credential, so "you may only write your own presence" is not a rule a
// handler enforces — it is a sentence a client cannot phrase. Somebody looking
// here for the authorization check will not find one, and this is why.
package presencehttp

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/presence"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// DefaultBasePath is where the routes are mounted unless a project says
// otherwise.
//
// Outside the API's own base path, like `/auth` and `/notifications`: these
// routes are rig's, they are not versioned with the project's resources, and
// nothing about them changes when `api.version` does.
const DefaultBasePath = "/presence"

// Handler serves presence.
type Handler struct {
	svc      *presence.Service
	basePath string
	claims   func(*http.Request) (tenancy.Claims, error)
	fail     func(http.ResponseWriter, *http.Request, error)
}

// Options configure the handler.
type Options struct {
	// BasePath is the prefix the routes sit under. Empty means
	// [DefaultBasePath].
	BasePath string

	// Claims identifies the caller.
	//
	// Required, and taken rather than assumed for the reason the generated server
	// takes it: a project authenticates its own way, and a presence route that
	// established the caller differently from every other route would be a second
	// answer to the one question a tenant boundary rests on.
	Claims func(*http.Request) (tenancy.Claims, error)

	// Fail writes an error response. Taken rather than assumed so that a
	// presence route's refusal looks like every other route's in the same
	// application.
	Fail func(http.ResponseWriter, *http.Request, error)
}

// New builds the handler.
func New(svc *presence.Service, opt Options) *Handler {
	if svc == nil {
		panic("presencehttp.New: a service is required")
	}
	if opt.Claims == nil {
		// Refusing here rather than serving presence to nobody. A handler with no
		// way to identify its caller would write every tab in the building into
		// one row, which reads as "presence is flickering" rather than as the
		// misconfiguration it is.
		panic("presencehttp.New: Claims is required")
	}
	h := &Handler{svc: svc, basePath: opt.BasePath, claims: opt.Claims, fail: opt.Fail}
	if h.basePath == "" {
		h.basePath = DefaultBasePath
	}
	if h.fail == nil {
		h.fail = writeError
	}
	return h
}

// Mount registers the three routes on a mux.
//
//	PUT    /presence   one heartbeat: where this tab is, and that it is still there
//	DELETE /presence   leave now rather than waiting out the TTL
//	GET    /presence   who is here, for a client that is not streaming
//
// It takes a mux rather than making one, because these routes belong on the same
// server as the rest of the API.
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.HandleFunc("PUT "+h.basePath, h.with(h.beat))
	mux.HandleFunc("DELETE "+h.basePath, h.with(h.leave))
	mux.HandleFunc("GET "+h.basePath, h.with(h.here))
}

// with establishes the caller and hands the request on.
func (h *Handler) with(next func(http.ResponseWriter, *http.Request, tenancy.Claims)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := h.claims(r)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		next(w, r.WithContext(tenancy.NewContext(r.Context(), claims)), claims)
	}
}

// Beat is what a browser sends on every heartbeat.
//
// The keys are camelCase whatever `api.json_case` says, and that is deliberate
// rather than an oversight. These routes are rig's, identical in every project,
// and the browser package is compiled against them once — a wire shape that
// changed with a project's naming configuration would be one that package could
// not parse, so it would have to take the casing as a parameter and every project
// would have to tell it.
//
// There is no account here and nowhere to put one. See the package
// documentation.
type Beat struct {
	// SessionKey is this tab's name for itself, stable for as long as it is open.
	SessionKey string `json:"sessionKey"`
	// Scope is which part of the application this is — a board, a document.
	Scope string `json:"scope"`
	// TargetTable is the table of the row being looked at. Absent is the scope
	// itself rather than a row in it.
	TargetTable string `json:"targetTable,omitempty"`
	// TargetID is which row. Absent with a table present is a list of them.
	TargetID *uuid.UUID `json:"targetId,omitempty"`
	// TargetField is which control has focus. Absent is looking rather than
	// typing.
	TargetField string `json:"targetField,omitempty"`
	// Activity is whether they are looking or typing. Absent reads as viewing.
	Activity string `json:"activity,omitempty"`
}

// Beaten is the answer to a heartbeat.
//
// Two of its four fields are the reason it is worth reading. `seenAt` is this
// server's clock at the moment of the write, which is the only reading of it a
// browser gets and therefore the anchor any client-side freshness test needs.
// `ttlSeconds` and `heartbeatSeconds` come back on every beat rather than from a
// configuration endpoint, because a browser is already listening here — so
// changing either is a deploy of the server and not a release of the front end,
// and a client built when the TTL was sixty seconds picks up twenty on its next
// beat.
type Beaten struct {
	ID               uuid.UUID `json:"id"`
	SeenAt           time.Time `json:"seenAt"`
	TTLSeconds       int       `json:"ttlSeconds"`
	HeartbeatSeconds int       `json:"heartbeatSeconds"`
}

// Person is one presence on the wire.
//
// It carries the target's table and identifier and not the target's row. A client
// subscribed to the same shapes already has the row and wants the identifier;
// embedding would be a join per person on the most frequently changing read in
// the application.
type Person struct {
	ID          uuid.UUID  `json:"id"`
	AccountID   uuid.UUID  `json:"accountId"`
	SessionKey  string     `json:"sessionKey"`
	Scope       string     `json:"scope"`
	TargetTable string     `json:"targetTable,omitempty"`
	TargetID    *uuid.UUID `json:"targetId,omitempty"`
	TargetField string     `json:"targetField,omitempty"`
	Activity    string     `json:"activity"`
	CreatedAt   time.Time  `json:"createdAt"`
	SeenAt      time.Time  `json:"seenAt"`
}

func (h *Handler) beat(w http.ResponseWriter, r *http.Request, claims tenancy.Claims) {
	var body Beat
	if err := decode(r, &body); err != nil {
		h.fail(w, r, err)
		return
	}

	activity, err := presence.ParseActivity(body.Activity)
	if err != nil {
		h.fail(w, r, rigerr.Invalid("%s", err))
		return
	}

	p, err := h.svc.Beat(r.Context(), claims, presence.Beat{
		SessionKey: body.SessionKey,
		Scope:      body.Scope,
		Target: presence.Target{
			Table: body.TargetTable,
			ID:    derefUUID(body.TargetID),
			Field: body.TargetField,
		},
		Activity: activity,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, Beaten{
		ID:               p.ID,
		SeenAt:           p.SeenAt,
		TTLSeconds:       int(h.svc.TTL().Seconds()),
		HeartbeatSeconds: int(h.svc.Heartbeat().Seconds()),
	})
}

// leaveBody is the leave's one field.
//
// A body rather than a path segment because the leave is sent from `pagehide`
// with `fetch({keepalive: true})`, which already has one — and it keeps one more
// opaque identifier out of every access log between here and the browser.
type leaveBody struct {
	SessionKey string `json:"sessionKey"`
}

func (h *Handler) leave(w http.ResponseWriter, r *http.Request, claims tenancy.Claims) {
	var body leaveBody
	if err := decode(r, &body); err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.svc.Leave(r.Context(), claims, body.SessionKey); err != nil {
		h.fail(w, r, err)
		return
	}
	// 204 for a session that was already gone as well as one that was here. A
	// retry of a request whose answer was lost is the ordinary case on this route,
	// and it should not look like a failure.
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) here(w http.ResponseWriter, r *http.Request, claims tenancy.Claims) {
	values := r.URL.Query()

	var q presence.Query
	q.Scope = values.Get("scope")
	q.Target.Table = values.Get("targetTable")
	q.Target.Field = values.Get("targetField")
	if raw := values.Get("targetId"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			h.fail(w, r, rigerr.BadRequest("targetId is not a valid identifier"))
			return
		}
		q.Target.ID = id
	}

	rows, err := h.svc.Here(r.Context(), claims, q)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	people := make([]Person, 0, len(rows))
	for _, p := range rows {
		people = append(people, personOf(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data":             people,
		"ttlSeconds":       int(h.svc.TTL().Seconds()),
		"heartbeatSeconds": int(h.svc.Heartbeat().Seconds()),
	})
}

func personOf(p *presence.Presence) Person {
	return Person{
		ID:          p.ID,
		AccountID:   p.AccountID,
		SessionKey:  p.SessionKey,
		Scope:       p.Scope,
		TargetTable: p.Target.Table,
		TargetID:    nilUUID(p.Target.ID),
		TargetField: p.Target.Field,
		Activity:    string(p.Activity),
		CreatedAt:   p.CreatedAt,
		SeenAt:      p.SeenAt,
	}
}

// decode reads a JSON body, refusing a field nothing here declares.
//
// DisallowUnknownFields for the reason the generated decoder uses it: a client
// that sent `accountId` believing it meant something should be told, not
// silently ignored — the field it was reaching for is the one this package
// exists to make unreachable.
func decode(r *http.Request, into any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return rigerr.BadRequest("the request body is not the shape this route takes: %s", err)
	}
	return nil
}

func derefUUID(p *uuid.UUID) uuid.UUID {
	if p == nil {
		return uuid.Nil
	}
	return *p
}

func nilUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError is the fallback for a project that did not supply one.
//
// Deliberately plain. A project with a generated server passes that server's
// error writer instead, so a presence route's failure looks like every other
// route's; this exists so the handler is usable on its own.
func writeError(w http.ResponseWriter, _ *http.Request, err error) {
	writeJSON(w, rigerr.StatusOf(err), map[string]any{"error": map[string]any{
		"code":    string(rigerr.CodeOf(err)),
		"message": err.Error(),
	}})
}
