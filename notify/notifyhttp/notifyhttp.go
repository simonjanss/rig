// Package notifyhttp is the HTTP shape of an inbox.
//
// It is here rather than in generated code for the reason the auth routes and
// the file routes are: the tables are rig's own and are the same in every
// project, so there is nothing for a generator to vary. Five routes regenerated
// identically into every application would be five routes to keep in step.
//
// Nothing here decides who may do anything. Every handler narrows to the account
// on the claims and cannot be asked to do otherwise: none of these routes takes a
// `scope` parameter, because "read everybody's notifications" is not a thing an
// application means.
//
// A project that wants more than this — the filter grammar, the sort keys, a
// generated client over the same rows — sets `notifications.expose` and gets a
// projected resource as well. Both stay, and the difference between them is the
// point: this is what a project gets without thinking about it.
package notifyhttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/notify"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// DefaultBasePath is where the routes are mounted unless a project says
// otherwise.
const DefaultBasePath = "/notifications"

// Handler serves the inbox.
type Handler struct {
	svc      *notify.Service
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
	// Required, and taken rather than assumed for the reason the generated
	// server takes it: a project authenticates its own way, and an inbox route
	// that established the caller differently from every other route would be a
	// second answer to the one question a tenant boundary rests on. The
	// generated mount passes the server's own.
	Claims func(*http.Request) (tenancy.Claims, error)

	// Fail writes an error response. It is taken rather than assumed so that an
	// inbox route's 404 looks like every other route's 404 in the same
	// application — the generated server has one error shape, and a second one
	// here would be a second thing a client has to understand.
	Fail func(http.ResponseWriter, *http.Request, error)
}

// New builds the handler.
func New(svc *notify.Service, opt Options) *Handler {
	if opt.Claims == nil {
		// Refusing here rather than serving an inbox to nobody: a handler with
		// no way to identify its caller would answer every request with the
		// same empty inbox, which reads as "you have no notifications" rather
		// than as the misconfiguration it is.
		panic("notifyhttp.New: Claims is required")
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

// with establishes the caller and hands the request on.
//
// Every route goes through it, which is what makes "narrows to the caller"
// structural rather than something five handlers each remember.
func (h *Handler) with(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := h.claims(r)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		next(w, r.WithContext(tenancy.NewContext(r.Context(), claims)))
	}
}

// Mount registers the five routes on a mux.
//
//	GET    /notifications                 the caller's inbox, newest first
//	GET    /notifications/_unread-count   the badge, one number
//	POST   /notifications/{id}/_read      mark one read
//	POST   /notifications/_read-all       mark the page's worth read
//	DELETE /notifications/{id}            remove one from the inbox
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET "+h.basePath, h.with(h.list))
	mux.HandleFunc("GET "+h.basePath+"/_unread-count", h.with(h.unreadCount))
	mux.HandleFunc("POST "+h.basePath+"/_read-all", h.with(h.readAll))
	mux.HandleFunc("POST "+h.basePath+"/{id}/_read", h.with(h.read))
	mux.HandleFunc("DELETE "+h.basePath+"/{id}", h.with(h.dismiss))
}

// Line is one inbox row on the wire.
//
// It carries the subject's identifier and not the subject's row, and that is a
// decision rather than an omission. A client doing live sync already has the
// post and wants the identifier; embedding would be a second query per page and
// two joins deep — through the notification — on the hottest read in the system.
// A client that wants the rows turns `notifications.expose` on and gets a
// resource with `embed` on its relations.
type Line struct {
	ID             uuid.UUID `json:"id"`
	NotificationID uuid.UUID `json:"notificationId"`
	Kind           string    `json:"kind"`
	// EventCount is how many events this line stands for: ten comments on one
	// post are one line saying ten.
	EventCount int        `json:"eventCount"`
	ReadAt     *time.Time `json:"readAt"`
	CreatedAt  time.Time  `json:"createdAt"`
}

func lineOf(r *notify.Recipient) Line {
	return Line{
		ID:             r.ID,
		NotificationID: r.NotificationID,
		Kind:           r.Kind,
		EventCount:     r.EventCount,
		ReadAt:         r.ReadAt,
		CreatedAt:      r.CreatedAt,
	}
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	q, err := query(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	rows, err := h.svc.Inbox(r.Context(), q)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	lines := make([]Line, 0, len(rows))
	for _, row := range rows {
		lines = append(lines, lineOf(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": lines})
}

func (h *Handler) unreadCount(w http.ResponseWriter, r *http.Request) {
	n, err := h.svc.UnreadCount(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"unread": n})
}

func (h *Handler) read(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.svc.MarkRead(r.Context(), id); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// readAll takes the same filter the list took.
//
// "Mark all read" on a filtered inbox that silently cleared the unfiltered one
// is the interaction people complain about, which is why this route reads a
// query string at all.
func (h *Handler) readAll(w http.ResponseWriter, r *http.Request) {
	q, err := query(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	n, err := h.svc.MarkAllRead(r.Context(), q)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"read": n})
}

func (h *Handler) dismiss(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.svc.Dismiss(r.Context(), id); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func query(r *http.Request) (notify.InboxQuery, error) {
	var q notify.InboxQuery
	values := r.URL.Query()

	q.UnreadOnly = values.Get("unread") == "true"
	q.Kind = values.Get("kind")

	if raw := values.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return q, rigerr.BadRequest("limit is not a number: %q", raw)
		}
		q.Limit = n
	}
	if raw := values.Get("before"); raw != "" {
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return q, rigerr.BadRequest("before is not an RFC 3339 timestamp: %q", raw)
		}
		q.Before = &at
	}
	return q, nil
}

func pathID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return uuid.Nil, rigerr.BadRequest("the identifier in the path is not a valid one")
	}
	return id, nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError is the fallback for a project that did not supply one.
//
// It is deliberately plain. A project that has a generated server passes that
// server's error writer instead, so an inbox route's failure looks like every
// other route's; this exists so the handler is usable on its own.
func writeError(w http.ResponseWriter, _ *http.Request, err error) {
	status := rigerr.StatusOf(err)
	if errors.Is(err, notify.ErrNotFound) {
		status = http.StatusNotFound
	}
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"code":    string(rigerr.CodeOf(err)),
		"message": err.Error(),
	}})
}
