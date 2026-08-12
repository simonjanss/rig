// Package web is a small server-rendered UI for the todo API.
//
// It exists to make the lifecycle features visible. Soft delete, a version
// history, restore and revert are the parts of rig that are hard to appreciate
// from a curl transcript: they are about what happened to a row over time, and
// time is easier to see in a list you can click.
//
// Nothing here is generated, and nothing here talks to the database. It calls
// the same service the JSON API calls — the interface in internal/api — which
// is the point worth taking from this file: the service layer is the
// application, and HTTP is one way to reach it. A second transport needs no
// second copy of the rules.
//
// It is also deliberately plain. HTMX over server-rendered fragments means
// there is no build step, no client state, and nothing between a click and a
// handler: every interaction below posts a form and swaps in the HTML that
// comes back.
package web

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/examples/todo/internal/api"
	"github.com/simonjanss/rig/examples/todo/internal/model"
	"github.com/simonjanss/rig/runtime/patch"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
)

//go:embed templates/*.gohtml
var files embed.FS

// DemoClaims is who the UI acts as.
//
// This example has no authentication — `rig setup-project` writes that — but
// tenancy is not optional: every generated query is scoped by it, so a caller
// without claims cannot read a row. A fixed tenant is the smallest thing that
// is still honest about how the layers below work.
var DemoClaims = tenancy.Claims{
	TenantID:  uuid.MustParse("00000000-0000-0000-0000-000000000001"),
	AccountID: uuid.MustParse("00000000-0000-0000-0000-0000000000a1"),
	Subject:   tenancy.SubjectAccount,
}

// Handler serves the UI.
type Handler struct {
	svc    api.TodoService
	claims tenancy.Claims
	tpl    *template.Template
}

// New builds the UI over a service.
//
// The claims are fixed, because this example has no authentication — that is
// what `rig setup-project` writes. Everything below still goes through the same
// tenant scoping as a real request: the claims travel in the request envelope
// and in the context, exactly as the generated handlers pass them.
func New(svc api.TodoService, claims tenancy.Claims) (*Handler, error) {
	tpl, err := template.New("").Funcs(helpers()).ParseFS(files, "templates/*.gohtml")
	if err != nil {
		return nil, err
	}
	return &Handler{svc: svc, claims: claims, tpl: tpl}, nil
}

// Mount registers the UI's routes.
//
// They are separate from the API's, under /ui, so that the two never argue
// about a path and it stays obvious which requests are which. The API is still
// the API: this adds a second caller, not a second implementation.
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /", h.page)
	mux.HandleFunc("POST /ui/todos", h.create)
	mux.HandleFunc("POST /ui/todos/{id}/title", h.rename)
	mux.HandleFunc("POST /ui/todos/{id}/complete", h.complete)
	mux.HandleFunc("POST /ui/todos/{id}/delete", h.delete)
	mux.HandleFunc("POST /ui/todos/{id}/restore", h.restore)
	mux.HandleFunc("POST /ui/todos/{id}/revert", h.revert)
	mux.HandleFunc("GET /ui/board", h.board)
}

// view is everything a render needs.
type view struct {
	Live  []*model.Todo
	Trash []*model.Todo

	// Open is the row whose history is showing, if any. Every action carries it
	// along, so acting on a row does not close the panel you were reading.
	Open     *model.Todo
	Timeline []entry

	// TrashOpen is whether the trash is expanded. It is closed to begin with —
	// on most days it is the least interesting part of the page — and carried
	// through every action, so it does not fold itself up while you are working
	// in it.
	TrashOpen bool

	Flash string
	Level string
}

func (h *Handler) page(w http.ResponseWriter, r *http.Request) {
	// One route serves the document and everything under it serves the same
	// fragment, so there is one template and one shape of response.
	h.render(w, "page", h.load(r, ""))
}

func (h *Handler) board(w http.ResponseWriter, r *http.Request) {
	h.render(w, "board", h.load(r, ""))
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	in := model.TodoCreateInput{Title: r.FormValue("title")}
	if p := r.FormValue("priority"); p != "" {
		in.Priority = model.TodoPriority(p)
	}

	_, err := h.svc.Create(h.ctx(r), api.Request[struct{}, struct{}, model.TodoCreateInput]{
		Claims: h.claims,
		Body:   in,
	})
	h.after(w, r, err, "Added.")
}

// rename is what puts a version in the history: every update snapshots the row
// as it was before writing the change.
func (h *Handler) rename(w http.ResponseWriter, r *http.Request) {
	id, ok := h.id(w, r)
	if !ok {
		return
	}

	_, err := h.svc.Update(h.ctx(r), api.Request[api.TodoUpdatePath, struct{}, model.TodoUpdateInput]{
		Claims: h.claims,
		Path:   api.TodoUpdatePath{ID: id},
		Body:   model.TodoUpdateInput{Title: patch.NewOptional(r.FormValue("title"))},
	})
	h.after(w, r, err, "Renamed. The previous title is in the history.")
}

