package main

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/examples/linearlite/services/outbox"
	"github.com/simonjanss/rig/notify"
	"github.com/simonjanss/rig/observe"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// The demonstration's own two routes, and the only hand-written HTTP in this
// example.
//
// They are here rather than in the schema because neither is about a table.
// The outbox is a ring buffer in this process's memory — it has no rows, no
// tenant column and nothing to filter — and the tour is a question about how
// the binary was started. A resource for either would be a resource that lies
// about what it is, and rig would generate a client, a filter grammar and an
// OpenAPI entry for something that vanishes on restart.
//
// `/_demo/` is a prefix nothing else claims: rig owns `/_rig/`, the API owns
// `/api/v1`, and the front end owns everything left over.
const demoPrefix = "/_demo/"

// registerDemo mounts them. getClaims is the same lookup every generated
// handler uses, so a signed-out browser gets a 401 here for the same reason it
// gets one from the board.
func registerDemo(mux *http.ServeMux, mail *outbox.Box, page *observe.Page,
	getClaims func(*http.Request) (tenancy.Claims, error),
	senders map[notify.Channel]notify.Sender,
) {
	// What the tour can offer, so the front end can leave out a link that would
	// only 404. The monitoring page is not mounted without a password in the
	// environment, and that is the ordinary case on a laptop.
	//
	// Signed in, like the outbox below, and for a reason worth saying out loud:
	// rig mounts that page as no route at all rather than one that answers 401,
	// so that a scan cannot learn there is a page there. An anonymous endpoint
	// answering "monitor": true would hand back exactly the fact rig went out
	// of its way not to leak. Nothing is lost — the nav that reads this is only
	// rendered for a session anyway.
	mux.HandleFunc("GET "+demoPrefix+"tour", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := tenantOf(r, getClaims); !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "sign in first"})
			return
		}
		// Which channels have a sender, because the preferences screen is a
		// screen about channels and one of them cannot work here. A channel
		// with nothing registered has no delivery rows written for it at all —
		// the right answer, and an invisible one without this.
		channels := make([]string, 0, len(senders))
		for _, c := range notify.Channels() {
			if _, ok := senders[c]; ok {
				channels = append(channels, string(c))
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"monitor":  page != nil && page.Unarmed() == "",
			"outbox":   mail != nil,
			"channels": channels,
		})
	})

	if mail == nil {
		return
	}

	// The mail nobody sent. Signed in, and scoped to the caller's tenant where
	// the message knows one — see outbox.Box.For for what that does not cover
	// and why a real application has no such screen at all.
	mux.HandleFunc("GET "+demoPrefix+"outbox", func(w http.ResponseWriter, r *http.Request) {
		tenantID, ok := tenantOf(r, getClaims)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "sign in first"})
			return
		}
		messages := mail.For(tenantID)
		if messages == nil {
			messages = []outbox.Message{}
		}
		writeJSON(w, http.StatusOK, messages)
	})
}

// tenantOf is the caller's tenant, and false for anybody these routes should
// not answer at all.
func tenantOf(r *http.Request, getClaims func(*http.Request) (tenancy.Claims, error)) (uuid.UUID, bool) {
	claims, err := getClaims(r)
	if err != nil || claims.TenantID == uuid.Nil {
		return uuid.Nil, false
	}
	return claims.TenantID, true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Nothing here is cacheable: one is a credential and the other is a fact
	// about this process.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