// complete calls the custom endpoint rather than an update, so the rule that
// completing a finished task is a conflict is the one being demonstrated.
func (h *Handler) complete(w http.ResponseWriter, r *http.Request) {
	id, ok := h.id(w, r)
	if !ok {
		return
	}

	_, err := h.svc.Complete(h.ctx(r), api.Request[api.TodoCompletePath, struct{}, api.TodoCompleteBody]{
		Claims: h.claims,
		Path:   api.TodoCompletePath{ID: id},
	})
	h.after(w, r, err, "Done.")
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	id, ok := h.id(w, r)
	if !ok {
		return
	}

	err := h.svc.Delete(h.ctx(r), api.Request[api.TodoDeletePath, struct{}, struct{}]{
		Claims: h.claims,
		Path:   api.TodoDeletePath{ID: id},
	})

	// Expanded whatever it was before, because the whole point of a soft delete
	// is that the row went somewhere rather than away, and a collapsed trash
	// makes a delete look like a disappearance.
	v := h.load(r, "Moved to the trash. The row is still there, stamped.")
	v.TrashOpen = true
	if err != nil {
		v.Flash, v.Level = message(err), "error"
	}
	h.render(w, "board", v)
}

func (h *Handler) restore(w http.ResponseWriter, r *http.Request) {
	id, ok := h.id(w, r)
	if !ok {
		return
	}

	row, err := h.svc.Restore(h.ctx(r), api.Request[api.TodoRestorePath, struct{}, struct{}]{
		Claims: h.claims,
		Path:   api.TodoRestorePath{ID: id},
	})

	// The restore hook renames a row whose title was taken while it was
	// retired, so what comes back is worth reporting rather than assuming.
	message := "Restored."
	if err == nil && row != nil {
		message = "Restored as " + row.Title + "."
	}
	h.after(w, r, err, message)
}

func (h *Handler) revert(w http.ResponseWriter, r *http.Request) {
	id, ok := h.id(w, r)
	if !ok {
		return
	}
	versionID, err := uuid.Parse(r.FormValue("versionId"))
	if err != nil {
		h.after(w, r, rigerr.BadRequest("that is not a version identifier"), "")
		return
	}

	_, err = h.svc.Revert(h.ctx(r), api.Request[api.TodoRevertPath, struct{}, api.TodoRevertBody]{
		Claims: h.claims,
		Path:   api.TodoRevertPath{ID: id},
		Body:   api.TodoRevertBody{VersionID: versionID},
	})
	h.after(w, r, err, "Reverted. The state it replaced is now itself in the history.")
}

// after re-renders the board, with a word about what just happened.
//
// Every action answers with the same fragment, so the page has one target and
// no action has to know what else on it might now be stale.
func (h *Handler) after(w http.ResponseWriter, r *http.Request, err error, done string) {
	v := h.load(r, done)
	if err != nil {
		v.Flash, v.Level = message(err), "error"
	}
	h.render(w, "board", v)
}

// load reads what the board shows: the live rows, the trash, and the history of
// the row whose panel is open.
func (h *Handler) load(r *http.Request, flash string) view {
	v := view{Flash: flash, Level: "ok"}
	ctx := h.ctx(r)

	live, err := h.svc.List(ctx, api.Request[struct{}, api.TodoListQuery, struct{}]{
		Claims: h.claims,
		Query:  api.TodoListQuery{Limit: 100},
	})
	if err != nil {
		v.Flash, v.Level = message(err), "error"
		return v
	}
	v.Live = live.Data

	// The trash is its own endpoint rather than a flag on the list, so nothing
	// can accidentally include deleted rows in an ordinary read.
	trash, err := h.svc.ListDeleted(ctx, api.Request[struct{}, api.TodoListDeletedQuery, struct{}]{
		Claims: h.claims,
		Query:  api.TodoListDeletedQuery{Limit: 100},
	})
	if err != nil {
		v.Flash, v.Level = message(err), "error"
		return v
	}
	v.Trash = trash.Data
	v.TrashOpen = expanded(r)

	openID, err := uuid.Parse(open(r))
	if err != nil {
		return v
	}

	row, err := h.svc.Get(ctx, api.Request[api.TodoGetPath, struct{}, struct{}]{
		Claims: h.claims,
		Path:   api.TodoGetPath{ID: openID},
	})
	if err != nil {
		return v // the row it was showing is gone; the board is still fine
	}
	v.Open = row

	versions, err := h.svc.Versions(ctx, api.Request[api.TodoVersionsPath, struct{}, struct{}]{
		Claims: h.claims,
		Path:   api.TodoVersionsPath{ID: openID},
	})
	if err == nil {
		v.Timeline = timeline(row, versions.Data)
	}
	return v
}

// entry is one point in a row's life.
type entry struct {
	// Row is the state at this point: a version, or the live row for the entry
	// at the top.
	Row     *model.Todo
	Current bool
	// First marks the oldest state, which is the row as it was created. Every
	// update snapshots what it replaced, so the last version is where the row
	// started.
	First bool

	// When this state began — for a version, the moment the row was last updated
	// before the copy was taken, which is the version's identity rather than the
	// time the copy was made.
	When time.Time

	// Undoes is what changed on the way out of this state, so it is exactly what
	// reverting to it would put back. The newest state has nothing to undo.
	Undoes []change
}

// change is one field that differs between two states.
type change struct {
	Field    string
	From, To string
}

// timeline turns the live row and its versions into a history, newest first.
//
// A version is the row as it was before an update, so the states run oldest to
// newest as version[n-1] … version[0] … live, and the interesting part is not
// the states but the steps between them. Each entry carries the step out of it,
// because that is the step reverting to it undoes — which makes the button and
// the words beside it describe the same thing.
func timeline(live *model.Todo, versions []*model.Todo) []entry {
	out := make([]entry, 0, len(versions)+1)

	out = append(out, entry{Row: live, Current: true, When: began(live)})
	for i, v := range versions {
		newer := live
		if i > 0 {
			newer = versions[i-1]
		}
		out = append(out, entry{
			Row:    v,
			First:  i == len(versions)-1,
			When:   began(v),
			Undoes: diff(v, newer),
		})
	}
	return out
}

// began is when a state started: its last update, or its creation when nothing
// has changed it.
//
// For a version this is snapshot_from_todo_at, which rig sets to the original's
// updated_at at copy time — the version of the state captured, not the moment
// the copy was written.
func began(row *model.Todo) time.Time {
	switch {
	case row.SnapshotFromTodoAt != nil:
		return *row.SnapshotFromTodoAt
	case row.UpdatedAt != nil:
		return *row.UpdatedAt
	default:
		return row.CreatedAt
	}
}

// diff is what changed between two states of one row.
//
// Only the fields somebody edits. The audit columns change on every update by
// definition, and reporting that updated_at was updated is noise.
func diff(from, to *model.Todo) []change {
	var out []change
	add := func(field, a, b string) {
		if a != b {
			out = append(out, change{Field: field, From: a, To: b})
		}
	}

	add("title", from.Title, to.Title)
	add("notes", text(from.Notes), text(to.Notes))
	add("priority", string(from.Priority), string(to.Priority))
	add("done", yesno(from.IsDone), yesno(to.IsDone))
	add("due", stamp(from.DueAt), stamp(to.DueAt))
	return out
}

func text(s *string) string {
	if s == nil || *s == "" {
		return "—"
	}
	return *s
}

func yesno(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func stamp(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return t.Local().Format("2 Jan 15:04")
}

// ctx puts the claims where the repository looks for them.
//
// The generated handlers do this before calling the service, and a second
// transport has to do it too: the tenant scope is enforced in the repository,
// from the context, so that no caller can forget it and no caller can override
// it.
func (h *Handler) ctx(r *http.Request) context.Context {
	return tenancy.NewContext(r.Context(), h.claims)
}

func (h *Handler) id(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		h.after(w, r, rigerr.BadRequest("that is not an identifier"), "")
		return uuid.Nil, false
	}
	return id, true
}

func (h *Handler) render(w http.ResponseWriter, name string, v view) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tpl.ExecuteTemplate(w, name, v); err != nil {
		// The status is already written by then, so there is nothing to say to
		// the browser. The server's log is the right place for it.
		http.Error(w, "render", http.StatusInternalServerError)
	}
}

// open is the row whose history the request wants to keep showing. Every form
// carries it, so it survives an action.
func open(r *http.Request) string {
	if v := r.FormValue("open"); v != "" {
		return v
	}
	return r.URL.Query().Get("open")
}

// expanded reports whether the trash should be showing.
//
// Server-side rather than a details element left to the browser: every action
// replaces the whole board, and an element the server did not know was open
// comes back closed. Carrying it costs a hidden field and means the page has one
// source of truth for what it looks like.
func expanded(r *http.Request) bool {
	if v := r.FormValue("trash"); v != "" {
		return v == "1"
	}
	return r.URL.Query().Get("trash") == "1"
}

// message is what to tell a person about a failure.
//
// The error's own message when it has one — those are written for a client, and
// the validators put the useful part there. Anything else is a server fault,
// and a stack of internals is not the user's problem.
func message(err error) string {
	var e *rigerr.Error
	if errors.As(err, &e) && e.Code != rigerr.CodeInternal {
		return e.Message
	}
	return "Something went wrong."
}

func helpers() template.FuncMap {
	return template.FuncMap{
		"when": func(t *time.Time) string {
			if t == nil {
				return ""
			}
			return ago(*t)
		},
		"ago":  ago,
		"full": func(t time.Time) string { return t.Local().Format("Mon 2 Jan 2006, 15:04:05") },
		"priorities": func() []model.TodoPriority {
			return model.AllTodoPriority
		},
	}
}

// ago is a rough distance in words, which is what a history wants: the exact
// moment is in the tooltip, and "4 minutes ago" is the part that means something
// while you are looking at it.
func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < 10*time.Second:
		return "just now"
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s ago"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m ago"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h ago"
	default:
		return t.Local().Format("2 Jan")
	}
}
